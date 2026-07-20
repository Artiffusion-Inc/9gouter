package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// clinepassResolver fetches the live model catalog from Cline's
// /api/v1/models endpoint for an authenticated ClinePass connection, keeping
// only cline-pass/* entries. Mirrors open-sse/services/clinepassModels.js.
//
// API keys are sent as a plain Bearer token; OAuth access tokens must carry
// the WorkOS "workos:" prefix (handled here by clamping the token the same
// way buildClineHeaders does).
type clinepassResolver struct {
	cache    *Cache
	client   *http.Client
	baseURL  string // override for tests; empty = production endpoint
	appVer   string // 9gouter app version for X-CLIENT-VERSION headers
}

const (
	clinepassModelsEndpoint = "https://api.cline.bot/api/v1/models"
	clinepassFetchTimeout   = 5 * time.Second
)

// NewClinepassResolver builds a ClinePass live resolver.
func NewClinepassResolver(cache *Cache, appVersion string) LiveModelResolver {
	if cache == nil {
		cache = NewCache(5 * time.Minute)
	}
	return &clinepassResolver{
		cache:  cache,
		client: &http.Client{Timeout: clinepassFetchTimeout},
		appVer: appVersion,
	}
}

func (r *clinepassResolver) ProviderID() string { return "clinepass" }

func (r *clinepassResolver) Resolve(ctx context.Context, creds provider.Credentials, opts ResolveOpts) (*Result, error) {
	log := loggerOr(opts.Logger)
	isAPIKey := creds.APIKey != ""
	token := creds.APIKey
	if !isAPIKey {
		token = creds.AccessToken
	}
	if token == "" {
		log.Debug("CLINEPASS_MODELS: no token; skipping live fetch")
		return nil, nil
	}
	key := clinepassCacheKey(creds)
	if cached, ok := r.cache.Get(key); ok {
		return cached, nil
	}
	endpoint := clinepassModelsEndpoint
	if r.baseURL != "" {
		endpoint = r.baseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	r.setHeaders(req, token, isAPIKey)
	raw, err := r.fetch(req, log)
	if err != nil {
		log.Warn("CLINEPASS_MODELS: fetch failed", "error", err)
		return nil, nil
	}
	models := expandClinepass(raw)
	if len(models) == 0 {
		return nil, nil
	}
	res := &Result{Models: models, RawModels: clinepassRawList(raw)}
	r.cache.Set(key, res)
	return res, nil
}

func (r *clinepassResolver) setHeaders(req *http.Request, token string, isAPIKey bool) {
	req.Header.Set("Accept", "application/json")
	if isAPIKey {
		req.Header.Set("Authorization", "Bearer "+token)
		return
	}
	// OAuth: clamp to workos: prefix and carry the Cline client fingerprint.
	if !strings.HasPrefix(token, "workos:") {
		token = "workos:" + token
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("HTTP-Referer", "https://cline.bot")
	req.Header.Set("X-Title", "Cline")
	req.Header.Set("User-Agent", "9Gouter/"+r.appVer)
	req.Header.Set("X-PLATFORM", "linux")
	req.Header.Set("X-CLIENT-TYPE", "9gouter")
	req.Header.Set("X-CLIENT-VERSION", r.appVer)
	req.Header.Set("X-CORE-VERSION", r.appVer)
	req.Header.Set("X-IS-MULTIROOT", "false")
}

func (r *clinepassResolver) fetch(req *http.Request, log Logger) ([]map[string]any, error) {
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clinepass /models %d", resp.StatusCode)
	}
	// Response is either a top-level array or {data: [...]}.
	var arr []map[string]any
	if err := json.Unmarshal(body, &arr); err != nil {
		var wrap struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(body, &wrap); err != nil {
			return nil, err
		}
		arr = wrap.Data
	}
	return arr, nil
}

func expandClinepass(raw []map[string]any) []ResolvedModel {
	var out []ResolvedModel
	seen := map[string]bool{}
	for _, m := range raw {
		id, _ := m["id"].(string)
		if !strings.HasPrefix(id, "cline-pass/") {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		name, _ := m["name"].(string)
		if name == "" {
			name = id
		}
		out = append(out, ResolvedModel{ID: id, Name: name, Kind: "chat", UpstreamModelID: id})
	}
	return out
}

func clinepassRawList(raw []map[string]any) []map[string]any {
	return raw
}

func clinepassCacheKey(creds provider.Credentials) string {
	seed := creds.APIKey
	if seed == "" {
		seed = creds.AccessToken
	}
	return stableHash("clinepass:" + seed)
}