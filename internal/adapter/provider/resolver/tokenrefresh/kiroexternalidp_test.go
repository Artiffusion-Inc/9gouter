package tokenrefresh

// kiroexternalidp_test.go ports the unit tests for src/lib/oauth/kiroExternalIdp.js
// (decolua/9router a4f44e3e): the Microsoft token-endpoint allowlist, scope
// normalization, JWT payload decoding, expiresAt resolution, the
// CLIProxyAPI auth normalization (import shape), and the refresh-param
// builder. Pure-logic tests — no HTTP — matching the JS unit suite.

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestValidateMicrosoftTokenEndpoint(t *testing.T) {
	cases := []struct {
		endpoint string
		wantErr  bool
	}{
		{"https://login.microsoftonline.com/tenant/oauth2/v2.0/token", false},
		{"https://login.microsoft.com/tenant/oauth2/v2.0/token", false},
		{"https://login.windows.net/tenant/oauth2/v2.0/token", false},
		{"  https://login.microsoftonline.com/t/oauth2/v2.0/token  ", false}, // trimmed
		{"http://login.microsoftonline.com/t", true},                         // not https
		{"https://evil.example.com/token", true},                             // not allowlisted
		{"", true},                                                           // empty
		{"not a url", true},                                                  // unparseable
		{"https://login.microsoftonline.com.evil.com/t", true},               // host spoof
	}
	for _, c := range cases {
		_, err := ValidateMicrosoftTokenEndpoint(c.endpoint)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateMicrosoftTokenEndpoint(%q) err=%v wantErr=%v", c.endpoint, err, c.wantErr)
		}
	}
}

func TestNormalizeScope(t *testing.T) {
	if got := NormalizeScope("a b  c "); got != "a b  c" {
		t.Errorf("string scope = %q", got)
	}
	if got := NormalizeScope([]any{"a", " b ", "", "c"}); got != "a b c" {
		t.Errorf("array scope = %q", got)
	}
	if got := NormalizeScope([]string{"a", "b"}); got != "a b" {
		t.Errorf("[]string scope = %q", got)
	}
	if got := NormalizeScope(123); got != "" {
		t.Errorf("non-string scope = %q, want empty", got)
	}
	if got := NormalizeScope(""); got != "" {
		t.Errorf("empty scope = %q", got)
	}
}

