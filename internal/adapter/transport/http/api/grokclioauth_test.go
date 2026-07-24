package api

// grokclioauth_test.go ports the device-code OAuth flow + the #2546 expiresAt
// fix (decolua/9router 7dfb3466) for grok-cli. The device-code / token-poll
// handlers are exercised E2E against real httptest.Server upstreams (auth.x.ai
// + cli-chat-proxy) via a host-swap transport — no mock executor, real
// ConnectionRepo on sqlite. The mapTokens / expiresAt / JWT-email helpers are
// pure-logic unit tests.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
)

// grokCliHostSwapTransport rewrites every request's host+scheme to the test
// server, so the production auth.x.ai / cli-chat-proxy.grok.com endpoints are
// redirected to the in-process httptest server. The request path is preserved
// so the handler can branch on /oauth2/device/code vs /oauth2/token vs /v1/user.
type grokCliHostSwapTransport struct{ to *url.URL }

func (t grokCliHostSwapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = t.to.Scheme
	req.URL.Host = t.to.Host
	req.Host = t.to.Host
	return http.DefaultTransport.RoundTrip(req)
}

func grokCliSwapClient(srv *httptest.Server) *http.Client {
	u, _ := url.Parse(srv.URL)
	return &http.Client{Transport: grokCliHostSwapTransport{to: u}, Timeout: 10 * time.Second}
}

// makeJWT builds a 3-part JWT with the given payload.
func makeJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	pb, _ := json.Marshal(payload)
	body := base64.RawURLEncoding.EncodeToString(pb)
	return header + "." + body + ".sig"
}

func TestGrokCliExpiresAt(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if got := grokCliExpiresAt(3600, now); got != "2026-07-24T13:00:00Z" {
		t.Errorf("grokCliExpiresAt(3600) = %v, want 2026-07-24T13:00:00Z", got)
	}
	if got := grokCliExpiresAt(0, now); got != nil {
		t.Errorf("grokCliExpiresAt(0) = %v, want nil (no expires_in)", got)
	}
	if got := grokCliExpiresAt(-5, now); got != nil {
		t.Errorf("grokCliExpiresAt(-5) = %v, want nil", got)
	}
}

func TestDecodeXaiIDTokenEmail(t *testing.T) {
	jwt := makeJWT(t, map[string]any{"email": "joe@x.ai"})
	if got := decodeXaiIDTokenEmail(jwt); got != "joe@x.ai" {
		t.Errorf("id_token email = %q", got)
	}
	jwt2 := makeJWT(t, map[string]any{"preferred_username": "alt@x.ai"})
	if got := decodeXaiIDTokenEmail(jwt2); got != "alt@x.ai" {
		t.Errorf("preferred_username = %q", got)
	}
	if decodeXaiIDTokenEmail("not.a.jwt") != "" {
		t.Error("expected empty for malformed")
	}
	if decodeXaiIDTokenEmail("") != "" {
		t.Error("expected empty for empty")
	}
}

func TestExtractEmailFromAccessToken(t *testing.T) {
	jwt := makeJWT(t, map[string]any{"email": "access@x.ai"})
	if got := extractEmailFromAccessToken(jwt); got != "access@x.ai" {
		t.Errorf("access token email = %q", got)
	}
	if extractEmailFromAccessToken("garbage") != "" {
		t.Error("expected empty for non-jwt")
	}
}

