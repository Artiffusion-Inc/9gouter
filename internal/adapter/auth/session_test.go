package auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	domainauth "github.com/Artiffusion-Inc/9gouter/internal/domain/auth"
)

func TestCookieStore_SetGetPrincipal(t *testing.T) {
	store, err := NewCookieStore("a-very-long-test-secret-32bytes")
	if err != nil {
		t.Fatalf("NewCookieStore: %v", err)
	}

	w := httptest.NewRecorder()
	sess := domainauth.Session{
		ID: "sess-1",
		Principal: domainauth.Principal{
			ID:    "user-1",
			Email: "user@example.com",
			Name:  "Test User",
		},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := store.Set(w, sess); err != nil {
		t.Fatalf("Set: %v", err)
	}

	cookies := w.Result().Cookies()
	var authCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == authCookieName {
			authCookie = c
			break
		}
	}
	if authCookie == nil {
		t.Fatal("expected auth_token cookie to be set")
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(authCookie)
	got, err := store.Get(r)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != sess.ID {
		t.Errorf("id mismatch: got %q want %q", got.ID, sess.ID)
	}
	if got.Principal.ID != sess.Principal.ID {
		t.Errorf("principal id mismatch: got %q want %q", got.Principal.ID, sess.Principal.ID)
	}
	if got.Principal.Email != sess.Principal.Email {
		t.Errorf("email mismatch: got %q want %q", got.Principal.Email, sess.Principal.Email)
	}
	if got.Principal.Name != sess.Principal.Name {
		t.Errorf("name mismatch: got %q want %q", got.Principal.Name, sess.Principal.Name)
	}
}

func TestCookieStore_TamperedCookie(t *testing.T) {
	store, err := NewCookieStore("a-very-long-test-secret-32bytes")
	if err != nil {
		t.Fatalf("NewCookieStore: %v", err)
	}

	w := httptest.NewRecorder()
	sess := domainauth.Session{
		ID:        "sess-2",
		Principal: domainauth.Principal{ID: "user-2"},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := store.Set(w, sess); err != nil {
		t.Fatalf("Set: %v", err)
	}

	cookies := w.Result().Cookies()
	var authCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == authCookieName {
			authCookie = c
			break
		}
	}
	if authCookie == nil {
		t.Fatal("expected auth_token cookie")
	}

	// Tamper with the payload: decode, mutate, re-encode, keep original signature.
	parts := strings.Split(authCookie.Value, ".")
	if len(parts) != 2 {
		t.Fatalf("unexpected cookie value format: %q", authCookie.Value)
	}
	plain, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	tampered := strings.Replace(string(plain), "user-2", "user-X", 1)
	tamperedPayload := base64.RawURLEncoding.EncodeToString([]byte(tampered))
	tamperedCookie := *authCookie
	tamperedCookie.Value = tamperedPayload + "." + parts[1]

	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&tamperedCookie)
	_, err = store.Get(r)
	if err == nil {
		t.Fatal("expected error for tampered cookie, got nil")
	}
	if err != ErrInvalidSession {
		t.Fatalf("expected ErrInvalidSession, got %v", err)
	}
}

