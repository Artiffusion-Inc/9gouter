package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// grokCliResolver fetches the live model catalog from grok.com's CLI proxy
// /models endpoint for an authenticated grok-cli connection. Mirrors
// open-sse/services/grokCliModels.js. The grok-cli auth token is an xAI OAuth
// access token; on a 401/403 the resolver refreshes via the XaiRefresher and
// retries once (proxy-aware through opts.ProxyOptions — reserved).
type grokCliResolver struct {
	cache     *Cache
	refresher TokenRefresher // *tokenrefresh.XaiRefresher
	client    *http.Client
	baseURL   string
	version   string
	clientID  string
	userAgent string
}

const (
	grokCliBaseURL   = "https://cli-chat-proxy.grok.com/v1"
	grokCliVersion   = "0.2.99"
	grokCliClientID  = "grok-shell"
	grokCliUserAgent = "grok-shell/0.2.99 (linux; x86_64)"
	grokCliFetchTimeout = 20 * time.Second
	grokCliCacheTTL     = 5 * time.Minute
)

// NewGrokCliResolver builds a grok-cli live resolver. refresher is the xAI
// token refresher (tokenrefresh.NewXaiRefresher); pass nil to disable refresh.
func NewGrokCliResolver(cache *Cache, refresher TokenRefresher) LiveModelResolver {
	if cache == nil {
		cache = NewCache(grokCliCacheTTL)
	}
	return &grokCliResolver{
		cache:     cache,
		refresher: refresher,
		client:    &http.Client{Timeout: grokCliFetchTimeout},
		baseURL:   grokCliBaseURL,
		version:   grokCliVersion,
		clientID:  grokCliClientID,
		userAgent: grokCliUserAgent,
	}
}

func (r *grokCliResolver) ProviderID() string { return "grok-cli" }

func (r *grokCliResolver) Resolve(ctx context.Context, creds provider.Credentials, opts ResolveOpts) (*Result, error) {
	log := loggerOr(opts.Logger)
	if creds.AccessToken == "" {
		log.Debug("GROK_CLI_MODELS: no accessToken; skipping live fetch")
		return nil, nil
	}
	key := grokCliCacheKey(creds)
	if cached, ok := r.cache.Get(key); ok {
		return cached, nil
	}
	raw, err := r.fetch(ctx, creds.AccessToken, creds.ProviderSpecificData)
	if err != nil {
		if is401or403(err) && refreshTokenOf(creds) != "" && r.refresher != nil {
			log.Info("GROK_CLI_MODELS: got 401/403; refreshing token")
			refreshed, rerr := r.refresher.Refresh(ctx, refreshTokenOf(creds), creds.ProviderSpecificData, log)
			if rerr != nil || refreshed == nil || refreshed.AccessToken == "" {
				log.Warn("GROK_CLI_MODELS: token refresh did not return a token")
				return nil, nil
			}
			if opts.OnCredentialsRefreshed != nil {
				_ = opts.OnCredentialsRefreshed(*refreshed)
			}
			raw2, err2 := r.fetch(ctx, refreshed.AccessToken, creds.ProviderSpecificData)
			if err2 != nil {
				log.Warn("GROK_CLI_MODELS: retry after refresh failed", "error", err2)
				return nil, nil
			}
			raw = raw2
		} else {
			log.Warn("GROK_CLI_MODELS: fetch failed", "error", err)
			return nil, nil
		}
	}
	models := parseGrokCliModels(raw)
	if len(models) == 0 {
		return nil, nil
	}
	res := &Result{Models: models, RawModels: raw}
	r.cache.Set(key, res)
	return res, nil
}

func (r *grokCliResolver) fetch(ctx context.Context, token string, psd map[string]any) ([]map[string]any, error) {
	url := r.baseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", r.userAgent)
	req.Header.Set("x-xai-token-auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", r.version)
	req.Header.Set("x-grok-client-identifier", r.clientID)
	req.Header.Set("x-grok-client-mode", "headless")
	if email, ok := psd["email"].(string); ok && email != "" {
		req.Header.Set("x-email", email)
	}
	for _, k := range []string{"userId", "principalId"} {
		if v, ok := psd[k].(string); ok && v != "" {
			req.Header.Set("x-userid", v)
			break
		}
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grok-cli /models %d", resp.StatusCode)
	}
	// Response may be a top-level array or {data|models|results: [...]} or an
	// object map keyed by id. Model entries are preserved as raw maps so the
	// caller can read context_length / max_output_tokens.
	var arr []map[string]any
	if err := json.Unmarshal(body, &arr); err != nil {
		var asMap map[string]map[string]any
		if err := json.Unmarshal(body, &asMap); err != nil {
			return nil, err
		}
		for k, v := range asMap {
			if v == nil {
				v = map[string]any{}
			}
			if _, ok := v["id"]; !ok {
				v["id"] = k
			}
			arr = append(arr, v)
		}
		return arr, nil
	}
	return arr, nil
}

// parseGrokCliModels mirrors the JS parseGrokCliModels: normalizes id/name
// and surfaces contextLength / maxOutputTokens when the upstream reports them.
func parseGrokCliModels(raw []map[string]any) []ResolvedModel {
	seen := map[string]bool{}
	var out []ResolvedModel
	for _, m := range raw {
		id := firstNonEmpty(
			asString(m["id"]), asString(m["model_id"]), asString(m["modelId"]),
			asString(m["model"]), asString(m["slug"]), asString(m["name"]),
		)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := firstNonEmpty(asString(m["display_name"]), asString(m["displayName"]), asString(m["name"]), id)
		ctxLen := asPositiveInt(m["context_length"], m["contextLength"], m["context_window"], m["contextWindow"])
		rm := ResolvedModel{ID: id, Name: name, Kind: "chat", UpstreamModelID: id}
		if ctxLen > 0 {
			rm.ContextLength = ctxLen
		}
		out = append(out, rm)
	}
	return out
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", t))
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func asPositiveInt(vs ...any) int {
	for _, v := range vs {
		switch t := v.(type) {
		case float64:
			if t > 0 {
				return int(t)
			}
		case int:
			if t > 0 {
				return t
			}
		case int64:
			if t > 0 {
				return int(t)
			}
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

func grokCliCacheKey(creds provider.Credentials) string {
	seed := refreshTokenOf(creds)
	if seed == "" {
		seed = creds.AccessToken
	}
	return stableHash("grok-cli:" + seed)
}