package api

// kimioauth.go ports the Kimi Code device-code OAuth flow from
// decolua/9router src/lib/oauth/providers.js ("kimi" entry: flowType
// "device_code", requestDeviceCode / pollToken / mapTokens). The Go build had
// no kimi import endpoint: registry.go's kimi comment explicitly noted "the
// dashboard device-code surface is not yet ported to Go" — tokens could be
// imported out-of-band but not acquired through the dashboard.
//
// This adds the two device-code endpoints the dashboard device-code modal
// calls, mirroring the grok-cli device-code port (grokclioauth.go) and the
// upstream providers.js "kimi" entry 1:1:
//
//	POST /api/oauth/kimi/device-code  → request a device code from
//	  auth.kimi.com (returns device_code, user_code, verification_uri,
//	  expires_in, interval) and carries the per-session _kimiDeviceId.
//	POST /api/oauth/kimi/poll          → poll for the token; on success
//	  mapTokens + persist a kimi ProviderConnection carrying the stable
//	  deviceId in providerSpecificData (the kimiHeaders hook + the token
//	  refresher both key on it, so it must survive the import).
//
// Constants mirror open-sse/providers/registry/kimi.js (oauth block) 1:1; the
// X-Msh-* headers reuse defaultexec.BuildKimiHeaders so refresh + chat share
// the exact same device fingerprint.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	defaultexec "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/default"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// Kimi OAuth constants — copied verbatim from the upstream registry kimi.js
// oauth block (commit 68566f53). Public client id; no secret. deviceCodeURL is
// only used by the dashboard device-code surface; the token refresher
// (tokenrefresh/kimi.go) holds the token/refresh URL + client id separately.
const (
	kimiDeviceCodeURL      = "https://auth.kimi.com/api/oauth/device_authorization"
	kimiKimiClientID       = "17e5f671-d194-4dfb-9706-5516cb48c098"
	kimiAuthorizeDeviceURL = "https://www.kimi.com/code/authorize_device"
	// kimiHTTPTimeout bounds the device-code request + token poll. The dashboard
	// polls /poll from the browser, so each server call is short.
	kimiHTTPTimeout = 20 * time.Second
)

// kimiDeviceCodeResponse is the auth.kimi.com /api/oauth/device_authorization
// response, returned verbatim (plus the per-session _kimiDeviceId) to the
// dashboard device-code modal.
type kimiDeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// kimiTokenResponse is the auth.kimi.com /api/oauth/token response. Kimi
// returns 200 for pending states with an `error` field (authorization_pending
// / slow_down) — the poll handler surfaces those as pending, not failures.
type kimiTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// kimiHTTPClient is the client used for the device-code + token calls. Package
// var so tests can swap in an httptest.Server-aware transport.
var kimiHTTPClient = &http.Client{Timeout: kimiHTTPTimeout}

// kimiDeviceCode implements POST /api/oauth/kimi/device-code: POST a
// form-encoded {client_id} to auth.kimi.com with the X-Msh-* device headers,
// mint a per-session deviceId, and return the device code for the dashboard to
// display + poll. Mirrors requestDeviceCode in providers.js.
func (h *oauthHandler) kimiDeviceCode(w http.ResponseWriter, r *http.Request) {
	deviceID := newKimiUUID()

	form := url.Values{}
	form.Set("client_id", kimiKimiClientID)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, kimiDeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	defaultexec.BuildKimiHeaders(req.Header, deviceID)

	resp, err := kimiHTTPClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Kimi device code request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		writeError(w, resp.StatusCode, fmt.Sprintf("Kimi device code request failed: %s", string(body)))
		return
	}
	var dc kimiDeviceCodeResponse
	if err := json.Unmarshal(body, &dc); err != nil {
		writeError(w, http.StatusBadGateway, "invalid device code response: "+err.Error())
		return
	}

	// Mirrors providers.js: verification_uri falls back to authorizeDeviceUrl;
	// verification_uri_complete falls back to authorizeDeviceUrl?user_code=...
	verificationURI := dc.VerificationURI
	if verificationURI == "" {
		verificationURI = kimiAuthorizeDeviceURL
	}
	verificationURIComplete := dc.VerificationURIComplete
	if verificationURIComplete == "" && dc.UserCode != "" {
		verificationURIComplete = fmt.Sprintf("%s?user_code=%s", kimiAuthorizeDeviceURL, dc.UserCode)
	}
	interval := dc.Interval
	if interval == 0 {
		interval = 5
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":                 true,
		"deviceCode":              dc.DeviceCode,
		"userCode":                dc.UserCode,
		"verificationUri":         verificationURI,
		"verificationUriComplete": verificationURIComplete,
		"expiresIn":               dc.ExpiresIn,
		"interval":                interval,
		// _kimiDeviceId rides alongside the device code so the poll call can
		// re-emit the same X-Msh-* fingerprint; the upstream JS passes it via
		// extraData, here it is returned for the dashboard to echo back.
		"_kimiDeviceId": deviceID,
	})
}

