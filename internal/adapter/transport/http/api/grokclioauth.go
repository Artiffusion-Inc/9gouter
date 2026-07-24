package api

// grokclioauth.go ports the grok-cli device-code OAuth flow from
// decolua/9router src/lib/oauth/providers.js ("grok-cli" entry: flowType
// "device_code", requestDeviceCode / pollToken / postExchange / mapTokens)
// plus the #2546 fix (7dfb3466): mapTokens now surfaces an absolute
// expiresAt computed from expires_in so the proactive ShouldRefreshCredentials
// path fires before the xAI token silently expires ~40-45 min after login.
//
// The Go build had no grok-cli import endpoint at all (oauth.go's generic
// providerAction returns a stub). This adds the two device-code endpoints the
// dashboard device-code modal calls:
//
//	POST /api/oauth/grok-cli/device-code  → request device code (returns
//	  {device_code, user_code, verification_uri, expires_in, interval})
//	POST /api/oauth/grok-cli/poll          → poll for the token; on success
//	  mapTokens + persist a grok-cli ProviderConnection carrying expiresAt.
//
// Constants mirror open-sse/providers/registry/grok-cli.js (oauth block) and
// src/lib/oauth/constants/xai.js 1:1.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// grok-cli OAuth constants (open-sse/providers/registry/grok-cli.js oauth).
const (
	grokCliClientID      = "b1a00492-073a-47ea-816f-4c329264a828"
	grokCliDeviceCodeURL = "https://auth.x.ai/oauth2/device/code"
	grokCliTokenURL      = "https://auth.x.ai/oauth2/token"
	grokCliScope         = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write"
	grokCliReferrer      = "grok-build"
	grokCliUserURL       = "https://cli-chat-proxy.grok.com/v1/user"
	grokCliUserAgent     = "grok-pager/0.2.93 grok-shell/0.2.93 (linux; x86_64)"
	grokCliClientVersion = "0.2.93"
	// grokCliHTTPTimeout bounds the device-code request + token poll + user
	// profile fetch. The dashboard polls the /poll endpoint from the browser,
	// so each server call is short.
	grokCliHTTPTimeout = 20 * time.Second
)

// grokCliDeviceCodeResponse is the auth.x.ai /oauth2/device/code response,
// returned verbatim to the dashboard device-code modal.
type grokCliDeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// grokCliTokenResponse is the auth.x.ai /oauth2/token response.
type grokCliTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	IDToken      string `json:"id_token"`
	Scope        string `json:"scope"`
	// Error fields used by the device-code polling protocol.
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// grokCliUserResponse is the best-effort cli-chat-proxy /v1/user profile.
type grokCliUserResponse struct {
	Email             string `json:"email"`
	UserID            string `json:"userId"`
	PrincipalID       string `json:"principalId"`
	FirstName         string `json:"firstName"`
	LastName          string `json:"lastName"`
	HasGrokCodeAccess *bool  `json:"hasGrokCodeAccess"`
	SubscriptionTier  any    `json:"subscriptionTier"`
}

// grokCliHTTPClient is the client used for the device-code/token/user calls.
// Package var so tests can swap in an httptest.Server-aware transport.
var grokCliHTTPClient = &http.Client{Timeout: grokCliHTTPTimeout}

// grokCliDeviceCode implements POST /api/oauth/grok-cli/device-code: POST a
// form-encoded {client_id, scope, referrer} to auth.x.ai and return the device
// code for the dashboard to display + poll. Mirrors requestDeviceCode in
// providers.js.
func (h *oauthHandler) grokCliDeviceCode(w http.ResponseWriter, r *http.Request) {
	form := url.Values{}
	form.Set("client_id", grokCliClientID)
	form.Set("scope", grokCliScope)
	if grokCliReferrer != "" {
		form.Set("referrer", grokCliReferrer)
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, grokCliDeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", grokCliUserAgent)

	resp, err := grokCliHTTPClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Grok CLI device code request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		writeError(w, resp.StatusCode, fmt.Sprintf("Grok CLI device code request failed: %s", string(body)))
		return
	}
	var dc grokCliDeviceCodeResponse
	if err := json.Unmarshal(body, &dc); err != nil {
		writeError(w, http.StatusBadGateway, "invalid device code response: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":         true,
		"deviceCode":      dc.DeviceCode,
		"userCode":        dc.UserCode,
		"verificationUri": dc.VerificationURI,
		"expiresIn":       dc.ExpiresIn,
		"interval":        dc.Interval,
	})
}

