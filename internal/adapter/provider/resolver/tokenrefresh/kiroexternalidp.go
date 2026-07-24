package tokenrefresh

// kiroexternalidp.go ports src/lib/oauth/kiroExternalIdp.js from
// decolua/9router a4f44e3e: the Microsoft Entra/365 SSO (external_idp)
// refresh-parameter builder + token-endpoint allowlist. Kiro accounts
// authenticated via CLIProxyAPI against Microsoft Entra refresh with a
// form-encoded OAuth2 refresh_token grant against a Microsoft login
// endpoint — never an arbitrary URL (the allowlist blocks token
// exfiltration via a crafted token_endpoint in the imported JSON).
//
// Two helpers are exported for callers (the kiro refresher + a future
// dashboard import handler): ValidateMicrosoftTokenEndpoint (allowlist) and
// NormalizeKiroExternalIDPAuth (the CLIProxyAPI JSON → connection import
// shape). buildExternalIDPRefreshParams is used by the refresher.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// microsoftTokenEndpointHosts is the allowlist of Microsoft login hosts a
// CLIProxyAPI-imported token_endpoint may target. Mirrors
// MICROSOFT_TOKEN_ENDPOINT_HOSTS in kiroExternalIdp.js.
var microsoftTokenEndpointHosts = map[string]bool{
	"login.microsoftonline.com": true,
	"login.microsoft.com":       true,
	"login.windows.net":         true,
}

// kiroExternalIDPDefaultRegion mirrors DEFAULT_REGION ("us-east-1").
const kiroExternalIDPDefaultRegion = "us-east-1"

