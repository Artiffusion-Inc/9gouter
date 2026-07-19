package resolver

import (
	"context"
	"sync"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// This file ports the JS token-refresh coalescing from
// open-sse/services/oauthCredentialManager.js (withCredentialRefreshLock —
// per-key inflight promise reuse) and open-sse/services/tokenRefresh/dedup.js
// (dedupRefresh — inflight + 10s successful-result cache). decolua/9router
// #2703 Fix 2b. Without it, a burst of chat requests on a freshly-expired
// token each trigger a separate refresh, racing the upstream token endpoint;
// with it the first caller refreshes and the rest reuse either the in-flight
// promise or the just-completed result.

// refreshResultTTL is how long a successful refresh result is reused for
// callers asking for the same key. Mirrors the JS REFRESH_RESULT_TTL_MS (10s).
// A failed refresh is never cached (the JS dedup deletes the key on throw).
const refreshResultTTL = 10 * time.Second

// RefreshDedup coalesces concurrent and near-concurrent token-refresh calls
// for the same key into a single upstream refresh. The empty key bypasses
// dedup and calls fn directly (mirrors dedupRefresh's `if (!oldToken) return
// fn()` early exit — a missing identity cannot be deduplicated safely).
//
// The concrete implementation is the process-wide SharedRefreshDedup; tests
// may substitute their own. A nil receiver is a no-op (calls fn directly).
type RefreshDedup interface {
	Refresh(ctx context.Context, key string, fn func() (*RefreshedCredentials, error)) (*RefreshedCredentials, error)
}

// refreshDedup is the concrete RefreshDedup. Safe for concurrent use.
type refreshDedup struct {
	mu       sync.Mutex
	inflight map[string]*refreshCall
	results  map[string]refreshCachedResult
}

type refreshCall struct {
	done   chan struct{}
	result *RefreshedCredentials
	err    error
}

type refreshCachedResult struct {
	result    *RefreshedCredentials
	expiresAt time.Time
}

func (d *refreshDedup) Refresh(ctx context.Context, key string, fn func() (*RefreshedCredentials, error)) (*RefreshedCredentials, error) {
	if key == "" {
		return fn()
	}
	if d == nil {
		return fn()
	}

	d.mu.Lock()
	if d.inflight == nil {
		d.inflight = map[string]*refreshCall{}
		d.results = map[string]refreshCachedResult{}
	}
	if cached, ok := d.results[key]; ok {
		if time.Now().Before(cached.expiresAt) {
			d.mu.Unlock()
			return cached.result, nil
		}
		delete(d.results, key)
	}
	if call, ok := d.inflight[key]; ok {
		d.mu.Unlock()
		select {
		case <-call.done:
			return call.result, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	d.inflight[key] = call
	d.mu.Unlock()

	call.result, call.err = fn()
	close(call.done)

	d.mu.Lock()
	delete(d.inflight, key)
	if call.err == nil && call.result != nil {
		d.results[key] = refreshCachedResult{result: call.result, expiresAt: time.Now().Add(refreshResultTTL)}
	}
	d.mu.Unlock()

	return call.result, call.err
}

// SharedRefreshDedup is the process-wide refresh dedup. The live resolvers
// and the chat-path refresh hook (Fix 2d) share it so a token rotating in
// the live resolver is visible to a concurrent chat refresh-retry within the
// TTL. Package-level singleton matches the JS module-level refreshLocks /
// refreshDedupCache Maps.
var SharedRefreshDedup RefreshDedup = &refreshDedup{}

// RefreshKey mirrors the JS getRefreshLockKey: a stable per-connection identity
// for dedup. Preference order matches the JS chain:
// connectionId → id → email → name → refreshToken(suffix) → "default".
// The refresh token lives in ProviderSpecificData["refreshToken"] (the
// Credentials struct does not have a dedicated field), mirroring how
// refreshTokenOf reads it elsewhere in this package.
func RefreshKey(creds provider.Credentials) string {
	psd := creds.ProviderSpecificData
	for _, k := range []string{"connectionId", "_connectionId", "id", "email", "name"} {
		if s, ok := psdString(psd, k); ok && s != "" {
			return s
		}
	}
	rt, _ := psdString(psd, "refreshToken")
	if rt != "" && len(rt) > 16 {
		return rt[len(rt)-16:]
	}
	if rt != "" {
		return rt
	}
	return "default"
}

// psdString reads a string field from a provider-specific-data map,
// tolerating string and numeric JSON values.
func psdString(m map[string]any, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[key]
	if !ok {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	default:
		return "", false
	}
}

// refreshDeduped runs a token refresh through the shared dedup so concurrent
// refresh requests for the same connection coalesce into one upstream call.
// Used by the kiro / copilot / grok-cli live resolvers (401/403 retry) and,
// in Fix 2d, by the chat-path reactive refresh. key is derived from creds
// via RefreshKey; opts is threaded into the underlying Refresh call (Fix 2a).
//
// refreshToken is the credential the refresher exchanges — for most providers
// it is creds.RefreshToken, but Copilot exchanges the GitHub access token
// (creds.AccessToken), so the caller passes it explicitly rather than the
// helper assuming creds.RefreshToken.
func refreshDeduped(ctx context.Context, refresher TokenRefresher, creds provider.Credentials, refreshToken string, opts ProxyOptions, log Logger) (*RefreshedCredentials, error) {
	key := RefreshKey(creds)
	return SharedRefreshDedup.Refresh(ctx, key, func() (*RefreshedCredentials, error) {
		return refresher.Refresh(ctx, refreshToken, creds.ProviderSpecificData, opts, log)
	})
}