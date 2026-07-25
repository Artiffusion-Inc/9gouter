package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	dbschema "github.com/Artiffusion-Inc/9gouter/internal/adapter/db"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/sqlite"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/imageproxy"
)

// mustOpenTestDB mirrors httptransport.mustOpenDB but is local to the app
// package (the composition-root tests cannot import the transport-layer test
// helper). It opens a file-backed SQLite DB in a temp dir and syncs the schema.
func mustOpenTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir, err := os.MkdirTemp("", "9gouter-app-test-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := dbschema.SyncSchema(db); err != nil {
		t.Fatalf("sync schema: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// recordedFetch captures the request context's proxy.ValidatedTarget and the
// effective proxy.ProxyFetchOptions, then delegates to a stub response. It is
// the seam that lets the app-level tests assert the production executor routes
// pinned/connection-backed requests through proxy.ProxyAwareFetch with the
// right metadata WITHOUT performing a real network connect.
type recordedFetch struct {
	gotValidated *proxy.ValidatedTarget
	gotProxyOpts *proxy.ProxyFetchOptions
	gotClient    *http.Client
	gotReq       *http.Request
	called       bool
	statusCode   int
	body         string
	fail         error
}

func (f *recordedFetch) call(ctx context.Context, client *http.Client, req *http.Request, opts proxy.Options, proxyOpts proxy.ProxyFetchOptions, fallback *proxy.Fallback) (*http.Response, error) {
	f.called = true
	f.gotClient = client
	// Snapshot the request (the caller may reuse/mutate after return).
	f.gotReq = req.Clone(ctx)
	po := proxyOpts
	f.gotProxyOpts = &po
	if vt, ok := proxy.ValidatedTargetFromContext(req.Context()); ok {
		v := vt
		f.gotValidated = &v
	}
	if f.fail != nil {
		return nil, f.fail
	}
	rec := httptest.NewRecorder()
	rec.WriteHeader(f.statusCode)
	if f.statusCode == 0 {
		rec.WriteHeader(http.StatusOK)
	}
	_, _ = rec.WriteString(f.body)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

func newExecutor(t *testing.T, db *sql.DB) *productionImageExecutor {
	t.Helper()
	return &productionImageExecutor{
		connections: repo.NewConnectionRepo(db),
		pools:       repo.NewProxyPoolRepo(db),
		proxyOpts:   proxy.Options{FetchConnectTimeout: 60000},
		logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		fetch:       proxy.ProxyAwareFetch,
		directClient: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// TestProductionImageExecutor_ConnectionBacked loads the connection and builds
// proxy.ProxyFetchOptions from its data blob. The fetch seam asserts the
// effective options (connectionProxyUrl + enabled) reach ProxyAwareFetch and
// the request carries the transport metadata.
func TestProductionImageExecutor_ConnectionBacked(t *testing.T) {
	db := mustOpenTestDB(t)
	connRepo := repo.NewConnectionRepo(db)
	connData := `{"apiKey":"sk-test","connectionProxyEnabled":true,"connectionProxyUrl":"http://egress.example:8080","connectionNoProxy":"localhost"}`
	if _, err := connRepo.Create(context.Background(), settings.ProviderConnection{
		ID:       "conn-a",
		Provider: "openai",
		AuthType: "apiKey",
		IsActive: true,
		Data:     json.RawMessage(connData),
	}); err != nil {
		t.Fatalf("create connection: %v", err)
	}

	exec := newExecutor(t, db)
	rec := &recordedFetch{statusCode: http.StatusOK, body: `{"created":1,"data":[{"url":"https://x/a.png"}]}`}
	exec.fetch = rec.call

	req := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/images/generations", nil)
	creds := domainProv.Credentials{APIKey: "sk-test", ProviderSpecificData: map[string]any{"_connectionId": "conn-a"}}
	ctx := imageproxy.WithTransportMetadata(req.Context(), imageproxy.TransportMetadata{
		ProviderID:   "openai",
		ConnectionID: "conn-a",
		Credentials:  creds,
		Phase:        "submit",
	})
	req = req.WithContext(ctx)

	resp, err := exec.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if !rec.called {
		t.Fatal("proxy-aware fetch seam was not called for connection-backed request")
	}
	if rec.gotProxyOpts == nil {
		t.Fatal("no proxy options captured")
	}
	if !rec.gotProxyOpts.ConnectionProxyEnabled {
		t.Errorf("ConnectionProxyEnabled = false, want true (from connection data)")
	}
	if rec.gotProxyOpts.ConnectionProxyUrl != "http://egress.example:8080" {
		t.Errorf("ConnectionProxyUrl = %q, want http://egress.example:8080", rec.gotProxyOpts.ConnectionProxyUrl)
	}
	if rec.gotProxyOpts.NoProxy != "localhost" {
		t.Errorf("NoProxy = %q, want localhost", rec.gotProxyOpts.NoProxy)
	}
}

// TestProductionImageExecutor_MissingConnection asserts the plan invariant:
// a connection-backed auth-provider request whose connection cannot be loaded
// fails hard — no direct fallback, no proxy-aware fetch.
func TestProductionImageExecutor_MissingConnection(t *testing.T) {
	db := mustOpenTestDB(t)
	exec := newExecutor(t, db)
	rec := &recordedFetch{statusCode: http.StatusOK}
	exec.fetch = rec.call

	req := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/images/generations", nil)
	creds := domainProv.Credentials{APIKey: "sk-test", ProviderSpecificData: map[string]any{"_connectionId": "no-such-conn"}}
	ctx := imageproxy.WithTransportMetadata(req.Context(), imageproxy.TransportMetadata{
		ProviderID:   "openai",
		ConnectionID: "no-such-conn",
		Credentials:  creds,
		Phase:        "submit",
	})
	req = req.WithContext(ctx)

	_, err := exec.Do(req)
	if err == nil {
		t.Fatal("expected error for missing connection, got nil")
	}
	if rec.called {
		t.Error("proxy-aware fetch seam was called for a missing-connection request; want hard failure before fetch")
	}
}

// TestProductionImageExecutor_PinnedTarget asserts a pinned ValidatedHost is
// translated to proxy.ValidatedTarget and attached to the request context that
// reaches ProxyAwareFetch. The fetch seam records the validated IP/port and
// asserts the original hostname is preserved (SNI/Host contract from step 1).
func TestProductionImageExecutor_PinnedTarget(t *testing.T) {
	db := mustOpenTestDB(t)
	exec := newExecutor(t, db)
	rec := &recordedFetch{statusCode: http.StatusOK, body: `{"ok":true}`}
	exec.fetch = rec.call

	ip := net.ParseIP("203.0.113.42")
	req := httptest.NewRequest(http.MethodGet, "https://images.example.com/path/img.png", nil)
	ctx := imageproxy.WithTransportMetadata(req.Context(), imageproxy.TransportMetadata{
		ProviderID: "fal-ai",
		Phase:      "input",
		ValidatedHost: imageproxy.ValidatedHost{
			Scheme:   "https",
			Hostname: "images.example.com",
			Port:     "443",
			IP:       ip,
		},
	})
	req = req.WithContext(ctx)

	if _, err := exec.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !rec.called {
		t.Fatal("proxy-aware fetch seam was not called for pinned request")
	}
	if rec.gotValidated == nil {
		t.Fatal("no validated target captured in request context")
	}
	if !rec.gotValidated.IP.Equal(ip) {
		t.Errorf("validated IP = %v, want %v", rec.gotValidated.IP, ip)
	}
	if rec.gotValidated.Port != "443" {
		t.Errorf("validated Port = %q, want 443", rec.gotValidated.Port)
	}
	if rec.gotValidated.Hostname != "images.example.com" {
		t.Errorf("validated Hostname = %q, want images.example.com (must be preserved as SNI/Host)", rec.gotValidated.Hostname)
	}
	// The request URL must be untouched so the upstream sees the real host.
	if rec.gotReq.URL.Hostname() != "images.example.com" {
		t.Errorf("request URL hostname = %q, want images.example.com", rec.gotReq.URL.Hostname())
	}
}

// TestProductionImageExecutor_NoAuthDirectOnly asserts a no-auth request
// (connectionID == "") does NOT invoke the proxy-aware fetch seam and instead
// hits the direct httptest upstream via the dedicated direct client (which
// does not follow redirects).
func TestProductionImageExecutor_NoAuthDirectOnly(t *testing.T) {
	db := mustOpenTestDB(t)
	exec := newExecutor(t, db)
	rec := &recordedFetch{statusCode: http.StatusOK}
	exec.fetch = rec.call

	upstreamHit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		_, _ = io.WriteString(w, `{"images":["b64"]}`)
	}))
	defer srv.Close()

	// Point the direct client at the test transport so it reaches the stub.
	exec.directClient = srv.Client()
	exec.directClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/sdapi/v1/txt2img", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	ctx := imageproxy.WithTransportMetadata(req.Context(), imageproxy.TransportMetadata{
		ProviderID: "sdwebui",
		Phase:      "submit",
		// ConnectionID intentionally empty — no-auth direct-only path.
	})
	req = req.WithContext(ctx)

	resp, err := exec.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if rec.called {
		t.Error("proxy-aware fetch seam was called for no-auth direct-only request; want direct client only")
	}
	if !upstreamHit {
		t.Error("direct upstream was not hit by the no-auth direct-only client")
	}
}