// kimiPoll implements POST /api/oauth/kimi/poll: poll auth.kimi.com for the
// device-code token grant. authorization_pending / slow_down return 200 with
// pending=true (the dashboard keeps polling); a successful token response runs
// mapTokens + persists a kimi connection carrying the stable deviceId, and
// returns the connection id. Mirrors pollToken + the import sequence in
// providers.js.
func (h *oauthHandler) kimiPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceCode   string `json:"deviceCode"`
		KimiDeviceID string `json:"_kimiDeviceId"`
	}
	if err := parseJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if strings.TrimSpace(body.DeviceCode) == "" {
		writeError(w, http.StatusBadRequest, "deviceCode is required")
		return
	}
	deviceID := strings.TrimSpace(body.KimiDeviceID)
	if deviceID == "" {
		// No echoed device id → mint a fresh one so the X-Msh-* set is non-empty;
		// the connection will still persist it for later refresh/chat reuse.
		deviceID = newKimiUUID()
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("client_id", kimiKimiClientID)
	form.Set("device_code", body.DeviceCode)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, kimiTokenURLForOAuth, strings.NewReader(form.Encode()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	defaultexec.BuildKimiHeaders(req.Header, deviceID)

	resp, err := kimiHTTPClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Kimi token poll failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	var tok kimiTokenResponse
	if err := json.Unmarshal(respBody, &tok); err != nil {
		writeError(w, http.StatusBadGateway, "invalid token response: "+err.Error())
		return
	}

	// CLIProxyAPI: Kimi returns 200 for pending states with an error field.
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

	mapped := kimiMapTokens(tok, deviceID, time.Now().UTC())

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
		ID:        fmt.Sprintf("kimi-%d", now.UnixNano()),
		Provider:  "kimi",
		AuthType:  "oauth",
		Priority:  0,
		IsActive:  true,
		Data:      dataJSON,
		CreatedAt: now,
		UpdatedAt: now,
	}
	resolved, err := h.deps.Connections.Create(r.Context(), conn)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"pending":    false,
		"connection": map[string]any{"id": resolved.ID, "provider": conn.Provider},
	})
}

// kimiMappedTokens is the result of kimiMapTokens — the connection data blob to
// persist. Mirrors the mapTokens return object.
type kimiMappedTokens struct {
	Data map[string]any
}

// kimiMapTokens mirrors mapTokens in providers.js: builds the connection data
// blob from the token response. The deviceId is stored in
// providerSpecificData so the kimiHeaders hook (chat path) and the KimiRefresher
// (refresh path) reuse the same device fingerprint the device-code session
// minted — Kimi's CLIProxyAPI parity keys the X-Msh-Device-Id on it.
func kimiMapTokens(tok kimiTokenResponse, deviceID string, now time.Time) kimiMappedTokens {
	psd := map[string]any{
		"authMethod": "device_code",
	}
	if deviceID != "" {
		psd["deviceId"] = deviceID
	}

	data := map[string]any{
		"accessToken":          tok.AccessToken,
		"refreshToken":         tok.RefreshToken,
		"expiresIn":            tok.ExpiresIn,
		"providerSpecificData": psd,
	}
	if tok.ExpiresIn > 0 {
		data["expiresAt"] = now.Add(time.Duration(tok.ExpiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	return kimiMappedTokens{Data: data}
}

// kimiTokenURLForOAuth is the auth.kimi.com token endpoint, the same host the
// token refresher uses. Defined here (not imported from tokenrefresh) to keep
// the dashboard device-code surface self-contained and avoid pulling the
// resolver package's transitive deps into the api package.
const kimiTokenURLForOAuth = "https://auth.kimi.com/api/oauth/token"

// newKimiUUID mints a fresh per-session device id. crypto/rand is used so the
// id is unique per device-code request; mirrors crypto.randomUUID() in the JS.
func newKimiUUID() string {
	return uuid.NewString()
}
