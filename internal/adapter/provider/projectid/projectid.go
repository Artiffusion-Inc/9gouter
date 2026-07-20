// Package projectid ports open-sse/services/projectId.js: it fetches and
// caches the real Google Cloud Code project ID bound to an authenticated
// Antigravity / Gemini CLI account, so requests to those providers carry the
// user's actual project rather than a random one (which Google's anti-abuse
// system flags). decolua/9gouter #2703 Fix 2e.
//
// The flow mirrors the JS service 1:1:
//
//  1. loadCodeAssist (POST v1internal:loadCodeAssist) → if it returns a
//     cloudaicompanionProject, that is the project id.
//  2. Otherwise onboardUser (POST v1internal:onboardUser, polled up to 5x
//     until done=true) → extract the project id from the onboard response.
//
// Results are cached per connection for 1h with inflight dedup so concurrent
// chat requests on the same connection share one fetch. A failed fetch returns
// ("", nil) — the caller falls back, mirroring the JS null return.
package projectid

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sync"
	"time"
)

// Endpoint constants — copied verbatim from open-sse/config/appConstants.js
// CLOUD_CODE_API and the loadCodeAssist/onboardUser paths. Declared as vars
// (not const) so tests can redirect them at an httptest server.
var (
	loadCodeAssistURL = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	onboardUserURL    = "https://cloudcode-pa.googleapis.com/v1internal:onboardUser"
)

const (
	cacheTTL    = 60 * time.Minute // projectIdCache freshness (JS CACHE_TTL_MS 1h)
	pendingTTL  = 2 * time.Minute  // abort an inflight fetch older than this (JS PENDING_TTL_MS)
	cleanupEvery = 10 * time.Minute // JS CLEANUP_INTERVAL_MS

	onboardMaxAttempts  = 5
	onboardAttemptTimeout = 30 * time.Second
)

var (
	// onboardRetryDelay is a var (not const) so tests can shorten the 2s
	// onboarding poll interval.
	onboardRetryDelay = 2 * time.Second
)

// ideType / pluginType / platform enum values, copied from
// appConstants.js IDE_TYPE / PLUGIN_TYPE / PLATFORM.
const (
	ideTypeAntigravity = 9
	pluginTypeGemini   = 2

	platformUnspecified  = 0
	platformDarwinAMD64  = 1
	platformDarwinARM64  = 2
	platformLinuxAMD64   = 3
	platformLinuxARM64   = 4
	platformWindowsAMD64 = 5
)

// Fetcher is the per-connection project-id resolver. It owns the cache and
// inflight dedup; the HTTP client is injectable for tests. The zero Fetcher is
// NOT usable — use New().
type Fetcher struct {
	client *http.Client

	mu      sync.Mutex
	cache   map[string]cacheEntry
	pending map[string]*pendingFetch

	stopOnce sync.Once
	stopCh   chan struct{}
}

type cacheEntry struct {
	projectID string
	fetchedAt time.Time
}

type pendingFetch struct {
	done   chan struct{}
	result string
	// startedAt is used by the cleanup sweep to abort fetches stuck longer than
	// pendingTTL (the JS service aborts via AbortController; here the sweep
	// just records and drops the entry — the inflight HTTP call's own context
	// timeout bounds it).
	startedAt time.Time
}

// New returns a Fetcher backed by the given client (nil → http.DefaultClient
// with a 60s timeout). It starts a background cleanup sweep that stops when
// Stop is called or the process exits (best-effort; the sweep goroutine does
// not block exit because it sleeps on a ticker that is never unref'd in Go,
// but Stop cancels it).
func New(client *http.Client) *Fetcher {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	f := &Fetcher{
		client:  client,
		cache:   make(map[string]cacheEntry),
		pending: make(map[string]*pendingFetch),
		stopCh:  make(chan struct{}),
	}
	go f.cleanupLoop()
	return f
}

// Stop halts the background cleanup sweep. Idempotent; safe to call from
// shutdown hooks.
func (f *Fetcher) Stop() {
	if f == nil {
		return
	}
	f.stopOnce.Do(func() { close(f.stopCh) })
}

func (f *Fetcher) cleanupLoop() {
	t := time.NewTicker(cleanupEvery)
	defer t.Stop()
	for {
		select {
		case <-f.stopCh:
			return
		case <-t.C:
			f.cleanup()
		}
	}
}