func TestBuildExternalIDPRefreshParams(t *testing.T) {
	req, err := buildExternalIDPRefreshParams("rt", map[string]any{
		"clientId":      "cid",
		"tokenEndpoint": "https://login.microsoftonline.com/t/oauth2/v2.0/token",
		"scope":         "offline_access",
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(req.body, "grant_type=refresh_token") {
		t.Errorf("body missing grant_type: %q", req.body)
	}
	if !strings.Contains(req.body, "client_id=cid") {
		t.Errorf("body missing client_id: %q", req.body)
	}
	if !strings.Contains(req.body, "refresh_token=rt") {
		t.Errorf("body missing refresh_token: %q", req.body)
	}
	if !strings.Contains(req.body, "scope=offline_access") {
		t.Errorf("body missing scope: %q", req.body)
	}
	if req.providerSpecificData["authMethod"] != "external_idp" {
		t.Errorf("psd authMethod = %v", req.providerSpecificData["authMethod"])
	}
}

func TestBuildExternalIDPRefreshParams_SnakeCase(t *testing.T) {
	// CLIProxyAPI JSON uses snake_case; the builder reads both.
	req, err := buildExternalIDPRefreshParams("rt", map[string]any{
		"client_id":      "cid",
		"token_endpoint": "https://login.microsoft.com/t/oauth2/v2.0/token",
		"scopes":         "offline_access",
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(req.body, "client_id=cid") {
		t.Errorf("body missing client_id: %q", req.body)
	}
}

func TestBuildExternalIDPRefreshParams_Errors(t *testing.T) {
	ep := "https://login.microsoftonline.com/t/oauth2/v2.0/token"
	cases := []struct {
		name   string
		psd    map[string]any
		rt     string
		errMsg string
	}{
		{"empty refresh token", map[string]any{"clientId": "c", "tokenEndpoint": ep, "scope": "s"}, "", "refresh token is required"},
		{"missing clientId", map[string]any{"tokenEndpoint": ep, "scope": "s"}, "rt", "clientId is required"},
		{"missing scope", map[string]any{"clientId": "c", "tokenEndpoint": ep}, "rt", "scope is required"},
		{"bad endpoint", map[string]any{"clientId": "c", "tokenEndpoint": "https://evil.example.com", "scope": "s"}, "rt", "Microsoft login endpoint"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := buildExternalIDPRefreshParams(c.rt, c.psd)
			if err == nil || !strings.Contains(err.Error(), c.errMsg) {
				t.Errorf("err = %v, want containing %q", err, c.errMsg)
			}
		})
	}
}

func TestDecodeJWTPayload(t *testing.T) {
	// Build a real 3-part JWT with a payload {email, exp}.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"u@example.com","exp":1700000000}`))
	jwt := header + "." + payload + ".sig"
	got := DecodeJWTPayload(jwt)
	if got == nil {
		t.Fatal("nil payload")
	}
	if got["email"] != "u@example.com" {
		t.Errorf("email = %v", got["email"])
	}
	if got["exp"] != float64(1700000000) {
		t.Errorf("exp = %v", got["exp"])
	}
	if DecodeJWTPayload("not.a.jwt") != nil {
		t.Error("expected nil for malformed")
	}
	if DecodeJWTPayload("") != nil {
		t.Error("expected nil for empty")
	}
	if DecodeJWTPayload("onlyone") != nil {
		t.Error("expected nil for non-3-part")
	}
}

func TestResolveExternalIDPExpiresAt(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	// Explicit RFC3339 wins.
	if got := ResolveExternalIDPExpiresAt(map[string]any{"expired": "2027-01-01T00:00:00Z"}, now); got != "2027-01-01T00:00:00Z" {
		t.Errorf("explicit expired = %q", got)
	}
	// expires_in seconds.
	if got := ResolveExternalIDPExpiresAt(map[string]any{"expires_in": float64(3600)}, now); got != "2026-07-24T13:00:00Z" {
		t.Errorf("expires_in = %q", got)
	}
	// string expires_in.
	if got := ResolveExternalIDPExpiresAt(map[string]any{"expires_in": "7200"}, now); got != "2026-07-24T14:00:00Z" {
		t.Errorf("expires_in string = %q", got)
	}
	// JWT exp claim.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1750000000}`))
	at := header + "." + payload + ".sig"
	if got := ResolveExternalIDPExpiresAt(map[string]any{"access_token": at}, now); got != "2025-06-15T15:06:40Z" {
		t.Errorf("jwt exp = %q", got)
	}
	// Default 3600s.
	if got := ResolveExternalIDPExpiresAt(map[string]any{}, now); got != "2026-07-24T13:00:00Z" {
		t.Errorf("default = %q", got)
	}
}

func TestNormalizeKiroExternalIDPAuth(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	// Build a JWT with email + exp for the email-extraction + exp path.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"joe@contoso.com","exp":1750000000}`))
	at := header + "." + payload + ".sig"

	auth, err := NormalizeKiroExternalIDPAuth(map[string]any{
		"auth_method":    "external_idp",
		"access_token":   at,
		"refresh_token":  "rt",
		"client_id":      "cid",
		"token_endpoint": "https://login.microsoftonline.com/t/oauth2/v2.0/token",
		"profile_arn":    "arn:aws:codewhisperer:us-east-1:123:profile/P",
		"scopes":         []any{"offline_access", " https://api/.default "},
		"region":         "eu-west-1",
	}, now)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if auth.AccessToken != at {
		t.Errorf("accessToken mismatch")
	}
	if auth.RefreshToken != "rt" {
		t.Errorf("refreshToken = %q", auth.RefreshToken)
	}
	if auth.Email != "joe@contoso.com" {
		t.Errorf("email = %q, want from JWT", auth.Email)
	}
	if auth.ExpiresAt != "2025-06-15T15:06:40Z" {
		t.Errorf("expiresAt = %q (jwt exp path)", auth.ExpiresAt)
	}
	if auth.ProviderSpecificData["profileArn"] != "arn:aws:codewhisperer:us-east-1:123:profile/P" {
		t.Errorf("psd profileArn = %v", auth.ProviderSpecificData["profileArn"])
	}
	if auth.ProviderSpecificData["region"] != "eu-west-1" {
		t.Errorf("psd region = %v", auth.ProviderSpecificData["region"])
	}
	if auth.ProviderSpecificData["authMethod"] != "external_idp" {
		t.Errorf("psd authMethod = %v", auth.ProviderSpecificData["authMethod"])
	}
	if auth.ProviderSpecificData["scope"] != "offline_access https://api/.default" {
		t.Errorf("psd scope = %v", auth.ProviderSpecificData["scope"])
	}
}

func TestNormalizeKiroExternalIDPAuth_JSONString(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	raw := `{"access_token":"at","refresh_token":"rt","client_id":"cid","token_endpoint":"https://login.microsoft.com/t","profile_arn":"p","scopes":"offline_access","email":"x@y.com","expires_in":1800}`
	auth, err := NormalizeKiroExternalIDPAuth(raw, now)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if auth.Email != "x@y.com" {
		t.Errorf("email = %q", auth.Email)
	}
	if auth.ExpiresAt != "2026-07-24T12:30:00Z" {
		t.Errorf("expiresAt = %q (expires_in path)", auth.ExpiresAt)
	}
}

func TestNormalizeKiroExternalIDPAuth_DefaultRegion(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	auth, err := NormalizeKiroExternalIDPAuth(map[string]any{
		"access_token":   "at",
		"refresh_token":  "rt",
		"client_id":      "cid",
		"token_endpoint": "https://login.windows.net/t",
		"profile_arn":    "p",
		"scopes":         "offline_access",
	}, now)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if auth.ProviderSpecificData["region"] != kiroExternalIDPDefaultRegion {
		t.Errorf("region = %v, want default us-east-1", auth.ProviderSpecificData["region"])
	}
}

func TestNormalizeKiroExternalIDPAuth_Errors(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	valid := map[string]any{
		"access_token":   "at",
		"refresh_token":  "rt",
		"client_id":      "cid",
		"token_endpoint": "https://login.microsoftonline.com/t",
		"profile_arn":    "p",
		"scopes":         "offline_access",
	}
	cases := []struct {
		name   string
		input  any
		errMsg string
	}{
		{"nil", nil, "is required"},
		{"empty string", "", "is required"},
		{"bad json string", "{not json", "is invalid"},
		{"wrong authMethod", map[string]any{"auth_method": "api_key"}, "Only external_idp"},
		{"missing access_token", func() map[string]any { v := copyMap(valid); delete(v, "access_token"); return v }(), "access_token is required"},
		{"missing refresh_token", func() map[string]any { v := copyMap(valid); delete(v, "refresh_token"); return v }(), "refresh_token is required"},
		{"missing client_id", func() map[string]any { v := copyMap(valid); delete(v, "client_id"); return v }(), "client_id is required"},
		{"missing scopes", func() map[string]any { v := copyMap(valid); delete(v, "scopes"); return v }(), "scopes is required"},
		{"missing profile_arn", func() map[string]any { v := copyMap(valid); delete(v, "profile_arn"); return v }(), "profile_arn is required"},
		{"bad endpoint", func() map[string]any { v := copyMap(valid); v["token_endpoint"] = "https://evil.example.com"; return v }(), "Microsoft login endpoint"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NormalizeKiroExternalIDPAuth(c.input, now)
			if err == nil || !strings.Contains(err.Error(), c.errMsg) {
				t.Errorf("err = %v, want containing %q", err, c.errMsg)
			}
		})
	}
}

func copyMap(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Silence unused-import guards for isolated compilation.
var _ = json.Marshal
