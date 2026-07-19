// Package tokenrefresh ports the per-provider OAuth/token-refresh functions
// from open-sse/services/tokenRefresh/providers.js. Each provider has its own
// refresh protocol; this package implements them one at a time.
//
// kiro is ported (kiro.go). This file ports the shared machinery used by the
// remaining providers: the HTTP helpers, the OAuth2 form-encoded generic
// refresh (mirroring JS refreshAccessToken), and classifyOAuthRefreshError.
// Per-provider refreshers that diverge from the generic flow live in their
// own files (claude.go, google.go, codex.go, ...).
//
// NOT YET PORTED: vertex (RS256 service-account JWT — needs go-jose). It
// returns ErrVertexNotPorted so callers fall back to the static catalog
// instead of silently doing the wrong thing.
package tokenrefresh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/resolver"
)

// ErrVertexNotPorted signals vertex refresh (RS256 JWT) is not yet ported.
var ErrVertexNotPorted = fmt.Errorf("vertex token refresh not yet ported (T027 follow-up: needs RS256 JWT)")

// defaultRefreshTimeout bounds every refresh HTTP call, mirroring the JS
// fetch defaults (no explicit timeout there, but the proxy stack applies one).
const defaultRefreshTimeout = 30 * time.Second

// newRefreshClient returns an *http.Client bounded by defaultRefreshTimeout.
// Tests inject their own client (via the httpClient field on each refresher)
// so they can point at an httptest.Server without dialing the real network.
func newRefreshClient() *http.Client {
	return &http.Client{Timeout: defaultRefreshTimeout}
}

// tokenResponse is the common shape returned by most OAuth2 token endpoints:
// {access_token, refresh_token, expires_in, id_token?, resource_url?}. Fields
// the provider does not return are simply left zero.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	IDToken      string `json:"id_token"`
	// Qwen returns resource_url at the top level (used to derive the base URL
	// for the actual chat endpoint on subsequent calls).
	ResourceURL string `json:"resource_url"`
}

// doForm posts a form-encoded body with the given headers and decodes a
// tokenResponse. A non-2xx response is classified and returned as an error.
func doForm(ctx context.Context, client *http.Client, endpoint string, form url.Values, headers http.Header, log resolver.Logger, label string) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	return doRequest(client, req, log, label)
}

// doJSON posts a JSON body and decodes a tokenResponse.
func doJSON(ctx context.Context, client *http.Client, endpoint string, body any, headers http.Header, log resolver.Logger, label string) (*tokenResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	return doRequest(client, req, log, label)
}

// doRequest executes the refresh request, classifies failures, and decodes the
// response into a tokenResponse. A non-2xx response is an error (caller falls
// back to the static catalog / marks unrecoverable).
func doRequest(client *http.Client, req *http.Request, log resolver.Logger, label string) (*tokenResponse, error) {
	if log == nil {
		log = resolver.NopLogger()
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Warn("token refresh network error", "label", label, "error", err)
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		log.Warn("token refresh failed", "label", label, "status", resp.StatusCode, "body", string(respBody))
		return nil, fmt.Errorf("%s refresh %d: %s", label, resp.StatusCode, string(respBody))
	}
	var tok tokenResponse
	if err := json.Unmarshal(respBody, &tok); err != nil {
		log.Warn("token refresh decode error", "label", label, "error", err)
		return nil, err
	}
	log.Info("token refreshed", "label", label)
	return &tok, nil
}

// fromToken builds a RefreshedCredentials from a tokenResponse, preserving
// the original refreshToken when the upstream did not rotate it.
func fromToken(tok *tokenResponse, originalRefreshToken string, includeID bool) *resolver.RefreshedCredentials {
	out := &resolver.RefreshedCredentials{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresIn:    tok.ExpiresIn,
	}
	if includeID {
		out.IDToken = tok.IDToken
	}
	if out.RefreshToken == "" {
		out.RefreshToken = originalRefreshToken
	}
	return out
}

// OAuthError classifies a refresh failure the same way the JS
// classifyOAuthRefreshError does. Permanent means the refresh token itself is
// bad (expired/reused/invalidated/invalid_grant) and re-auth is required.
type OAuthError struct {
	Status     int
	Code       string
	Description string
	Permanent  bool
}

func (e *OAuthError) Error() string {
	return fmt.Sprintf("oauth refresh %d: %s %s", e.Status, e.Code, e.Description)
}

// classifyOAuthRefreshError mirrors open-sse/services/tokenRefresh/providers.js.
// It parses an upstream error body (JSON or plain) and reports whether the
// failure is permanent (refresh token unusable).
func classifyOAuthRefreshError(errorText string, status int) OAuthError {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(errorText), &parsed); err != nil {
		parsed = nil
	}
	code := ""
	desc := ""
	if parsed != nil {
		if e, ok := parsed["error"].(map[string]any); ok {
			if c, ok := e["code"].(string); ok {
				code = c
			}
			if c, ok := e["message"].(string); ok && desc == "" {
				desc = c
			}
		}
		if c, ok := parsed["error"].(string); ok && code == "" {
			code = c
		}
		if c, ok := parsed["error_code"].(string); ok && code == "" {
			code = c
		}
		if c, ok := parsed["error_description"].(string); ok {
			desc = c
		}
		if c, ok := parsed["message"].(string); ok && desc == "" {
			desc = c
		}
	}
	if desc == "" {
		desc = errorText
	}
	combined := strings.ToLower(code + " " + desc)
	permanent := false
	for _, marker := range []string{
		"refresh_token_expired",
		"refresh_token_reused",
		"refresh_token_invalidated",
		"invalid_grant",
	} {
		if strings.Contains(combined, marker) {
			permanent = true
			break
		}
	}
	return OAuthError{Status: status, Code: code, Description: desc, Permanent: permanent}
}

// readLimit reads up to n bytes from r. A read error is swallowed (the caller
// already has the response status and can act on an empty body).
func readLimit(r io.Reader, n int64) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, n))
	return b
}

// jsonUnmarshal is a thin wrapper kept for symmetry with readLimit so call
// sites read as a pair; it just delegates to encoding/json.
func jsonUnmarshal(b []byte, dst any) error { return json.Unmarshal(b, dst) }

// jsonMarshal is the matching wrapper for json.Unmarshal.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }