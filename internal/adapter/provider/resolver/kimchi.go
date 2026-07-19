package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// kimchiResolver fetches the live model catalog from the Kimchi
// /v1/models/metadata endpoint for an authenticated connection. Mirrors
// open-sse/services/kimchiModels.js. Kimchi has no OAuth refresh — the token
// is a long-lived apiKey / accessToken. The endpoint is configurable via
// ProviderSpecificData.kimchiEndpoint (default https://llm.kimchi.dev).
type kimchiResolver struct {
	cache   *Cache
	client  *http.Client
	baseURL string // default endpoint; psd.kimchiEndpoint overrides per-connection
}

const (
	kimchiDefaultAPI   = "https://llm.kimchi.dev"
	kimchiUserAgent    = "kimchi/0.1.40"
	kimchiFetchTimeout = 20 * time.Second
	kimchiCacheTTL     = 5 * time.Minute
)

// NewKimchiResolver builds a Kimchi live resolver.
func NewKimchiResolver(cache *Cache) LiveModelResolver {
	if cache == nil {
		cache = NewCache(kimchiCacheTTL)
	}
	return &kimchiResolver{
		cache:   cache,
		client:  &http.Client{Timeout: kimchiFetchTimeout},
		baseURL: kimchiDefaultAPI,
	}
}

func (r *kimchiResolver) ProviderID() string { return "kimchi" }

func (r *kimchiResolver) Resolve(ctx context.Context, creds provider.Credentials, opts ResolveOpts) (*Result, error) {
	log := loggerOr(opts.Logger)
	token := kimchiToken(creds)
	if token == "" {
		log.Debug("KIMCHI_MODELS: no token; skipping live fetch")
		return nil, nil
	}
	endpoint := kimchiEndpoint(creds, r.baseURL)
	key := kimchiCacheKey(creds, endpoint)
	if cached, ok := r.cache.Get(key); ok {
		return cached, nil
	}
	raw, err := r.fetch(ctx, token, endpoint)
	if err != nil {
		log.Warn("KIMCHI_MODELS: fetch failed", "error", err)
		return nil, nil
	}
	models := normalizeKimchiModels(raw)
	if len(models) == 0 {
		return nil, nil
	}
	res := &Result{Models: models, RawModels: raw}
	r.cache.Set(key, res)
	return res, nil
}

func (r *kimchiResolver) fetch(ctx context.Context, token, endpoint string) ([]map[string]any, error) {
	url := strings.TrimRight(endpoint, "/") + "/v1/models/metadata?include_in_cli=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", kimchiUserAgent)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kimchi /models %d", resp.StatusCode)
	}
	var wrap struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, err
	}
	return wrap.Models, nil
}

// normalizeKimchiModels mirrors the JS normalizeKimchiModel: extracts id,
// name, kind (llm vs imageToText), contextLength, reasoning, and vision.
func normalizeKimchiModels(raw []map[string]any) []ResolvedModel {
	var out []ResolvedModel
	seen := map[string]bool{}
	for _, item := range raw {
		id := firstNonEmpty(asString(item["slug"]), asString(item["id"]), asString(item["model"]), asString(item["name"]))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := firstNonEmpty(asString(item["display_name"]), asString(item["displayName"]), asString(item["name"]), id)
		inputMods, _ := item["input_modalities"].([]any)
		vision := containsAny(inputMods, "image")
		reasoning, _ := item["reasoning"].(bool)
		kind := "llm"
		if containsAny(inputMods, "image") {
			kind = "imageToText"
		}
		ctxLen := 0
		if limits, ok := item["limits"].(map[string]any); ok {
			ctxLen = asPositiveInt(limits["context_window"])
		}
		if ctxLen == 0 {
			ctxLen = asPositiveInt(item["contextLength"], item["context_length"])
		}
		rm := ResolvedModel{ID: id, Name: name, Kind: kind, UpstreamModelID: id}
		if ctxLen > 0 {
			rm.ContextLength = ctxLen
		}
		rm.Capabilities = &Capabilities{Thinking: reasoning, Agentic: false}
		_ = vision // surfaced via Capabilities if needed later
		out = append(out, rm)
	}
	return out
}

func containsAny(arr []any, target string) bool {
	for _, v := range arr {
		if s, ok := v.(string); ok && s == target {
			return true
		}
	}
	return false
}

func kimchiToken(creds provider.Credentials) string {
	if creds.AccessToken != "" {
		return creds.AccessToken
	}
	if creds.APIKey != "" {
		return creds.APIKey
	}
	if v, ok := creds.ProviderSpecificData["apiKey"].(string); ok {
		return v
	}
	return ""
}

func kimchiEndpoint(creds provider.Credentials, fallback string) string {
	if v, ok := creds.ProviderSpecificData["kimchiEndpoint"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func kimchiCacheKey(creds provider.Credentials, endpoint string) string {
	psd := creds.ProviderSpecificData
	seed := ""
	for _, k := range []string{"userId", "username"} {
		if v, ok := psd[k].(string); ok && v != "" {
			seed = v
			break
		}
	}
	if seed == "" {
		seed = refreshTokenOf(creds)
	}
	if seed == "" {
		seed = kimchiToken(creds)
	}
	if seed == "" {
		seed = "anonymous"
	}
	return stableHash("kimchi:" + strings.TrimRight(endpoint, "/") + ":" + seed)
}