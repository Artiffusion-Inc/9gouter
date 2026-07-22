package resolver

// cursor.go ports the cursor live-model resolver from open-sse/services/
// cursorModels.js (upstream v0.5.40, commit 6994cd1f). Cursor exposes the
// account-specific model picker through the AgentService GetUsableModels
// Connect RPC at agent.api5.cursor.sh (HTTP/2-only). The resolver POSTs an
// unframed application/proto body with the full Cursor header set (built by
// cursorexec.BuildCursorHeaders — bumped clientVersion 3.12.17 + the
// x-cursor-client-commit gateway fingerprint) and decodes the
// agent.v1.GetUsableModelsResponse via cursorexec.ParseCursorUsableModels.
//
// Unlike grok-cli / codex, the cursor resolver does NOT refresh on 401: the
// upstream cursorModels.js returns null on any failure so callers fall back to
// the static catalog, and there is no CursorRefresher. On any failure (no
// accessToken/machineId, network, 4xx/5xx, empty) the resolver returns
// (nil, nil) so /v1/models and the dashboard never break.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	cursorexec "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/cursor"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

const (
	cursorAgentBaseURL = "https://agent.api5.cursor.sh"
	cursorModelsPath   = "/agent.v1.AgentService/GetUsableModels"
	cursorFetchTimeout = 10 * time.Second
	cursorCacheTTL     = 5 * time.Minute
)

type cursorResolver struct {
	cache   *Cache
	client  *http.Client
	baseURL string // test override
}

// NewCursorResolver builds a Cursor live resolver. Cursor has no token
// refresher (the upstream returns null on auth failure), so unlike the codex /
// grok-cli resolvers there is no TokenRefresher argument.
func NewCursorResolver(cache *Cache) LiveModelResolver {
	if cache == nil {
		cache = NewCache(cursorCacheTTL)
	}
	return &cursorResolver{
		cache:   cache,
		client:  &http.Client{Timeout: cursorFetchTimeout},
		baseURL: cursorAgentBaseURL,
	}
}

func (r *cursorResolver) ProviderID() string { return "cursor" }

// setTransport swaps the HTTP transport (test hook; see resolver.withSwap).
func (r *cursorResolver) setTransport(t http.RoundTripper) { r.client.Transport = t }

func (r *cursorResolver) Resolve(ctx context.Context, creds provider.Credentials, opts ResolveOpts) (*Result, error) {
	log := loggerOr(opts.Logger)
	token := creds.AccessToken
	machineID, _ := creds.ProviderSpecificData["machineId"].(string)
	if token == "" || machineID == "" {
		log.Debug("CURSOR_MODELS: no accessToken or machineId; skipping live fetch")
		return nil, nil
	}

	key := cursorCacheKey(creds)
	if cached, ok := r.cache.Get(key); ok {
		return cached, nil
	}

	raw, err := r.fetch(ctx, token, machineID, creds.ProviderSpecificData)
	if err != nil {
		log.Warn("CURSOR_MODELS: live model fetch failed", "error", err)
		return nil, nil
	}

	parsed := cursorexec.ParseCursorUsableModels(raw)
	if len(parsed) == 0 {
		return nil, nil
	}
	models := make([]ResolvedModel, 0, len(parsed))
	for _, m := range parsed {
		models = append(models, ResolvedModel{ID: m.ID, Name: m.Name, Kind: "chat", UpstreamModelID: m.ID})
	}
	res := &Result{Models: models}
	r.cache.Set(key, res)
	return res, nil
}

func (r *cursorResolver) fetch(ctx context.Context, token, machineID string, psd map[string]any) ([]byte, error) {
	u := strings.TrimRight(r.baseURL, "/") + cursorModelsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return nil, err
	}
	ghostMode := true
	if v, ok := psd["ghostMode"].(bool); ok {
		ghostMode = v
	} else if s, ok := psd["ghostMode"].(string); ok {
		ghostMode = s != "false"
	}
	headers := cursorexec.BuildCursorHeaders(cursorexec.CursorHeadersOpts{
		AccessToken: token,
		MachineID:   machineID,
		GhostMode:   ghostMode,
	})
	// Connect unary calls use an unframed protobuf body, unlike Cursor chat's
	// streaming application/connect+proto endpoint. Strip the streaming-only
	// connect headers.
	delete(headers, "connect-accept-encoding")
	delete(headers, "connect-protocol-version")
	headers["accept"] = "application/proto"
	headers["content-type"] = "application/proto"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// Empty protobuf body — GetUsableModels takes no request payload.
	req.Body = nil

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cursor GetUsableModels %d", resp.StatusCode)
	}
	return body, nil
}

// cursorCacheKey builds a stable per-credential cache key from the machineId +
// accessToken, mirroring the JS cacheKey.
func cursorCacheKey(creds provider.Credentials) string {
	seed := ""
	if mid, ok := creds.ProviderSpecificData["machineId"].(string); ok && mid != "" {
		seed = mid + ":"
	}
	seed += creds.AccessToken
	if seed == "" {
		return stableHash("cursor:cursor-anonymous")
	}
	return stableHash("cursor:" + seed)
}