// TestProductionImageExecutor_NoAuthDirectRejectsRedirect asserts the direct
// client does not follow redirects (the local guard in step 3 builds on this
// contract: a non-loopback / redirect target is rejected rather than followed).
func TestProductionImageExecutor_NoAuthDirectRejectsRedirect(t *testing.T) {
	db := mustOpenTestDB(t)
	exec := newExecutor(t, db)
	rec := &recordedFetch{statusCode: http.StatusOK}
	exec.fetch = rec.call

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"images":["final"]}`)
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	exec.directClient = srv.Client()
	exec.directClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/sdapi/v1/txt2img", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	ctx := imageproxy.WithTransportMetadata(req.Context(), imageproxy.TransportMetadata{
		ProviderID: "sdwebui",
		Phase:      "submit",
	})
	req = req.WithContext(ctx)

	resp, err := exec.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want %d (redirect must not be followed)", resp.StatusCode, http.StatusFound)
	}
	if rec.called {
		t.Error("proxy-aware fetch seam was called during redirect handling; want direct client only")
	}
}

// TestProductionImageExecutor_NoMetadataFallback asserts a request without
// transport metadata (test fallback / unmounted path) degrades to a plain
// http.DefaultClient.Do rather than failing — preserving the fallbackExecutor
// contract from the usecase.
func TestProductionImageExecutor_NoMetadataFallback(t *testing.T) {
	db := mustOpenTestDB(t)
	exec := newExecutor(t, db)
	rec := &recordedFetch{statusCode: http.StatusOK}
	exec.fetch = rec.call

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	// Use the test server's client so http.DefaultClient (which we can't easily
	// retarget) is bypassed by routing through a request the server handles.
	// Actually we must use http.DefaultClient here — so point the request at the
	// server and rely on DefaultClient. httptest.NewServer uses 127.0.0.1 which
	// DefaultClient reaches fine.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// Intentionally no WithTransportMetadata.

	resp, err := exec.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if rec.called {
		t.Error("proxy-aware fetch seam was called for a no-metadata request; want plain DefaultClient.Do")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
