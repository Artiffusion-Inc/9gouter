package resolver

import (
	"context"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// This file ports the proactive refresh orchestration from
// open-sse/services/oauthCredentialManager.js (withCredentialRefreshLock →
// shouldRefreshCredentials → refresh → mergeRefreshedCredentials → persist).
// decolua/9router #2703 Fix 2c.
//
// The orchestration lives in the resolver package (not the transport layer)
// so the dedup (resolver.SharedRefreshDedup) and the merge helpers are reused
// by both the chat-path proactive refresh (v1.go before serving) and the
// reactive 401/403 retry (Fix 2d). The transport layer supplies the
// TokenRefresher (from tokenrefresh.Lookup) and the proxy options; the
// resolver never imports tokenrefresh (which would close an import cycle:
// tokenrefresh → resolver).

// ProactiveRefreshResult is what ProactiveRefreshIfNeeded returns. Patch is
// the connection-data patch the caller persists via
// ConnectionRepo.ApplyConnectionPatch (or nil when no refresh was needed).
// Refreshed reports whether a refresh actually ran (Patch non-nil implies
// true, but Patch can be non-nil with Refreshed=false in the unrecoverable
// case). Unrecoverable reports that the refresh token is permanently invalid
// and the connection must be marked for re-auth.
type ProactiveRefreshResult struct {
	Patch        map[string]any
	Refreshed    bool
	Unrecoverable bool
}

// ProactiveRefreshIfNeeded runs a proactive credential refresh when the
// connection's access token is near expiry (within the provider's refresh
// lead) or stale past the provider's maxRefreshAgeMs. It dedups concurrent
// refreshes for the same connection (SharedRefreshDedup), routes the refresh
// HTTP call through the connection's proxy stack (opts), and returns a merge
// patch the caller persists.
//
// data is the connection's JSON data blob (accessToken/refreshToken/expiresAt/
// lastRefreshAt/...). refresher may be nil — when a provider has no refresh
// handler the function is a no-op (returns a zero result). now allows tests
// to inject a clock; production passes time.Now().
//
// The caller MUST persist Patch when Refreshed is true (or when Unrecoverable
// is true, persisting the marker so the connection is flagged for re-auth).
// Persistence is the caller's job — the resolver package does not touch the DB.
func ProactiveRefreshIfNeeded(ctx context.Context, providerID string, data map[string]any, refresher TokenRefresher, opts ProxyOptions, log Logger, now time.Time) (ProactiveRefreshResult, error) {
	if refresher == nil {
		return ProactiveRefreshResult{}, nil
	}
	if !ShouldRefreshCredentials(providerID, data, now) {
		return ProactiveRefreshResult{}, nil
	}
	creds := CredentialsForRefresh(data)
	refreshToken, _ := psdString(data, "refreshToken")
	// Copilot exchanges the GitHub access token, not a refresh token; its
	// refresher reads the access token out of PSD. Keep the refreshToken
	// argument as the connection's refresh token for everyone else — the
	// Copilot refresher ignores it.
	refreshed, err := refreshDeduped(ctx, refresher, creds, refreshToken, opts, log)
	if err != nil {
		return ProactiveRefreshResult{Refreshed: false}, err
	}
	patch := MergeRefreshedCredentials(providerID, data, refreshed, now)
	if patch == nil {
		return ProactiveRefreshResult{Refreshed: true}, nil
	}
	if UnrecoverableRefreshPatch(patch) {
		return ProactiveRefreshResult{Patch: patch, Refreshed: true, Unrecoverable: true}, nil
	}
	return ProactiveRefreshResult{Patch: patch, Refreshed: true}, nil
}

// keep the provider import referenced even when future edits drop direct use.
var _ = provider.Credentials{}