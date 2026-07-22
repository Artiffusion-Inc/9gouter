package resolver

// codex.go ports the codex live model resolver from upstream v0.5.40
// (src/app/api/providers/[id]/models/route.js, commit d587b2a4). The endpoint
// /codex/models gates each entry by minimal_client_version against a query
// value; codex CLI's own manifest (openai/codex codex-rs/models-manager/models.json)
// requires 0.144.0 for its newest models, so a stale client_version here comes
// back 200 with those entries quietly missing instead of erroring. We send
// 0.144.6 (above the gate), the originator: codex_cli_rs header every other
// codex call site uses, and retry once on 401/403 via the CodexRefresher
// (refresh-aware model sync), matching gemini-cli / grok-cli.
//
// On any failure (network, 4xx/5xx, missing credentials, empty result) the
// resolver returns (nil, nil) and the caller falls back to the static catalog,
// so a broken live fetch never breaks /v1/models or the dashboard models list.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// CODEX_CLIENT_VERSION is the client_version query sent to /codex/models. It
// must stay above the minimal_client_version gate codex CLI's manifest enforces
// for its newest models (0.144.0); a stale value returns 200 with entries
// silently filtered out instead of erroring.
const CODEX_CLIENT_VERSION = "0.144.6"

// CODEX_MODELS_URL is the live catalog endpoint with the client_version gate.
const CODEX_MODELS_URL = "https://chatgpt.com/backend-api/codex/models?client_version=" + CODEX_CLIENT_VERSION

const (
	codexFetchTimeout = 15 * time.Second
	codexCacheTTL     = 5 * time.Minute
)

type codexResolver struct {
	cache     *Cache
	refresher TokenRefresher // *tokenrefresh.CodexRefresher
	client    *http.Client
	baseURL   string // test override
}

// NewCodexResolver builds a Codex live resolver. refresher is the codex OAuth
// refresher (tokenrefresh.NewCodexRefresher); pass nil to disable refresh
// (resolver falls back to static on 401).
func NewCodexResolver(cache *Cache, refresher TokenRefresher) LiveModelResolver {
	if cache == nil {
		cache = NewCache(codexCacheTTL)
	}
	return &codexResolver{
		cache:     cache,
		refresher: refresher,
		client:    &http.Client{Timeout: codexFetchTimeout},
	}
}

func (r *codexResolver) ProviderID() string { return "codex" }

// setTransport swaps the HTTP transport (test hook; see resolver.withSwap).
func (r *codexResolver) setTransport(t http.RoundTripper) { r.client.Transport = t }

func (r *codexResolver) Resolve(ctx context.Context, creds provider.Credentials, opts ResolveOpts) (*Result, error) {
	log := loggerOr(opts.Logger)
	token := creds.AccessToken
	if token == "" {
		log.Debug("CODEX_MODELS: no accessToken; skipping live fetch")
		return nil, nil
	}
	key := codexCacheKey(creds)
	if cached, ok := r.cache.Get(key); ok {
		return cached, nil
	}
	raw, err := r.fetch(ctx, token)
	if err != nil {
		// 401/403 → refresh the codex token and retry once, persisting the
		// refreshed token through OnCredentialsRefreshed (refresh-aware sync).
		if is401or403(err) && refreshTokenOf(creds) != "" && r.refresher != nil {
			log.Info("CODEX_MODELS: got 401/403; refreshing codex token")
			refreshed, rerr := refreshDeduped(ctx, r.refresher, creds, refreshTokenOf(creds), opts.ProxyOptions, log)
			if rerr != nil || refreshed == nil || refreshed.AccessToken == "" {
				log.Warn("CODEX_MODELS: token refresh did not return a token")
				return nil, nil
			}
			if opts.OnCredentialsRefreshed != nil {
				_ = opts.OnCredentialsRefreshed(*refreshed)
			}
			raw2, err2 := r.fetch(ctx, refreshed.AccessToken)
			if err2 != nil {
				log.Warn("CODEX_MODELS: retry after refresh failed", "error", err2)
				return nil, nil
			}
			raw = raw2
		} else {
			log.Warn("CODEX_MODELS: fetch failed", "error", err)
			return nil, nil
		}
	}
	models := expandCodex(raw)
	if len(models) == 0 {
		return nil, nil
	}
	res := &Result{Models: models, RawModels: raw}
	r.cache.Set(key, res)
	return res, nil
}

func (r *codexResolver) fetch(ctx context.Context, token string) ([]map[string]any, error) {
	u := CODEX_MODELS_URL
	if r.baseURL != "" {
		u = r.baseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("originator", "codex_cli_rs")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex /models %d", resp.StatusCode)
	}
	// The endpoint may return a bare array or an OpenAI-shaped {data:[...]}.
	parsed := parseOpenAIStyleModels(body)
	return parsed, nil
}

// parseOpenAIStyleModels accepts either a bare JSON array or an OpenAI-shaped
// envelope ({data|models|results}) and returns the model slice. Mirrors the JS
// parseOpenAIStyleModels helper.
func parseOpenAIStyleModels(body []byte) []map[string]any {
	// Bare array?
	var arr []map[string]any
	if err := json.Unmarshal(body, &arr); err == nil {
		return arr
	}
	var env struct {
		Data    []map[string]any `json:"data"`
		Models  []map[string]any `json:"models"`
		Results []map[string]any `json:"results"`
	}
	_ = json.Unmarshal(body, &env)
	switch {
	case env.Data != nil:
		return env.Data
	case env.Models != nil:
		return env.Models
	case env.Results != nil:
		return env.Results
	}
	return nil
}

// expandCodex maps the raw codex catalog into ResolvedModel entries, appending a
// synthetic <id>-review variant for every chat model (mirrors JS
// appendCodexReviewModels). Image and embed models are skipped; existing
// -review ids are not duplicated.
func expandCodex(raw []map[string]any) []ResolvedModel {
	var out []ResolvedModel
	seen := map[string]bool{}
	for _, m := range raw {
		id := firstNonEmpty(strOf(m["id"]), strOf(m["slug"]), strOf(m["model"]), strOf(m["name"]))
		if id == "" {
			continue
		}
		name := firstNonEmpty(strOf(m["display_name"]), strOf(m["displayName"]), strOf(m["name"]), id)
		isChat := modelType(m) != "image" && !containsFold(id, "embed")
		if !isChat || endsWith(id, "-review") {
			if !seen[id] {
				seen[id] = true
				out = append(out, ResolvedModel{ID: id, Name: name, Kind: "chat", UpstreamModelID: id})
			}
			continue
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, ResolvedModel{ID: id, Name: name, Kind: "chat", UpstreamModelID: id})
		}
		reviewID := id + "-review"
		if !seen[reviewID] {
			seen[reviewID] = true
			out = append(out, ResolvedModel{
				ID:              reviewID,
				Name:            name + " Review",
				Kind:            "chat",
				UpstreamModelID: id,
			})
		}
	}
	return out
}

func modelType(m map[string]any) string {
	if t, ok := m["type"].(string); ok && t != "" {
		return t
	}
	return "llm"
}

// strOf reads a string field from a raw map entry (any → string).
func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func containsFold(s, sub string) bool {
	if sub == "" {
		return false
	}
	ls := len(s)
	lsub := len(sub)
	for i := 0; i+lsub <= ls; i++ {
		if equalFold(s[i:i+lsub], sub) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca == cb {
			continue
		}
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func codexCacheKey(creds provider.Credentials) string {
	seed := creds.AccessToken
	if seed == "" {
		seed = "codex-anonymous"
	}
	return stableHash("codex:" + seed)
}