func TestGrokCliMapTokens_SurfacesExpiresAt(t *testing.T) {
	// The #2546 regression: mapTokens must surface expiresAt computed from
	// expires_in so the proactive ShouldRefreshCredentials path fires.
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	idTok := makeJWT(t, map[string]any{"email": "joe@x.ai", "exp": float64(1750000000)})
	tok := grokCliTokenResponse{
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresIn:    3600,
		IDToken:      idTok,
		Scope:        grokCliScope,
	}
	mapped := grokCliMapTokens(tok, nil, now)
	if mapped.Email != "joe@x.ai" {
		t.Errorf("email = %q, want joe@x.ai (from id_token)", mapped.Email)
	}
	if ea, _ := mapped.Data["expiresAt"].(string); ea != "2026-07-24T13:00:00Z" {
		t.Errorf("expiresAt = %v, want 2026-07-24T13:00:00Z (#2546 fix)", ea)
	}
	if ei, _ := mapped.Data["expiresIn"].(int); ei != 3600 {
		t.Errorf("expiresIn = %v", ei)
	}
	psd, _ := mapped.Data["providerSpecificData"].(map[string]any)
	if psd["authMethod"] != "device_code" {
		t.Errorf("psd authMethod = %v", psd["authMethod"])
	}
	if psd["idToken"] != idTok {
		t.Errorf("psd idToken missing")
	}
	if psd["email"] != "joe@x.ai" {
		t.Errorf("psd email = %v", psd["email"])
	}
}

func TestGrokCliMapTokens_NoExpiresIn(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	tok := grokCliTokenResponse{AccessToken: "at", RefreshToken: "rt"}
	mapped := grokCliMapTokens(tok, nil, now)
	if mapped.Data["expiresAt"] != nil {
		t.Errorf("expiresAt = %v, want nil when expires_in absent", mapped.Data["expiresAt"])
	}
}

func TestGrokCliMapTokens_EmailFromUser(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	tok := grokCliTokenResponse{AccessToken: "at", RefreshToken: "rt", ExpiresIn: 3600}
	hasAccess := true
	user := &grokCliUserResponse{
		Email:             "from-profile@x.ai",
		UserID:            "u-123",
		FirstName:         "Joe",
		LastName:          "Xai",
		HasGrokCodeAccess: &hasAccess,
		SubscriptionTier:  "pro",
	}
	mapped := grokCliMapTokens(tok, user, now)
	if mapped.Email != "from-profile@x.ai" {
		t.Errorf("email = %q, want from user profile", mapped.Email)
	}
	if dn, _ := mapped.Data["displayName"].(string); dn != "Joe Xai" {
		t.Errorf("displayName = %q, want 'Joe Xai'", dn)
	}
	psd, _ := mapped.Data["providerSpecificData"].(map[string]any)
	if psd["userId"] != "u-123" {
		t.Errorf("psd userId = %v", psd["userId"])
	}
	if psd["hasGrokCodeAccess"] != true {
		t.Errorf("psd hasGrokCodeAccess = %v", psd["hasGrokCodeAccess"])
	}
	if psd["subscriptionTier"] != "pro" {
		t.Errorf("psd subscriptionTier = %v", psd["subscriptionTier"])
	}
}

func TestGrokCliDeviceCode_E2E(t *testing.T) {
	var gotPath string
	var gotCT string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dc-123",
			"user_code":        "ABCD-EFGH",
			"verification_uri": "https://x.ai/device",
			"expires_in":       900,
			"interval":         5,
		})
	}))
	defer srv.Close()
	prev := grokCliHTTPClient
	grokCliHTTPClient = grokCliSwapClient(srv)
	t.Cleanup(func() { grokCliHTTPClient = prev })

	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest(http.MethodPost, "/api/oauth/grok-cli/device-code", strings.NewReader(""))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/oauth2/device/code" {
		t.Errorf("upstream path = %q, want /oauth2/device/code", gotPath)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	parsed, _ := url.ParseQuery(gotBody)
	if parsed.Get("client_id") != grokCliClientID {
		t.Errorf("client_id = %q", parsed.Get("client_id"))
	}
	if parsed.Get("scope") != grokCliScope {
		t.Errorf("scope = %q", parsed.Get("scope"))
	}
	if parsed.Get("referrer") != grokCliReferrer {
		t.Errorf("referrer = %q", parsed.Get("referrer"))
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["deviceCode"] != "dc-123" {
		t.Errorf("deviceCode = %v", resp["deviceCode"])
	}
	if resp["userCode"] != "ABCD-EFGH" {
		t.Errorf("userCode = %v", resp["userCode"])
	}
}

