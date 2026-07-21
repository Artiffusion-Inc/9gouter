package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	domainauth "github.com/Artiffusion-Inc/9gouter/internal/domain/auth"
)

// TestAuth_LoginLogoutStatus exercises the password login flow with the default
// (no-hash) verifier, the logout surface, and the /api/auth/status response
// shape.
func TestAuth_LoginLogoutStatus(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterAuth(mux, deps, config.Config{})
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Status before login — public route; authenticated=false.
	req := httptest.NewRequest("GET", "/api/auth/status", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status (pre-login) = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var st map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if st["authenticated"] != false {
		t.Fatalf("pre-login authenticated = %v, want false", st["authenticated"])
	}
	if st["hasPassword"] != false {
		t.Fatalf("pre-login hasPassword = %v, want false (fresh DB has no password set)", st["hasPassword"])
	}

	// Login with default password "123456".
	req = httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"password":"123456"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var loginResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("unmarshal login: %v", err)
	}
	if loginResp["success"] != true {
		t.Fatalf("login success = %v, want true; body=%s", loginResp["success"], rec.Body.String())
	}
	// Cookie should be set.
	var sessionCookie string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "auth_token" {
			sessionCookie = c.Value
		}
	}
	if sessionCookie == "" {
		t.Fatal("login did not set auth_token cookie")
	}

	// Status with the session cookie — authenticated=true.
	req = httptest.NewRequest("GET", "/api/auth/status", nil)
	req.Header.Set("Cookie", "auth_token="+sessionCookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status (post-login) = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal status post-login: %v", err)
	}
	if st["authenticated"] != true {
		t.Fatalf("post-login authenticated = %v, want true", st["authenticated"])
	}

	// Login with bad password → 401.
	req = httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"password":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad login status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}

	// Login with invalid body → 400.
	req = httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{bad`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid login body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Logout — always 200.
	req = httptest.NewRequest("POST", "/api/auth/logout", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAuth_ResetPassword verifies the resetPassword handler clears the stored
// password and returns success.
func TestAuth_ResetPassword(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterAuth(mux, deps, config.Config{})
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest("POST", "/api/auth/reset-password", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resetPassword status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal resetPassword: %v", err)
	}
	if resp["success"] != true {
		t.Fatalf("resetPassword success = %v, want true", resp["success"])
	}
}

// TestAuth_OidcStartNotConfigured verifies the oidcStart handler returns 503
// when OIDC is not configured (no issuer/clientID/secret in settings).
func TestAuth_OidcStartNotConfigured(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterAuth(mux, deps, config.Config{})

	// GET (the form the dashboard navigates to).
	req := httptest.NewRequest("GET", "/api/auth/oidc/start", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("oidcStart GET status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}

	// POST same.
	req = httptest.NewRequest("POST", "/api/auth/oidc/start", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("oidcStart POST status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAuth_OidcTestMissingFields verifies the oidcTest handler returns 400 when
// issuer/clientID are absent (both from request and settings).
func TestAuth_OidcTestMissingFields(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterAuth(mux, deps, config.Config{})

	req := httptest.NewRequest("POST", "/api/auth/oidc/test", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oidcTest missing status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAuth_OidcTestBadIssuer verifies the oidcTest handler returns 502 when
// the issuer URL is unreachable (OIDC discovery fails).
func TestAuth_OidcTestBadIssuer(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterAuth(mux, deps, config.Config{})

	body := `{"issuerUrl":"http://127.0.0.1:1/invalid-oidc","clientId":"c","scopes":"openid"}`
	req := httptest.NewRequest("POST", "/api/auth/oidc/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("oidcTest bad issuer status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAuth_OidcCallback verifies the stubbed OIDC callback returns 200.
func TestAuth_OidcCallback(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterAuth(mux, deps, config.Config{})

	req := httptest.NewRequest("POST", "/api/auth/oidc/callback", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("oidcCallback status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAuth_HelperFunctions exercises the unexported helpers displayName,
// clientIP, and nullableString directly.
func TestAuth_HelperFunctions(t *testing.T) {
	// displayName.
	if displayName(nil) != "" {
		t.Fatalf("displayName(nil) = %q, want empty", displayName(nil))
	}
	sess := &domainauth.Session{}
	sess.Principal.Name = "Alice"
	if got := displayName(sess); got != "Alice" {
		t.Fatalf("displayName(Alice) = %q, want Alice", got)
	}
	sess.Principal.Name = ""
	sess.Principal.Email = "alice@example.com"
	if got := displayName(sess); got != "alice@example.com" {
		t.Fatalf("displayName(email) = %q, want alice@example.com", got)
	}
	sess.Principal.Email = ""
	if got := displayName(sess); got != "Password user" {
		t.Fatalf("displayName(empty) = %q, want Password user", got)
	}

	// nullableString.
	if nullableString("") != nil {
		t.Fatalf("nullableString(\"\") = %v, want nil", nullableString(""))
	}
	if nullableString("x") != "x" {
		t.Fatalf("nullableString(\"x\") = %v, want x", nullableString("x"))
	}

	// clientIP — X-Forwarded-For wins.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	if got := clientIP(req); got != "10.0.0.1" {
		t.Fatalf("clientIP XFF = %q, want 10.0.0.1", got)
	}
	req.Header.Del("X-Forwarded-For")
	req.Header.Set("X-Real-Ip", "10.0.0.2")
	if got := clientIP(req); got != "10.0.0.2" {
		t.Fatalf("clientIP X-Real-Ip = %q, want 10.0.0.2", got)
	}
	req.Header.Del("X-Real-Ip")
	req.RemoteAddr = "10.0.0.3:1234"
	if got := clientIP(req); got != "10.0.0.3:1234" {
		t.Fatalf("clientIP RemoteAddr = %q, want 10.0.0.3:1234", got)
	}

	// headerRequestAdapter nil guard.
	a := headerRequestAdapter{Request: nil}
	if a.Header("X-Anything") != "" {
		t.Fatalf("nil adapter Header = %q, want empty", a.Header("X-Anything"))
	}
}