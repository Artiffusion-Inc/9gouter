package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domainauth "github.com/Artiffusion-Inc/9gouter/internal/domain/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

type fakeGuardSettings struct {
	data []byte
	err  error
}

func (f *fakeGuardSettings) Get(ctx context.Context) (*settings.Settings, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &settings.Settings{Data: f.data}, nil
}

type fakeStore struct {
	valid bool
}

func (f *fakeStore) Get(r *http.Request) (*domainauth.Session, error) {
	if !f.valid {
		return nil, errInvalidSession
	}
	return &domainauth.Session{}, nil
}

func (f *fakeStore) Set(w http.ResponseWriter, s domainauth.Session) error { return nil }
func (f *fakeStore) Clear(w http.ResponseWriter) error                     { return nil }

// errInvalidSession mirrors domainauth.ErrInvalidSession without an import cycle.
var errInvalidSession = errInvalidSessionSentinel{}

type errInvalidSessionSentinel struct{}

func (errInvalidSessionSentinel) Error() string { return "invalid session" }

func guardBlob(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func newGuardHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("dashboard-ok"))
	})
}

func TestDashboardGuard_NonDashboardPathPassesThrough(t *testing.T) {
	g := NewDashboardGuard(newGuardHandler(), &fakeStore{valid: false}, &fakeGuardSettings{
		data: guardBlob(t, map[string]any{"requireLogin": true}),
	})
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "dashboard-ok" {
		t.Fatalf("non-dashboard path should pass through; got %d %q", rec.Code, rec.Body.String())
	}
}

func TestDashboardGuard_RequireLoginTrueNoSessionRedirects(t *testing.T) {
	g := NewDashboardGuard(newGuardHandler(), &fakeStore{valid: false}, &fakeGuardSettings{
		data: guardBlob(t, map[string]any{"requireLogin": true}),
	})
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Host = "localhost:20127"
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("requireLogin=true + no session should redirect, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc == "" || !strings.HasPrefix(loc, "/login") {
		t.Fatalf("expected redirect to /login, got %q", loc)
	}
}

func TestDashboardGuard_RequireLoginTrueValidSessionPasses(t *testing.T) {
	g := NewDashboardGuard(newGuardHandler(), &fakeStore{valid: true}, &fakeGuardSettings{
		data: guardBlob(t, map[string]any{"requireLogin": true}),
	})
	req := httptest.NewRequest(http.MethodGet, "/dashboard/providers/123", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "dashboard-ok" {
		t.Fatalf("valid session should pass, got %d %q", rec.Code, rec.Body.String())
	}
}

func TestDashboardGuard_RequireLoginFalseNoSessionPasses(t *testing.T) {
	g := NewDashboardGuard(newGuardHandler(), &fakeStore{valid: false}, &fakeGuardSettings{
		data: guardBlob(t, map[string]any{"requireLogin": false}),
	})
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "dashboard-ok" {
		t.Fatalf("requireLogin=false should pass without session, got %d %q", rec.Code, rec.Body.String())
	}
}

func TestDashboardGuard_TunnelBlockedRedirects(t *testing.T) {
	// tunnelDashboardAccess=false + Host matches tunnelUrl hostname → redirect.
	g := NewDashboardGuard(newGuardHandler(), &fakeStore{valid: true}, &fakeGuardSettings{
		data: guardBlob(t, map[string]any{
			"requireLogin":          true,
			"tunnelDashboardAccess": false,
			"tunnelUrl":             "https://my-tunnel.example.com",
		}),
	})
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Host = "my-tunnel.example.com"
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("tunnel host should be blocked even with a valid session, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("expected redirect to /login, got %q", loc)
	}
}

func TestDashboardGuard_TailscaleBlockedRedirects(t *testing.T) {
	g := NewDashboardGuard(newGuardHandler(), &fakeStore{valid: true}, &fakeGuardSettings{
		data: guardBlob(t, map[string]any{
			"requireLogin":          true,
			"tunnelDashboardAccess": false,
			"tailscaleUrl":          "http://tail-vm.tail-something.ts.net",
		}),
	})
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Host = "tail-vm.tail-something.ts.net"
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("tailscale host should be blocked, got %d", rec.Code)
	}
}

func TestDashboardGuard_TunnelAllowedWhenEnabled(t *testing.T) {
	// tunnelDashboardAccess=true (default) → tunnel host NOT blocked, session
	// check proceeds normally (valid session → pass).
	g := NewDashboardGuard(newGuardHandler(), &fakeStore{valid: true}, &fakeGuardSettings{
		data: guardBlob(t, map[string]any{
			"requireLogin":          true,
			"tunnelDashboardAccess": true,
			"tunnelUrl":             "https://my-tunnel.example.com",
		}),
	})
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Host = "my-tunnel.example.com"
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "dashboard-ok" {
		t.Fatalf("tunnel host with access enabled should pass with session, got %d %q", rec.Code, rec.Body.String())
	}
}

func TestDashboardGuard_TunnelBlockedIgnoresPortInHost(t *testing.T) {
	// Host header carries a port; the gate strips it before comparing (mirrors
	// dashboardGuard.js host.split(":")[0]).
	g := NewDashboardGuard(newGuardHandler(), &fakeStore{valid: true}, &fakeGuardSettings{
		data: guardBlob(t, map[string]any{
			"requireLogin":          true,
			"tunnelDashboardAccess": false,
			"tunnelUrl":             "https://my-tunnel.example.com",
		}),
	})
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Host = "my-tunnel.example.com:443"
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("tunnel host with port should still be blocked, got %d", rec.Code)
	}
}

func TestDashboardGuard_SettingsErrorFailClosed(t *testing.T) {
	// On settings read error the gate keeps the safe defaults: requireLogin
	// defaults true, so no session → redirect (fail-closed, matches JS catch{}).
	g := NewDashboardGuard(newGuardHandler(), &fakeStore{valid: false}, &fakeGuardSettings{
		err: errBoom,
	})
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("settings error should fail closed (redirect), got %d", rec.Code)
	}
}

var errBoom = errBoomSentinel{}

type errBoomSentinel struct{}

func (errBoomSentinel) Error() string { return "boom" }