// grokCliPoll implements POST /api/oauth/grok-cli/poll: poll auth.x.ai for the
// device-code token grant. authorization_pending / slow_down return 200 with
// pending=true (the dashboard keeps polling); a successful token response runs
// mapTokens + postExchange, persists a grok-cli connection carrying expiresAt,
// and returns the connection id. Mirrors pollToken + the import sequence in
// providers.js. The #2546 fix is mapTokens surfacing expiresAt.
func (h *oauthHandler) grokCliPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceCode string `json:"deviceCode"`
	}
	if err := parseJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if strings.TrimSpace(body.DeviceCode) == "" {
		writeError(w, http.StatusBadRequest, "deviceCode is required")
		return
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("device_code", body.DeviceCode)
	form.Set("client_id", grokCliClientID)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, grokCliTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", grokCliUserAgent)

	resp, err := grokCliHTTPClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Grok CLI token poll failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	var tok grokCliTokenResponse
	if err := json.Unmarshal(respBody, &tok); err != nil {
		writeError(w, http.StatusBadGateway, "invalid token response: "+err.Error())
		return
	}

	// Device-code protocol: pending / slow_down are expected while the user
	// authorizes — return 200 with pending=true so the dashboard keeps polling.
	if tok.Error == "authorization_pending" || tok.Error == "slow_down" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"pending": true,
			"error":   tok.Error,
		})
		return
	}
	if tok.Error != "" {
		writeError(w, http.StatusBadRequest, tok.Error+": "+tok.ErrorDescription)
		return
	}
	if tok.AccessToken == "" {
		writeError(w, http.StatusBadGateway, "token response missing access_token")
		return
	}

	// postExchange: best-effort user profile from cli-chat-proxy (non-fatal).
	user := grokCliFetchUser(r.Context(), tok.AccessToken)

	mapped := grokCliMapTokens(tok, user, time.Now().UTC())

	if h.deps.Connections == nil {
		writeError(w, http.StatusServiceUnavailable, "Connections repo unavailable")
		return
	}
	dataJSON, err := json.Marshal(mapped.Data)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode connection data: "+err.Error())
		return
	}
	now := time.Now().UTC()
	conn := settings.ProviderConnection{
		ID:        fmt.Sprintf("grok-cli-%d", now.UnixNano()),
		Provider:  "grok-cli",
		AuthType:  "oauth",
		Name:      mapped.Email,
		Email:     mapped.Email,
		Priority:  0,
		IsActive:  true,
		Data:      dataJSON,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := h.deps.Connections.Create(r.Context(), conn); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"pending":    false,
		"connection": map[string]any{"id": conn.ID, "provider": conn.Provider, "email": conn.Email},
	})
}

// grokCliMappedTokens is the result of grokCliMapTokens — the connection data
// blob to persist. Mirrors the mapTokens return object.
type grokCliMappedTokens struct {
	Email string
	Data  map[string]any
}

