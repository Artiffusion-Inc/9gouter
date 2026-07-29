package api

// kimioauth_test.go exercises the Kimi Code device-code OAuth flow E2E against
// an in-process httptest.Server (auth.kimi.com) via the same host-swap
// transport pattern as grokclioauth_test.go. The mapTokens unit test pins the
// deviceId roundtrip into providerSpecificData — the kimiHeaders hook + the
// KimiRefresher both key on X-Msh-Device-Id, so the device id the device-code
// session minted must survive the import.

import (
	"context"
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

// kimiHostSwapTransport rewrites every request's host+scheme to the test server
// so the production auth.kimi.com endpoints hit the in-process server. The path
// is preserved so the handler can branch on /api/oauth/device_authorization vs
// /api/oauth/token.
type kimiHostSwapTransport struct{ to *url.URL }

func (t kimiHostSwapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = t.to.Scheme
	req.URL.Host = t.to.Host
	req.Host = t.to.Host
	return http.DefaultTransport.RoundTrip(req)
}

func kimiSwapClient(srv *httptest.Server) *http.Client {
	u, _ := url.Parse(srv.URL)
	return &http.Client{Transport: kimiHostSwapTransport{to: u}, Timeout: 10 * time.Second}
}

func TestKimiMapTokens_DeviceIdRoundtrip(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tok := kimiTokenResponse{
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresIn:    3600,
	}
	mapped := kimiMapTokens(tok, "device-abc", now)
	if at, _ := mapped.Data["accessToken"].(string); at != "at" {
		t.Errorf("accessToken = %q", at)
	}
	if ea, _ := mapped.Data["expiresAt"].(string); ea != "2026-07-29T13:00:00Z" {
		t.Errorf("expiresAt = %v, want 2026-07-29T13:00:00Z", ea)
	}
	psd, _ := mapped.Data["providerSpecificData"].(map[string]any)
	if psd["authMethod"] != "device_code" {
		t.Errorf("psd authMethod = %v", psd["authMethod"])
	}
	if psd["deviceId"] != "device-abc" {
		t.Errorf("psd deviceId = %v, want device-abc (must survive import)", psd["deviceId"])
	}
}

func TestKimiMapTokens_NoExpiresIn(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tok := kimiTokenResponse{AccessToken: "at", RefreshToken: "rt"}
	mapped := kimiMapTokens(tok, "d", now)
	if mapped.Data["expiresAt"] != nil {
		t.Errorf("expiresAt = %v, want nil when expires_in absent", mapped.Data["expiresAt"])
	}
}

func TestKimiDeviceCode_E2E(t *testing.T) {
	var gotPath, gotCT, gotBody string
	var gotMshDeviceID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotMshDeviceID = r.Header.Get("X-Msh-Device-Id")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "dc-kimi",
			"user_code":                 "KIMI-CODE",
			"verification_uri":          "https://www.kimi.com/code/authorize_device",
			"verification_uri_complete": "https://www.kimi.com/code/authorize_device?user_code=KIMI-CODE",
			"expires_in":                600,
			"interval":                  5,
		})
	}))
	defer srv.Close()
	prev := kimiHTTPClient
	kimiHTTPClient = kimiSwapClient(srv)
	t.Cleanup(func() { kimiHTTPClient = prev })

	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest(http.MethodPost, "/api/oauth/kimi/device-code", strings.NewReader(""))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/api/oauth/device_authorization" {
		t.Errorf("upstream path = %q, want /api/oauth/device_authorization", gotPath)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	// X-Msh-Device-Id must be emitted by BuildKimiHeaders so the device fingerprint
	// is consistent across the device-code request and the later token poll.
	if gotMshDeviceID == "" {
		t.Error("X-Msh-Device-Id header missing from device-code request")
	}
	parsed, _ := url.ParseQuery(gotBody)
	if parsed.Get("client_id") != kimiKimiClientID {
		t.Errorf("client_id = %q", parsed.Get("client_id"))
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["deviceCode"] != "dc-kimi" {
		t.Errorf("deviceCode = %v", resp["deviceCode"])
	}
	if resp["userCode"] != "KIMI-CODE" {
		t.Errorf("userCode = %v", resp["userCode"])
	}
	if resp["_kimiDeviceId"] == "" || resp["_kimiDeviceId"] == nil {
		t.Errorf("_kimiDeviceId = %v, want a non-empty uuid for the poll roundtrip", resp["_kimiDeviceId"])
	}
}

func TestKimiPoll_E2E_Success(t *testing.T) {
	var tokenHits int
	var gotDeviceID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/oauth/token" {
			tokenHits++
			gotDeviceID = r.Header.Get("X-Msh-Device-Id")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at-kimi",
				"refresh_token": "rt-kimi",
				"expires_in":    7200,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	prev := kimiHTTPClient
	kimiHTTPClient = kimiSwapClient(srv)
	t.Cleanup(func() { kimiHTTPClient = prev })

	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	body := `{"deviceCode":"dc-kimi","_kimiDeviceId":"device-echoed"}`
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/kimi/poll", strings.NewReader(body))
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
	// The poll must re-emit the device id echoed from the device-code response so
	// the X-Msh-* fingerprint matches the original device-code request.
	if gotDeviceID != "device-echoed" {
		t.Errorf("X-Msh-Device-Id on token poll = %q, want device-echoed", gotDeviceID)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["success"] != true {
		t.Errorf("success = %v", resp["success"])
	}
	conn, _ := resp["connection"].(map[string]any)
	connID, _ := conn["id"].(string)
	if connID == "" {
		t.Fatal("missing connection.id")
	}
	got, err := deps.Connections.GetByID(context.Background(), connID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Provider != "kimi" {
		t.Errorf("provider = %q", got.Provider)
	}
	var data map[string]any
	_ = json.Unmarshal(got.Data, &data)
	psd, _ := data["providerSpecificData"].(map[string]any)
	if psd["deviceId"] != "device-echoed" {
		t.Errorf("persisted psd deviceId = %v, want device-echoed", psd["deviceId"])
	}
	if psd["authMethod"] != "device_code" {
		t.Errorf("persisted psd authMethod = %v", psd["authMethod"])
	}
}

func TestKimiPoll_E2E_Pending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
	}))
	defer srv.Close()
	prev := kimiHTTPClient
	kimiHTTPClient = kimiSwapClient(srv)
	t.Cleanup(func() { kimiHTTPClient = prev })

	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest(http.MethodPost, "/api/oauth/kimi/poll", strings.NewReader(`{"deviceCode":"dc-pending"}`))
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

func TestKimiPoll_E2E_MissingDeviceCode(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest(http.MethodPost, "/api/oauth/kimi/poll", strings.NewReader(`{}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// Silence unused-import guards for isolated compilation.
var _ = adapterauth.NewCookieStore