func TestCookieStore_MissingCookie(t *testing.T) {
	store, err := NewCookieStore("a-very-long-test-secret-32bytes")
	if err != nil {
		t.Fatalf("NewCookieStore: %v", err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	if _, err := store.Get(r); err != ErrInvalidSession {
		t.Fatalf("expected ErrInvalidSession for missing cookie, got %v", err)
	}
}

func TestCookieStore_Clear(t *testing.T) {
	store, err := NewCookieStore("a-very-long-test-secret-32bytes")
	if err != nil {
		t.Fatalf("NewCookieStore: %v", err)
	}
	w := httptest.NewRecorder()
	if err := store.Clear(w); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	cookies := w.Result().Cookies()
	var cleared *http.Cookie
	for _, c := range cookies {
		if c.Name == authCookieName {
			cleared = c
			break
		}
	}
	if cleared == nil {
		t.Fatal("expected clear cookie")
	}
	if cleared.Value != "" {
		t.Errorf("expected empty value, got %q", cleared.Value)
	}
	if cleared.MaxAge >= 0 {
		t.Errorf("expected negative MaxAge, got %d", cleared.MaxAge)
	}
}

func TestCookieStore_WeakSecretRejected(t *testing.T) {
	if _, err := NewCookieStore("short"); err != ErrSecretTooShort {
		t.Fatalf("expected ErrSecretTooShort, got %v", err)
	}
}

func TestCookieStore_Expired(t *testing.T) {
	store, err := NewCookieStore("a-very-long-test-secret-32bytes")
	if err != nil {
		t.Fatalf("NewCookieStore: %v", err)
	}
	w := httptest.NewRecorder()
	sess := domainauth.Session{
		ID:        "sess-expired",
		Principal: domainauth.Principal{ID: "user"},
		ExpiresAt: time.Now().Add(-time.Second),
	}
	if err := store.Set(w, sess); err != nil {
		t.Fatalf("Set: %v", err)
	}
	cookies := w.Result().Cookies()
	r := httptest.NewRequest("GET", "/", nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	if _, err := store.Get(r); err != ErrInvalidSession {
		t.Fatalf("expected ErrInvalidSession for expired session, got %v", err)
	}
}

// findAuthCookie returns the auth_token cookie from a recorder response.
func findAuthCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == authCookieName {
			return c
		}
	}
	t.Fatal("expected auth_token cookie to be set")
	return nil
}

// TestCookieStore_DefaultNotSecure reproduces the bug behind the dashboard
// bouncing to /login behind an HTTPS-terminating proxy: Set() has no
// *http.Request (the store interface threads only the ResponseWriter), so the
// X-Forwarded-Proto auto-detect in secureCookie is unreachable and a store
// built without WithForceSecure emits a non-Secure cookie. Deployments must
// opt in via AUTH_COOKIE_SECURE=true (→ WithForceSecure).
func TestCookieStore_DefaultNotSecure(t *testing.T) {
	store, err := NewCookieStore("a-very-long-test-secret-32bytes")
	if err != nil {
		t.Fatalf("NewCookieStore: %v", err)
	}
	w := httptest.NewRecorder()
	if err := store.Set(w, domainauth.Session{ID: "s", Principal: domainauth.Principal{ID: "u"}, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if c := findAuthCookie(t, w); c.Secure {
		t.Fatalf("default store cookie should NOT be Secure (got Secure=true); Set() has no request context for proto auto-detect")
	}
}

// TestCookieStore_ForceSecure asserts the AUTH_COOKIE_SECURE=true path: with
// WithForceSecure(true) both Set() and Clear() emit the Secure flag, and the
// store built this way round-trips a cookie a browser would keep on HTTPS.
func TestCookieStore_ForceSecure(t *testing.T) {
	base, err := NewCookieStore("a-very-long-test-secret-32bytes")
	if err != nil {
		t.Fatalf("NewCookieStore: %v", err)
	}
	store := base.WithForceSecure(true)

	// Set emits Secure.
	w := httptest.NewRecorder()
	if err := store.Set(w, domainauth.Session{ID: "s", Principal: domainauth.Principal{ID: "u"}, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if c := findAuthCookie(t, w); !c.Secure {
		t.Fatalf("ForceSecure store cookie should be Secure (got Secure=false)")
	}
	// The signed cookie still round-trips through Get.
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(findAuthCookie(t, w))
	if _, err := store.Get(r); err != nil {
		t.Fatalf("Get after ForceSecure Set: %v", err)
	}

	// Clear also emits Secure so the invalidation cookie survives HTTPS.
	w2 := httptest.NewRecorder()
	if err := store.Clear(w2); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if c := findAuthCookie(t, w2); !c.Secure {
		t.Fatalf("ForceSecure store Clear cookie should be Secure (got Secure=false)")
	}
}

// TestCookieStore_WithForceSecureIsIndependent confirms the option returns a
// new store without mutating the original — so an unset AUTH_COOKIE_SECURE
// (default false) cannot accidentally flip Secure on elsewhere.
func TestCookieStore_WithForceSecureIsIndependent(t *testing.T) {
	base, err := NewCookieStore("a-very-long-test-secret-32bytes")
	if err != nil {
		t.Fatalf("NewCookieStore: %v", err)
	}
	forced := base.WithForceSecure(true)
	if base.forceSecure {
		t.Fatal("WithForceSecure mutated the original store")
	}
	if !forced.forceSecure {
		t.Fatal("WithForceSecure did not set forceSecure on the copy")
	}
}
