package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	domainauth "github.com/Artiffusion-Inc/9gouter/internal/domain/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// alwaysInvalidStore is a domainauth.Store whose Get always returns an error
// (no valid session), so NewAuthFunc's session path is exercised as "no
// session" and the requireLogin gate becomes the deciding branch.
type alwaysInvalidStore struct{}

func (alwaysInvalidStore) Set(http.ResponseWriter, domainauth.Session) error { return nil }
func (alwaysInvalidStore) Get(*http.Request) (*domainauth.Session, error) {
	return nil, errors.New("no session")
}
func (alwaysInvalidStore) Clear(http.ResponseWriter) error { return nil }

func newGETRequest(path string) *http.Request {
	return httptest.NewRequest(http.MethodGet, path, nil)
}

// fakeSettingsRepo is a minimal settingsReader for the gate tests. It counts
// Get calls so the cache TTL is observable. When err is set, every Get fails;
// otherwise the data blob is returned.
type fakeSettingsRepo struct {
	data  []byte
	err   error
	calls atomic.Int64
}

func (f *fakeSettingsRepo) Get(ctx context.Context) (*settings.Settings, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return &settings.Settings{Data: f.data}, nil
}

func settingsBlob(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestRequireLoginGate_DefaultsToTrueWhenMissing(t *testing.T) {
	g := &requireLoginGate{settings: &fakeSettingsRepo{data: settingsBlob(t, map[string]any{})}}
	if !g.RequireLogin(context.Background()) {
		t.Fatalf("missing requireLogin should default to true (require login)")
	}
}

func TestRequireLoginGate_RespectsExplicitFalse(t *testing.T) {
	g := &requireLoginGate{settings: &fakeSettingsRepo{data: settingsBlob(t, map[string]any{"requireLogin": false})}}
	if g.RequireLogin(context.Background()) {
		t.Fatalf("requireLogin=false should bypass session auth")
	}
}

func TestRequireLoginGate_RespectsExplicitTrue(t *testing.T) {
	g := &requireLoginGate{settings: &fakeSettingsRepo{data: settingsBlob(t, map[string]any{"requireLogin": true})}}
	if !g.RequireLogin(context.Background()) {
		t.Fatalf("requireLogin=true should require login")
	}
}

func TestRequireLoginGate_NonBoolFallsBackToTrue(t *testing.T) {
	g := &requireLoginGate{settings: &fakeSettingsRepo{data: settingsBlob(t, map[string]any{"requireLogin": "no"})}}
	if !g.RequireLogin(context.Background()) {
		t.Fatalf("non-bool requireLogin should default to true")
	}
}

func TestRequireLoginGate_CachesWithinTTL(t *testing.T) {
	repo := &fakeSettingsRepo{data: settingsBlob(t, map[string]any{"requireLogin": true})}
	g := &requireLoginGate{settings: repo}
	_ = g.RequireLogin(context.Background())
	_ = g.RequireLogin(context.Background())
	_ = g.RequireLogin(context.Background())
	if got := repo.calls.Load(); got != 1 {
		t.Fatalf("expected 1 DB read (cached), got %d", got)
	}
}

func TestRequireLoginGate_RereadsAfterTTL(t *testing.T) {
	repo := &fakeSettingsRepo{data: settingsBlob(t, map[string]any{"requireLogin": true})}
	g := &requireLoginGate{settings: repo}
	_ = g.RequireLogin(context.Background())
	// Force the cache entry to look stale without sleeping.
	g.mu.Lock()
	g.cachedAt = time.Now().Add(-requireLoginCacheTTL - time.Second)
	g.mu.Unlock()
	_ = g.RequireLogin(context.Background())
	if got := repo.calls.Load(); got != 2 {
		t.Fatalf("expected 2 DB reads after TTL expiry, got %d", got)
	}
}

func TestRequireLoginGate_NilIsDenyByDefault(t *testing.T) {
	var g *requireLoginGate
	if !g.RequireLogin(context.Background()) {
		t.Fatalf("nil gate must default to require-login (deny-by-default)")
	}
}

func TestRequireLoginGate_ReadErrorReturnsTrue(t *testing.T) {
	repo := &fakeSettingsRepo{data: settingsBlob(t, map[string]any{"requireLogin": false}), err: errors.New("boom")}
	g := &requireLoginGate{settings: repo}
	if !g.RequireLogin(context.Background()) {
		t.Fatalf("DB error should fall back to require-login (safe default)")
	}
}

func TestNewAuthFunc_BypassesWhenRequireLoginFalse(t *testing.T) {
	// Use a session store that never has a valid session.
	store := &alwaysInvalidStore{}
	gate := &requireLoginGate{settings: &fakeSettingsRepo{data: settingsBlob(t, map[string]any{"requireLogin": false})}}
	auth := NewAuthFunc(store, gate)

	req := newGETRequest("/api/keys")
	if !auth(req) {
		t.Fatalf("non-protected /api route should pass when requireLogin=false and no session")
	}
}

func TestNewAuthFunc_StillProtectsAlwaysProtectedWhenRequireLoginFalse(t *testing.T) {
	store := &alwaysInvalidStore{}
	gate := &requireLoginGate{settings: &fakeSettingsRepo{data: settingsBlob(t, map[string]any{"requireLogin": false})}}
	auth := NewAuthFunc(store, gate)

	for _, p := range []string{
		"/api/settings/database",
		"/api/shutdown",
		"/api/oauth/cursor/auto-import",
		"/api/oauth/kiro/auto-import",
		"/api/version/shutdown",
		"/api/version/update",
	} {
		req := newGETRequest(p)
		if auth(req) {
			t.Fatalf("ALWAYS_PROTECTED %s must require a session even when requireLogin=false", p)
		}
	}
}

func TestNewAuthFunc_RequiresSessionWhenRequireLoginTrue(t *testing.T) {
	store := &alwaysInvalidStore{}
	gate := &requireLoginGate{settings: &fakeSettingsRepo{data: settingsBlob(t, map[string]any{"requireLogin": true})}}
	auth := NewAuthFunc(store, gate)

	req := newGETRequest("/api/keys")
	if auth(req) {
		t.Fatalf("non-protected /api route must require a session when requireLogin=true and none present")
	}
}

func TestNewAuthFunc_NilGateDenies(t *testing.T) {
	store := &alwaysInvalidStore{}
	auth := NewAuthFunc(store, nil)
	req := newGETRequest("/api/keys")
	if auth(req) {
		t.Fatalf("nil gate with no session must deny (deny-by-default)")
	}
}
