package resolver

import (
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// This file ports the proactive-refresh decision (shouldRefreshCredentials)
// and the refresh-result merge (mergeRefreshedCredentials) from
// open-sse/services/oauthCredentialManager.js. decolua/9router #2703 Fix 2c.
//
// The credential material here lives on a connection record's JSON data blob
// (accessToken / refreshToken / expiresAt / idToken / projectId /
// providerSpecificData / ...), surfaced into provider.Credentials at the
// call site. Merge works over a generic map[string]any patch (the connection
// data view), so it can be persisted via ConnectionRepo.ApplyConnectionPatch
// without coupling the resolver package to the db layer.

// expiryMsFromCreds mirrors getCredentialExpiryMs: parse expiresAt (or
// tokenExpiresAt) as either a JS epoch-ms number or an RFC3339 string.
// Returns nil when absent/unparseable.
func expiryMsFromCreds(data map[string]any) *time.Time {
	for _, k := range []string{"expiresAt", "tokenExpiresAt"} {
		v, ok := data[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case float64:
			if t <= 0 {
				continue
			}
			ms := int64(t)
			if ms < 1e12 { // seconds → ms (JS Date.now() is ~1.7e12)
				ms = ms * 1000
			}
			tt := time.UnixMilli(ms).UTC()
			return &tt
		case string:
			if tt, err := time.Parse(time.RFC3339Nano, t); err == nil {
				return &tt
			}
			if tt, err := time.Parse(time.RFC3339, t); err == nil {
				return &tt
			}
		}
	}
	return nil
}

// lastRefreshAtFromCreds mirrors getCredentialLastRefreshMs: lastRefreshAt or
// lastRefresh (top-level or in providerSpecificData).
func lastRefreshAtFromCreds(data map[string]any) *time.Time {
	keys := []string{"lastRefreshAt", "lastRefresh"}
	for _, k := range keys {
		if s, ok := data[k].(string); ok && s != "" {
			if tt, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return &tt
			}
		}
	}
	if psd, ok := data["providerSpecificData"].(map[string]any); ok {
		for _, k := range keys {
			if s, ok := psd[k].(string); ok && s != "" {
				if tt, err := time.Parse(time.RFC3339Nano, s); err == nil {
					return &tt
				}
			}
		}
	}
	return nil
}

// isRefreshStaleBeyond mirrors isCodexRefreshStale: true when lastRefreshAt is
// missing or older than maxAge. Used by providers declaring maxRefreshAgeMs
// (codex) to proactively refresh a token whose access token is not yet near
// expiry but whose refresh token may have aged out.
func isRefreshStaleBeyond(data map[string]any, maxAge time.Duration, now time.Time) bool {
	last := lastRefreshAtFromCreds(data)
	if last == nil {
		return true
	}
	return now.Sub(*last) >= maxAge
}

// ShouldRefreshCredentials mirrors the JS shouldRefreshCredentials: refresh
// proactively when (a) the access token expires within the provider's
// refresh lead, or (b) the provider declares maxRefreshAgeMs and the last
// refresh is older than that. data is the connection's JSON data blob view
// (accessToken/expiresAt/refreshToken/lastRefreshAt/...). providerID selects
// the per-provider policy (refreshLeadMs, maxRefreshAgeMs).
//
// A connection with no refresh token is never proactively refreshed (there
// is nothing to exchange); reactive 401/403 handling (Fix 2d) covers that
// path. now allows tests to inject a clock; production passes time.Now().
func ShouldRefreshCredentials(providerID string, data map[string]any, now time.Time) bool {
	if data == nil {
		return false
	}
	if rt, _ := data["refreshToken"].(string); rt == "" {
		// No refresh token → cannot proactively refresh.
	} else if exp := expiryMsFromCreds(data); exp != nil && exp.Sub(now) < refreshLeadMs(providerID) {
		return true
	}
	pol := oauthPolicyFor(providerID)
	if pol.maxRefreshAgeMs > 0 {
		if rt, _ := data["refreshToken"].(string); rt != "" {
			if isRefreshStaleBeyond(data, time.Duration(pol.maxRefreshAgeMs)*time.Millisecond, now) {
				return true
			}
		}
	}
	return false
}

