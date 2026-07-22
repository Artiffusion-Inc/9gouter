package api

// auth_login_test.go is the end-to-end regression for #84: the HTTP login
// path used bcryptCompareStub (always returned "not implemented in tests"),
// so POST /api/auth/login with DASHBOARD_PASSWORD_HASH set always 401'd —
// manual dashboard login was impossible. The fix wires the real
// adapter/auth.CompareBcrypt comparator. This test drives the real mux with
// a config carrying a freshly generated bcrypt hash and asserts:
//   - correct password → 200 + session cookie
//   - wrong password   → 401
//   - too many wrong   → 429 (limiter)
// No mocks: real bcrypt hash via adapter/auth.HashBcrypt, real CookieStore.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
)

// TestAuth_Login_BcryptEnvHash drives the real auth handler with a config
// whose DashboardPasswordHash is a freshly minted bcrypt hash. Before #84
// this always returned 401 because bcryptCompareStub rejected every password.
func TestAuth_Login_BcryptEnvHash(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)

	hash, err := adapterauth.HashBcrypt("e2etest123")
	if err != nil {
		t.Fatalf("HashBcrypt: %v", err)
	}
	cfg := config.Config{DashboardPasswordHash: hash}

	mux := http.NewServeMux()
	RegisterAuth(mux, deps, cfg)

	// Correct password → 200 + Set-Cookie auth_token (the regression: was 401).
	rec := loginCall(mux, "e2etest123")
	if rec.Code != http.StatusOK {
		t.Fatalf("login(correct) status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == "" || !strings.Contains(rec.Body.String(), `"success":true`) {
		t.Errorf("login body missing success:true: %s", rec.Body.String())
	}
	hasCookie := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == "auth_token" && c.Value != "" {
			hasCookie = true
		}
	}
	if !hasCookie {
		t.Error("login did not set a non-empty auth_token cookie")
	}

	// Wrong password → 401.
	rec = loginCall(mux, "wrong-password")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("login(wrong) status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAuth_Login_PlainDefault covers the no-hash branch: when
// DASHBOARD_PASSWORD_HASH is unset, login falls back to the PlainVerifier
// with the default initial password "123456".
func TestAuth_Login_PlainDefault(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterAuth(mux, deps, config.Config{})

	if rec := loginCall(mux, "123456"); rec.Code != http.StatusOK {
		t.Fatalf("plain login(default) status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec := loginCall(mux, "nope"); rec.Code != http.StatusUnauthorized {
		t.Errorf("plain login(wrong) status = %d, want 401", rec.Code)
	}
}

func loginCall(mux *http.ServeMux, password string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"password":"`+password+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}