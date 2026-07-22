package resolver

import "time"

// This file ports the per-provider OAuth refresh policy constants from the JS
// registry (open-sse/providers/registry/*.js oauth.{refreshLeadMs, maxRefreshAgeMs,
// trackRefreshAt}) and open-sse/config/appConstants.js (TOKEN_EXPIRY_BUFFER_MS,
// REFRESH_LEAD_MS). The JS build derived REFRESH_LEAD_MS from the registry; the
// Go rewrite has no such registry, so the values are inlined here as a small
// table keyed by provider id. decolua/9router #2703 Fix 2c.

// tokenExpiryBufferMs is the default refresh lead when a provider does not
// declare its own refreshLeadMs. Mirrors JS TOKEN_EXPIRY_BUFFER_MS (5 min).
const tokenExpiryBufferMs = 5 * 60 * 1000

// oauthPolicy is the per-provider refresh policy.
type oauthPolicy struct {
	// refreshLeadMs is how far before expiry a token is proactively
	// refreshed. 0 → tokenExpiryBufferMs default.
	refreshLeadMs int
	// maxRefreshAgeMs, when non-zero, triggers a proactive refresh once the
	// last refresh is older than this (codex: 8 days) — even if the access
	// token is not yet near expiry, because the refresh token itself may be
	// rotated out from under a long-running connection.
	maxRefreshAgeMs int
	// trackRefreshAt, when true, always stamps lastRefreshAt on a successful
	// merge (codex), so maxRefreshAge staleness can be measured.
	trackRefreshAt bool
}

// oauthPolicies inlines the registry oauth blocks. Only providers that
// declare a non-default policy are listed; all others use
// tokenExpiryBufferMs. Values copied verbatim from the JS registry.
var oauthPolicies = map[string]oauthPolicy{
	"codex": {
		refreshLeadMs:   432000000, // 5 days
		maxRefreshAgeMs: 691200000, // 8 days
		trackRefreshAt:  true,
	},
	// kimi: merged dual-auth provider (upstream v0.5.40, commit 68566f53).
	// CLIProxyAPI refreshThresholdSeconds = 300. The legacy `kimi-coding` id
	// resolves to `kimi` via the registry alias map, so a separate entry is no
	// longer needed.
	"kimi": {
		refreshLeadMs: 300000, // 5 min
	},
}

// refreshLeadMs returns the proactive-refresh lead for a provider (its own
// refreshLeadMs, else the default tokenExpiryBufferMs). Mirrors JS
// getRefreshLeadMs.
func refreshLeadMs(providerID string) time.Duration {
	if p, ok := oauthPolicies[providerID]; ok && p.refreshLeadMs > 0 {
		return time.Duration(p.refreshLeadMs) * time.Millisecond
	}
	return time.Duration(tokenExpiryBufferMs) * time.Millisecond
}

// oauthPolicyFor returns the policy for a provider (zero value when absent).
func oauthPolicyFor(providerID string) oauthPolicy {
	if p, ok := oauthPolicies[providerID]; ok {
		return p
	}
	return oauthPolicy{}
}