// MergeRefreshedCredentials mirrors the JS mergeRefreshedCredentials: build a
// flat-field patch that overwrites the refreshed token fields while
// preserving everything else (non-token fields, providerSpecificData merge).
// current is the connection's existing data blob; refreshed is the
// TokenRefresher result. Returns nil when refreshed is nil (nothing to merge).
//
// The returned patch is a map[string]any suitable for
// ConnectionRepo.ApplyConnectionPatch (Fix 2e/2d persist it). Fields the
// upstream did not rotate are taken from current (refreshToken, idToken).
// expiresIn is converted to an expiresAt ISO timestamp; projectId and
// providerSpecificData are merged shallowly.
func MergeRefreshedCredentials(providerID string, current map[string]any, refreshed *RefreshedCredentials, now time.Time) map[string]any {
	if refreshed == nil {
		return nil
	}
	if refreshed.Unrecoverable {
		// Mirror JS: return the unrecoverable marker so the caller marks the
		// connection as needing re-auth rather than persisting partial creds.
		return map[string]any{"__unrecoverable": true}
	}
	next := map[string]any{}
	if refreshed.AccessToken != "" {
		next["accessToken"] = refreshed.AccessToken
	}
	if refreshed.APIKey != "" {
		next["apiKey"] = refreshed.APIKey
	}
	if refreshed.Token != "" {
		next["token"] = refreshed.Token
	}
	// refreshToken: refreshed value, else preserve current.
	if refreshed.RefreshToken != "" {
		next["refreshToken"] = refreshed.RefreshToken
	} else if current != nil {
		if rt, ok := current["refreshToken"].(string); ok && rt != "" {
			next["refreshToken"] = rt
		}
	}
	// idToken: refreshed, else preserve current.
	if refreshed.IDToken != "" {
		next["idToken"] = refreshed.IDToken
	} else if current != nil {
		if idt, ok := current["idToken"].(string); ok && idt != "" {
			next["idToken"] = idt
		}
	}
	if refreshed.ExpiresIn > 0 {
		next["expiresIn"] = refreshed.ExpiresIn
		next["expiresAt"] = now.Add(time.Duration(refreshed.ExpiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
	} else if refreshed.ExpiresAt != "" {
		next["expiresAt"] = refreshed.ExpiresAt
	}
	if refreshed.ProjectID != "" {
		next["projectId"] = refreshed.ProjectID
	}
	if refreshed.CopilotToken != "" {
		next["copilotToken"] = refreshed.CopilotToken
	}
	if refreshed.CopilotTokenExpiresAt != "" {
		next["copilotTokenExpiresAt"] = refreshed.CopilotTokenExpiresAt
	}
	if refreshed.ProviderSpecificData != nil {
		existing := map[string]any{}
		if current != nil {
			if psd, ok := current["providerSpecificData"].(map[string]any); ok {
				existing = psd
			}
		}
		merged := map[string]any{}
		for k, v := range existing {
			merged[k] = v
		}
		for k, v := range refreshed.ProviderSpecificData {
			merged[k] = v
		}
		next["providerSpecificData"] = merged
	}
	// lastRefreshAt: stamped when the provider tracks refresh staleness, or
	// whenever any token field was rotated. Mirrors the JS condition.
	pol := oauthPolicyFor(providerID)
	if pol.trackRefreshAt || next["accessToken"] != nil || next["apiKey"] != nil || next["token"] != nil || next["refreshToken"] != nil || next["copilotToken"] != nil {
		next["lastRefreshAt"] = now.UTC().Format(time.RFC3339Nano)
	}
	return next
}

// UnrecoverableRefreshPatch reports whether a merged patch carries the
// unrecoverable marker (refresh token permanently invalid → re-auth needed).
// Callers check this before persisting and mark the connection unavailable.
func UnrecoverableRefreshPatch(patch map[string]any) bool {
	_, ok := patch["__unrecoverable"]
	return ok
}

// CredentialsForRefresh builds the provider.Credentials the TokenRefresher
// needs from a connection's data blob: AccessToken + ProviderSpecificData
// (refreshToken, clientId, clientSecret, region, authMethod, ...). It is the
// bridge the chat-path refresh hook (Fix 2d) and the proactive refresh call
// site use to hand the refresher a credential view without reconstructing
// the full connection record.
func CredentialsForRefresh(data map[string]any) provider.Credentials {
	creds := provider.Credentials{
		ProviderSpecificData: map[string]any{},
	}
	if data == nil {
		return creds
	}
	if at, ok := data["accessToken"].(string); ok {
		creds.AccessToken = at
	}
	if t, ok := data["token"].(string); ok && creds.AccessToken == "" {
		creds.AccessToken = t
	}
	for k, v := range data {
		switch k {
		case "accessToken", "token":
			continue
		default:
			creds.ProviderSpecificData[k] = v
		}
	}
	if exp := expiryMsFromCreds(data); exp != nil {
		creds.ExpiresAt = exp
	}
	return creds
}