// cleanup evicts stale cache entries and drops pending fetches older than
// pendingTTL (the inflight HTTP call is bounded by its own context timeout).
func (f *Fetcher) cleanup() {
	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, e := range f.cache {
		if now.Sub(e.fetchedAt) >= cacheTTL {
			delete(f.cache, id)
		}
	}
	for id, p := range f.pending {
		if now.Sub(p.startedAt) > pendingTTL {
			delete(f.pending, id)
		}
	}
}

// Get returns the cached project id for the connection if fresh.
func (f *Fetcher) Get(connectionID string) (string, bool) {
	if f == nil || connectionID == "" {
		return "", false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.cache[connectionID]
	if !ok {
		return "", false
	}
	if time.Since(e.fetchedAt) >= cacheTTL {
		return "", false
	}
	return e.projectID, true
}

// Invalidate drops the cached project id for a connection (call after a token
// rotation that may have rebound the user to a different project).
func (f *Fetcher) Invalidate(connectionID string) {
	if f == nil || connectionID == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.cache, connectionID)
}

// RemoveConnection drops both the cache entry and any inflight fetch for a
// connection (call when a connection is deleted).
func (f *Fetcher) RemoveConnection(connectionID string) {
	if f == nil || connectionID == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.cache, connectionID)
	delete(f.pending, connectionID)
}

// ForConnection returns the real project id for an authenticated
// Antigravity / Gemini CLI connection, fetching it from the Cloud Code
// Assist API on a cache miss. Concurrent calls for the same connection
// coalesce into a single fetch. Returns ("", nil) on any failure — the caller
// falls back (the JS service returns null and the chat path proceeds without
// a project id, which surfaces upstream as a 400 the user can re-auth past).
//
// accessToken is the OAuth access token (post-refresh). proxyURL is reserved
// for routing the fetch through the connection's proxy stack; the JS service
// used a plain fetch, so this is currently direct (Fix 2e scope: project-id
// fetch, not route-awareness — the loadCodeAssist endpoint is a Google API not
// behind the customer proxy).
func (f *Fetcher) ForConnection(ctx context.Context, connectionID, accessToken string) (string, error) {
	if f == nil || connectionID == "" || accessToken == "" {
		return "", nil
	}
	// Fresh cache hit — no lock contention past the read.
	if id, ok := f.Get(connectionID); ok {
		return id, nil
	}

	f.mu.Lock()
	if p, ok := f.pending[connectionID]; ok {
		f.mu.Unlock()
		// Wait for the leader's result; honor ctx cancellation.
		select {
		case <-p.done:
			return p.result, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	p := &pendingFetch{done: make(chan struct{}), startedAt: time.Now()}
	f.pending[connectionID] = p
	f.mu.Unlock()

	result := f.fetchAndCache(ctx, connectionID, accessToken)

	f.mu.Lock()
	delete(f.pending, connectionID)
	if result != "" {
		f.cache[connectionID] = cacheEntry{projectID: result, fetchedAt: time.Now()}
	}
	p.result = result
	close(p.done)
	f.mu.Unlock()

	return result, nil
}

// fetchAndCache runs the loadCodeAssist → onboardUser flow. A fetch error is
// logged-to-nil: the caller gets ("") and proceeds, matching the JS
// "return null on failure" contract.
func (f *Fetcher) fetchAndCache(ctx context.Context, connectionID, accessToken string) string {
	pid, err := f.fetchProjectID(ctx, accessToken)
	if err != nil || pid == "" {
		// Match the JS service: swallow the error, return empty so the caller
		// falls back. The error is not surfaced to the chat request.
		return ""
	}
	return pid
}

// fetchProjectID mirrors fetchProjectId: loadCodeAssist first; if no project,
// pick the default tier and onboardUser (polled).
func (f *Fetcher) fetchProjectID(ctx context.Context, accessToken string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"metadata": loadCodeAssistMetadata(),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loadCodeAssistURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	applyLoadCodeAssistHeaders(req, accessToken)
	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("loadCodeAssist: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var data loadCodeAssistResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", fmt.Errorf("loadCodeAssist: decode: %w", err)
	}
	if pid := extractProjectID(data.CloudAICompanionProject); pid != "" {
		return pid, nil
	}
	// No project yet — onboard the user with the default tier (legacy-tier
	// fallback when no tier is marked isDefault).
	tierID := "legacy-tier"
	for _, tier := range data.AllowedTiers {
		if tier != nil && tier.IsDefault && tier.ID != "" {
			tierID = tier.ID
			break
		}
	}
	return f.onboardUser(ctx, accessToken, tierID)
}

// onboardUser mirrors the JS onboardUser poll: up to onboardMaxAttempts POSTs,
// each with a 30s timeout, retrying every 2s until done=true.
func (f *Fetcher) onboardUser(ctx context.Context, accessToken, tierID string) (string, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"tierId":   tierID,
		"metadata": loadCodeAssistMetadata(),
	})
	for attempt := 1; attempt <= onboardMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, onboardAttemptTimeout)
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, onboardUserURL, bytes.NewReader(reqBody))
		if err != nil {
			cancel()
			return "", err
		}
		applyLoadCodeAssistHeaders(req, accessToken)
		resp, err := f.client.Do(req)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			// Network error: wait and retry (matches JS continue-on-error).
			if !sleepCtx(ctx, onboardRetryDelay) {
				return "", ctx.Err()
			}
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if attempt == onboardMaxAttempts {
				return "", fmt.Errorf("onboardUser: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
			}
			if !sleepCtx(ctx, onboardRetryDelay) {
				return "", ctx.Err()
			}
			continue
		}
		var data onboardResponse
		if err := json.Unmarshal(raw, &data); err != nil {
			if attempt == onboardMaxAttempts {
				return "", fmt.Errorf("onboardUser: decode: %w", err)
			}
			if !sleepCtx(ctx, onboardRetryDelay) {
				return "", ctx.Err()
			}
			continue
		}
		if data.Done {
			if pid := extractProjectID(data.Response.CloudAICompanionProject); pid != "" {
				return pid, nil
			}
			return "", fmt.Errorf("onboardUser done but no project_id")
		}
		if !sleepCtx(ctx, onboardRetryDelay) {
			return "", ctx.Err()
		}
	}
	return "", nil
}

