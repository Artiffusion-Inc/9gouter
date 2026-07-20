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