// grokCliMapTokens mirrors mapTokens in providers.js: builds the connection
// data blob from the token response + best-effort user profile. The #2546 fix
// (7dfb3466) is the expiresAt field computed from expires_in — without it the
// proactive ShouldRefreshCredentials path (which only reads expiresAt /
// tokenExpiresAt) never fires and only the reactive 401 path refreshes, so the
// xAI token silently expires ~40-45 min after login.
//
// email is resolved from the id_token (decodeXaiIdTokenEmail), then the access
// token JWT, then the user profile. providerSpecificData mirrors identity
// (authMethod=device_code, idToken, email, userId, hasGrokCodeAccess,
// subscriptionTier) so the grok-cli executor can set x-email / x-userid without
// depending on the top-level credential shape.
func grokCliMapTokens(tok grokCliTokenResponse, user *grokCliUserResponse, now time.Time) grokCliMappedTokens {
	email := decodeXaiIDTokenEmail(tok.IDToken)
	if email == "" {
		email = extractEmailFromAccessToken(tok.AccessToken)
	}
	if email == "" && user != nil {
		email = user.Email
	}

	var userID string
	var displayName string
	if user != nil {
		if user.UserID != "" {
			userID = user.UserID
		} else if user.PrincipalID != "" {
			userID = user.PrincipalID
		}
		displayName = strings.TrimSpace(strings.Join([]string{user.FirstName, user.LastName}, " "))
	}

	psd := map[string]any{
		"authMethod": "device_code",
	}
	if tok.IDToken != "" {
		psd["idToken"] = tok.IDToken
	}
	if email != "" {
		psd["email"] = email
	}
	if userID != "" {
		psd["userId"] = userID
	}
	if user != nil && user.HasGrokCodeAccess != nil {
		psd["hasGrokCodeAccess"] = *user.HasGrokCodeAccess
	}
	if user != nil && user.SubscriptionTier != nil {
		psd["subscriptionTier"] = user.SubscriptionTier
	}

	data := map[string]any{
		"accessToken":  tok.AccessToken,
		"refreshToken": tok.RefreshToken,
		"expiresIn":    tok.ExpiresIn,
		"scope":        tok.Scope,
		// #2546: surface an absolute expiry so the proactive refresh path
		// (ShouldRefreshCredentials) fires before the token silently expires.
		// expiresAt is nil when the upstream omitted expires_in (the proactive
		// path then falls back to maxRefreshAge staleness, never silently).
		"expiresAt":            grokCliExpiresAt(tok.ExpiresIn, now),
		"providerSpecificData": psd,
	}
	if email != "" {
		data["email"] = email
	}
	if displayName != "" {
		data["displayName"] = displayName
	}
	return grokCliMappedTokens{Email: email, Data: data}
}

// grokCliExpiresAt mirrors the #2546 expiresAt computation: now + expires_in
// seconds as an RFC3339 timestamp, or nil when expires_in is absent/zero.
func grokCliExpiresAt(expiresIn int, now time.Time) any {
	if expiresIn <= 0 {
		return nil
	}
	return now.Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
}

// grokCliFetchUser mirrors postExchange: a best-effort GET of the cli-chat-proxy
// /v1/user profile with the grok-cli fingerprint headers. Returns nil on any
// failure (the import still succeeds without a profile).
func grokCliFetchUser(ctx context.Context, accessToken string) *grokCliUserResponse {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, grokCliUserURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", grokCliUserAgent)
	req.Header.Set("x-xai-token-auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", grokCliClientVersion)

	resp, err := grokCliHTTPClient.Do(req)
	if err != nil || resp == nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var u grokCliUserResponse
	if err := json.Unmarshal(body, &u); err != nil {
		return nil
	}
	return &u
}

// decodeXaiIDTokenEmail mirrors decodeXaiIdTokenEmail: best-effort base64url
// decode of the id_token payload and extract the email / preferred_username /
// upn claim. Returns "" on any malformation.
func decodeXaiIDTokenEmail(idToken string) string {
	payload := decodeJWTPayloadGeneric(idToken)
	if payload == nil {
		return ""
	}
	for _, k := range []string{"email", "preferred_username", "upn"} {
		if v, ok := payload[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// extractEmailFromAccessToken mirrors extractEmailFromAccessToken: decode the
// access token JWT payload (xAI access tokens are JWTs) and extract the email
// claim. Returns "" on any malformation.
func extractEmailFromAccessToken(accessToken string) string {
	payload := decodeJWTPayloadGeneric(accessToken)
	if payload == nil {
		return ""
	}
	for _, k := range []string{"email", "preferred_username", "upn"} {
		if v, ok := payload[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// decodeJWTPayloadGeneric decodes the middle segment of a JWT (base64url) into a
// map. Returns nil on any malformation.
func decodeJWTPayloadGeneric(jwt string) map[string]any {
	if jwt == "" {
		return nil
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil
	}
	seg := parts[1]
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	dec, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		dec, err = base64.StdEncoding.DecodeString(seg)
		if err != nil {
			return nil
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(dec, &payload); err != nil {
		return nil
	}
	return payload
}