// loadCodeAssistResponse models the loadCodeAssist response subset we read.
type loadCodeAssistResponse struct {
	CloudAICompanionProject cloudAIProject `json:"cloudaicompanionProject"`
	AllowedTiers            []*allowedTier  `json:"allowedTiers"`
}

// onboardResponse models the onboardUser polled response.
type onboardResponse struct {
	Done     bool `json:"done"`
	Response struct {
		CloudAICompanionProject cloudAIProject `json:"cloudaicompanionProject"`
	} `json:"response"`
}

// cloudAICompanionProject is either a string (the project id) or an object
// with an id field — the JS extractProjectId handles both shapes.
type cloudAIProject struct {
	raw  json.RawMessage
	id   string
	str  string
}

func (p *cloudAIProject) UnmarshalJSON(data []byte) error {
	p.raw = data
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err == nil {
			p.str = s
		}
		return nil
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &obj); err == nil {
		p.id = obj.ID
	}
	return nil
}

// extractProjectID mirrors the JS extractProjectId / extractProjectIdFromOnboard:
// a non-empty string wins; otherwise the object's id.
func extractProjectID(p cloudAIProject) string {
	if p.str != "" {
		return p.str
	}
	return p.id
}

type allowedTier struct {
	ID        string `json:"id"`
	IsDefault bool   `json:"isDefault"`
}

// loadCodeAssistMetadata returns the request metadata body copied from
// LOAD_CODE_ASSIST_METADATA (ideType=ANTIGRAVITY=9, pluginType=GEMINI=2,
// platform=runtime-derived).
func loadCodeAssistMetadata() map[string]any {
	return map[string]any{
		"ideType":    ideTypeAntigravity,
		"platform":    platformEnum(),
		"pluginType": pluginTypeGemini,
	}
}

// platformEnum mirrors getPlatformEnum: darwin/linux/windows × amd64/arm64.
func platformEnum() int {
	switch runtime.GOOS {
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return platformDarwinARM64
		}
		return platformDarwinAMD64
	case "linux":
		if runtime.GOARCH == "arm64" {
			return platformLinuxARM64
		}
		return platformLinuxAMD64
	case "windows":
		return platformWindowsAMD64
	}
	return platformUnspecified
}

// applyLoadCodeAssistHeaders sets the LOAD_CODE_ASSIST_HEADERS + Authorization.
func applyLoadCodeAssistHeaders(req *http.Request, accessToken string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "google-api-nodejs-client/9.15.1")
	req.Header.Set("X-Goog-Api-Client", "google-cloud-sdk vscode_cloudshelleditor/0.1")
	clientMeta, _ := json.Marshal(map[string]any{
		"ideType":    ideTypeAntigravity,
		"platform":    platformEnum(),
		"pluginType": pluginTypeGemini,
	})
	req.Header.Set("Client-Metadata", string(clientMeta))
	req.Header.Set("Authorization", "Bearer "+accessToken)
}

// sleepCtx sleeps for d, returning false if ctx was cancelled during the wait.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}