func TestGrokCliPoll_E2E_Success(t *testing.T) {
	// Token endpoint returns a token; user endpoint returns a profile. The poll
	// handler maps tokens (#2546 expiresAt), persists a grok-cli connection.
	idTok := makeJWT(t, map[string]any{"email": "poll@x.ai"})
	var tokenHits, userHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			tokenHits++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at-poll",
				"refresh_token": "rt-poll",
				"expires_in":    7200,
				"id_token":      idTok,
				"scope":         grokCliScope,
			})
		case "/v1/user":
			userHits++
			hasAccess := true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"email":             "profile@x.ai",
				"userId":            "u-999",
				"firstName":         "Poll",
				"hasGrokCodeAccess": hasAccess,
				"subscriptionTier":  "pro",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	prev := grokCliHTTPClient
	grokCliHTTPClient = grokCliSwapClient(srv)
	t.Cleanup(func() { grokCliHTTPClient = prev })

	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest(http.MethodPost, "/api/oauth/grok-cli/poll", strings.NewReader(`{"deviceCode":"dc-123"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if tokenHits != 1 {
		t.Errorf("token endpoint hits = %d, want 1", tokenHits)
	}
	if userHits != 1 {
		t.Errorf("user endpoint hits = %d, want 1", userHits)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["success"] != true {
		t.Errorf("success = %v", resp["success"])
	}
	if resp["pending"] != false {
		t.Errorf("pending = %v", resp["pending"])
	}
	conn, _ := resp["connection"].(map[string]any)
	connID, _ := conn["id"].(string)
	if connID == "" {
		t.Fatal("missing connection.id")
	}
	// The connection is persisted with expiresAt (#2546) — verify by reading it back.
	got, err := deps.Connections.GetByID(context.Background(), connID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Provider != "grok-cli" {
		t.Errorf("provider = %q", got.Provider)
	}
	var data map[string]any
	_ = json.Unmarshal(got.Data, &data)
	if ea, _ := data["expiresAt"].(string); ea == "" {
		t.Error("persisted connection missing expiresAt (#2546 regression)")
	}
	if ei, _ := data["expiresIn"].(float64); int(ei) != 7200 {
		t.Errorf("expiresIn = %v, want 7200", ei)
	}
	// email from id_token wins over user profile.
	if email, _ := data["email"].(string); email != "poll@x.ai" {
		t.Errorf("email = %q, want poll@x.ai (id_token wins)", email)
	}
	psd, _ := data["providerSpecificData"].(map[string]any)
	if psd["authMethod"] != "device_code" {
		t.Errorf("psd authMethod = %v", psd["authMethod"])
	}
	if psd["userId"] != "u-999" {
		t.Errorf("psd userId = %v", psd["userId"])
	}
}

func TestGrokCliPoll_E2E_Pending(t *testing.T) {
	// authorization_pending returns 200 with pending=true so the dashboard
	// keeps polling — NOT a 4xx error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "authorization_pending",
		})
	}))
	defer srv.Close()
	prev := grokCliHTTPClient
	grokCliHTTPClient = grokCliSwapClient(srv)
	t.Cleanup(func() { grokCliHTTPClient = prev })

	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest(http.MethodPost, "/api/oauth/grok-cli/poll", strings.NewReader(`{"deviceCode":"dc-pending"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (pending is not an error), body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["pending"] != true {
		t.Errorf("pending = %v, want true", resp["pending"])
	}
	if resp["success"] != false {
		t.Errorf("success = %v, want false while pending", resp["success"])
	}
}

func TestGrokCliPoll_E2E_MissingDeviceCode(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest(http.MethodPost, "/api/oauth/grok-cli/poll", strings.NewReader(`{}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestGrokCliPoll_E2E_SlowDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
	}))
	defer srv.Close()
	prev := grokCliHTTPClient
	grokCliHTTPClient = grokCliSwapClient(srv)
	t.Cleanup(func() { grokCliHTTPClient = prev })

	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest(http.MethodPost, "/api/oauth/grok-cli/poll", strings.NewReader(`{"deviceCode":"dc-slow"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (slow_down is pending), body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["pending"] != true {
		t.Errorf("pending = %v, want true for slow_down", resp["pending"])
	}
}

// Silence unused-import guards for isolated compilation.
var _ = adapterauth.NewCookieStore