// ValidateMicrosoftTokenEndpoint mirrors validateMicrosoftTokenEndpoint: the
// endpoint must be a non-empty https URL whose host is on the Microsoft login
// allowlist. Returns the normalized URL string.
func ValidateMicrosoftTokenEndpoint(rawEndpoint string) (string, error) {
	endpoint := strings.TrimSpace(rawEndpoint)
	if endpoint == "" {
		return "", errors.New("token_endpoint is required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", errors.New("token_endpoint must be a valid URL")
	}
	if parsed.Scheme != "https" {
		return "", errors.New("token_endpoint must use https")
	}
	host := strings.ToLower(parsed.Hostname())
	if !microsoftTokenEndpointHosts[host] {
		return "", errors.New("token_endpoint must be a Microsoft login endpoint")
	}
	return parsed.String(), nil
}

// NormalizeScope mirrors normalizeScope: an array of scopes is joined with
// spaces (each trimmed, empties dropped); a string scope is trimmed.
func NormalizeScope(scopes any) string {
	switch v := scopes.(type) {
	case []any:
		var parts []string
		for _, s := range v {
			if ss, ok := s.(string); ok {
				if t := strings.TrimSpace(ss); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, " ")
	case []string:
		var parts []string
		for _, s := range v {
			if t := strings.TrimSpace(s); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, " ")
	case string:
		return strings.TrimSpace(v)
	}
	return ""
}

// externalIDPRefreshRequest is the resolved refresh call: the validated token
// endpoint, the form-encoded body, and the normalized ProviderSpecificData to
// carry back into the connection.
type externalIDPRefreshRequest struct {
	tokenEndpoint        string
	body                 string
	providerSpecificData map[string]any
}

// buildExternalIDPRefreshParams mirrors buildExternalIdpRefreshParams: resolves
// clientId / tokenEndpoint / scope from the connection's ProviderSpecificData,
// validates them, and builds the form-encoded refresh_token grant body. The
// returned providerSpecificData re-stamps authMethod=external_idp and the
// normalized clientId/tokenEndpoint/scope so the caller merges a clean patch.
func buildExternalIDPRefreshParams(refreshToken string, psd map[string]any) (*externalIDPRefreshRequest, error) {
	if psd == nil {
		psd = map[string]any{}
	}
	clientID := stringFromPSD(psd, "clientId", "client_id")
	tokenEndpointRaw := stringFromPSD(psd, "tokenEndpoint", "token_endpoint")
	scope := NormalizeScope(psd["scope"])
	if scope == "" {
		scope = NormalizeScope(psd["scopes"])
	}

	if refreshToken == "" {
		return nil, errors.New("refresh token is required")
	}
	if clientID == "" {
		return nil, errors.New("clientId is required for external_idp refresh")
	}
	if scope == "" {
		return nil, errors.New("scope is required for external_idp refresh")
	}
	tokenEndpoint, err := ValidateMicrosoftTokenEndpoint(tokenEndpointRaw)
	if err != nil {
		return nil, err
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", clientID)
	form.Set("refresh_token", refreshToken)
	form.Set("scope", scope)

	out := map[string]any{}
	for k, v := range psd {
		out[k] = v
	}
	out["authMethod"] = "external_idp"
	out["clientId"] = clientID
	out["tokenEndpoint"] = tokenEndpoint
	out["scope"] = scope

	return &externalIDPRefreshRequest{
		tokenEndpoint:        tokenEndpoint,
		body:                 form.Encode(),
		providerSpecificData: out,
	}, nil
}

// stringFromPSD reads a string field from ProviderSpecificData, trying a list
// of keys in order (camelCase then snake_case, mirroring the JS
// `input.access_token || input.accessToken` fallbacks).
func stringFromPSD(psd map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := psd[k].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// DecodeJWTPayload mirrors decodeJwtPayload: best-effort base64url decode of
// the JWT middle segment into a map. Returns nil on any malformation (used to
// extract email / preferred_username / exp from an imported access token).
func DecodeJWTPayload(jwt string) map[string]any {
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

// ResolveExternalIDPExpiresAt mirrors resolveExpiresAt: prefer an explicit
// expired/expires_at/expiresAt RFC3339 timestamp, then expires_in (seconds
// from now), then the JWT exp claim, then the 3600s default. now is injected
// for deterministic tests. Returns an RFC3339 timestamp.
func ResolveExternalIDPExpiresAt(input map[string]any, now time.Time) string {
	const defaultExpiresIn = 3600
	for _, k := range []string{"expired", "expires_at", "expiresAt"} {
		if v, ok := input[k].(string); ok && v != "" {
			if _, err := time.Parse(time.RFC3339, v); err == nil {
				return v
			}
		}
	}
	for _, k := range []string{"expires_in", "expiresIn"} {
		switch v := input[k].(type) {
		case float64:
			if v > 0 {
				return now.Add(time.Duration(v) * time.Second).Format(time.RFC3339)
			}
		case string:
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
				return now.Add(time.Duration(n) * time.Second).Format(time.RFC3339)
			}
		}
	}
	if at, ok := input["access_token"].(string); ok {
		if payload := DecodeJWTPayload(at); payload != nil {
			if exp, ok := payload["exp"].(float64); ok && exp > 0 {
				return time.Unix(int64(exp), 0).UTC().Format(time.RFC3339)
			}
		}
	}
	return now.Add(defaultExpiresIn * time.Second).Format(time.RFC3339)
}

// KiroExternalIDPAuth is the normalized CLIProxyAPI auth shape returned by
// NormalizeKiroExternalIDPAuth — the connection import payload. Mirrors the
// return object of normalizeKiroExternalIdpAuth.
type KiroExternalIDPAuth struct {
	AccessToken          string
	RefreshToken         string
	ExpiresAt            string
	Email                string
	ProviderSpecificData map[string]any
}

// NormalizeKiroExternalIDPAuth mirrors normalizeKiroExternalIdpAuth: parses a
// CLIProxyAPI auth JSON object (or string) into the connection import shape,
// validating that authMethod is external_idp (or empty) and the required
// access_token / refresh_token / client_id / scopes / profile_arn fields are
// present. now is injected for deterministic expiresAt resolution.
func NormalizeKiroExternalIDPAuth(raw any, now time.Time) (*KiroExternalIDPAuth, error) {
	input, ok := raw.(map[string]any)
	if !ok {
		if s, ok := raw.(string); ok && s != "" {
			if err := json.Unmarshal([]byte(s), &input); err != nil {
				return nil, errors.New("CLIProxyAPI auth JSON is invalid")
			}
		} else {
			return nil, errors.New("CLIProxyAPI auth JSON is required")
		}
	}
	if input == nil {
		return nil, errors.New("CLIProxyAPI auth JSON is required")
	}

	authMethod := stringFromPSD(input, "authMethod", "auth_method")
	if authMethod != "" && authMethod != "external_idp" {
		return nil, errors.New("Only external_idp Kiro auth is supported by this importer")
	}

	accessToken := stringFromPSD(input, "access_token", "accessToken")
	refreshToken := stringFromPSD(input, "refresh_token", "refreshToken")
	clientID := stringFromPSD(input, "client_id", "clientId")
	tokenEndpoint, err := ValidateMicrosoftTokenEndpoint(stringFromPSD(input, "token_endpoint", "tokenEndpoint"))
	if err != nil {
		return nil, err
	}
	profileArn := stringFromPSD(input, "profile_arn", "profileArn")
	region := stringFromPSD(input, "region")
	if region == "" {
		region = kiroExternalIDPDefaultRegion
	}
	scope := NormalizeScope(input["scopes"])
	if scope == "" {
		scope = NormalizeScope(input["scope"])
	}

	if accessToken == "" {
		return nil, errors.New("access_token is required")
	}
	if refreshToken == "" {
		return nil, errors.New("refresh_token is required")
	}
	if clientID == "" {
		return nil, errors.New("client_id is required")
	}
	if scope == "" {
		return nil, errors.New("scopes is required")
	}
	if profileArn == "" {
		return nil, errors.New("profile_arn is required")
	}

	email, _ := input["email"].(string)
	if email == "" {
		if payload := DecodeJWTPayload(accessToken); payload != nil {
			for _, k := range []string{"email", "preferred_username", "upn", "sub"} {
				if v, ok := payload[k].(string); ok && v != "" {
					email = v
					break
				}
			}
		}
	}

	return &KiroExternalIDPAuth{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    ResolveExternalIDPExpiresAt(input, now),
		Email:        email,
		ProviderSpecificData: map[string]any{
			"profileArn":    profileArn,
			"region":        region,
			"authMethod":    "external_idp",
			"provider":      "CLIProxyAPI",
			"clientId":      clientID,
			"tokenEndpoint": tokenEndpoint,
			"scope":         scope,
		},
	}, nil
}
