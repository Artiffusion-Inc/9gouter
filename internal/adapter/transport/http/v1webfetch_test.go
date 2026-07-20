package http

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/webfetch"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/http/api"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// stubWebFetchHandler records the last request and returns a canned result.
type stubWebFetchHandler struct {
	lastReq WebFetchRequest
	body    []byte
	status  int
	err     error
}

func (s *stubWebFetchHandler) Handle(ctx context.Context, req WebFetchRequest) (WebFetchResult, error) {
	s.lastReq = req
	if s.err != nil {
		return WebFetchResult{StatusCode: s.status, Err: s.err}, s.err
	}
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	return WebFetchResult{StatusCode: status, Body: s.body}, nil
}

var _ WebFetchHandler = (*stubWebFetchHandler)(nil)

// newWebFetchMux builds a v1 mux backed by a fresh test DB and a stub
// WebFetchHandler. It returns the mux and the DB so callers can register
// connections on the SAME database the handler resolves against.
func newWebFetchMux(t *testing.T, stub WebFetchHandler) (*http.ServeMux, *sql.DB) {
	t.Helper()
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	deps := V1Deps{
		APIKeysRepo:    repo.NewAPIKeyRepo(db),
		SettingsRepo:   repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db),
		ComboRepo:      repo.NewComboRepo(db),
		AliasRepo:      repo.NewAliasRepo(db),
		NodeRepo:       repo.NewNodeRepo(db),
		ProxyPoolRepo:  repo.NewProxyPoolRepo(db),
		Config:         config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
		WebFetch:       stub,
	}
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	return mux, db
}

// newFetchReq builds a POST /v1/web/fetch (or /api/v1/web/fetch) request from
// the given body, marked as loopback so the api-key gate treats it as local
// (matching the existing chat/embeddings test convention).
func newFetchReq(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.RemoteAddr = "127.0.0.1:12345"
	return req
}

func TestV1WebFetch_HappyPath(t *testing.T) {
	stub := &stubWebFetchHandler{body: []byte(`{"provider":"jina-reader","url":"https://example.com"}`)}
	mux, db := newWebFetchMux(t, stub)
	mustCreateConnection(t, db, "jina-reader", `{"apiKey":"k-jina"}`)

	body := `{"provider":"jina-reader","url":"https://example.com","format":"markdown","max_characters":1000}`
	req := newFetchReq("/v1/web/fetch", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := stub.lastReq.ProviderID; got != "jina-reader" {
		t.Errorf("ProviderID = %q, want jina-reader", got)
	}
	if got := stub.lastReq.Params.URL; got != "https://example.com" {
		t.Errorf("Params.URL = %q", got)
	}
	if got := stub.lastReq.Params.Format; got != "markdown" {
		t.Errorf("Params.Format = %q, want markdown", got)
	}
	if got := stub.lastReq.Params.MaxCharacters; got != 1000 {
		t.Errorf("Params.MaxCharacters = %d, want 1000", got)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out["provider"] != "jina-reader" {
		t.Errorf("provider = %v", out["provider"])
	}
}

func TestV1WebFetch_ModelFieldAccepted(t *testing.T) {
	stub := &stubWebFetchHandler{body: []byte(`{}`)}
	mux, db := newWebFetchMux(t, stub)
	mustCreateConnection(t, db, "firecrawl", `{"apiKey":"k-fc"}`)

	// body.model instead of body.provider — provider IS the model.
	body := `{"model":"firecrawl","url":"https://example.com"}`
	req := newFetchReq("/v1/web/fetch", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := stub.lastReq.ProviderID; got != "firecrawl" {
		t.Errorf("ProviderID = %q, want firecrawl (from model field)", got)
	}
}

func TestV1WebFetch_MissingProvider(t *testing.T) {
	stub := &stubWebFetchHandler{}
	mux, _ := newWebFetchMux(t, stub)
	body := `{"url":"https://example.com"}`
	req := newFetchReq("/v1/web/fetch", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestV1WebFetch_MissingURL(t *testing.T) {
	stub := &stubWebFetchHandler{}
	mux, _ := newWebFetchMux(t, stub)
	body := `{"provider":"jina-reader"}`
	req := newFetchReq("/v1/web/fetch", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestV1WebFetch_BadJSON(t *testing.T) {
	stub := &stubWebFetchHandler{}
	mux, _ := newWebFetchMux(t, stub)
	req := newFetchReq("/v1/web/fetch", `{not json`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestV1WebFetch_SSRFBlocked(t *testing.T) {
	cases := []string{
		"http://127.0.0.1:8080/admin",
		"http://localhost/secrets",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/internal",
		"http://192.168.1.1/",
		"file:///etc/passwd",
		"ftp://example.com/",
	}
	stub := &stubWebFetchHandler{}
	mux, _ := newWebFetchMux(t, stub)
	for _, u := range cases {
		body := `{"provider":"jina-reader","url":"` + u + `"}`
		req := newFetchReq("/v1/web/fetch", body)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("url %q: status = %d, want 400 (SSRF/invalid)", u, rec.Code)
		}
	}
	if stub.lastReq.ProviderID != "" {
		t.Errorf("SSRF-blocked request must not reach the usecase; got %+v", stub.lastReq)
	}
}

func TestV1WebFetch_PublicURLAccepted(t *testing.T) {
	// Belt-and-braces: a public hostname must pass the SSRF guard.
	if err := assertPublicURL("https://example.com/path?q=1"); err != nil {
		t.Errorf("public url rejected: %v", err)
	}
	if err := assertPublicURL("http://example.com/"); err != nil {
		t.Errorf("public url rejected: %v", err)
	}
}

func TestV1WebFetch_NoCredentials(t *testing.T) {
	stub := &stubWebFetchHandler{}
	mux, _ := newWebFetchMux(t, stub)
	// jina-reader is a real web-fetch provider id but no connection is
	// registered → resolveCredentials returns NotFound.
	body := `{"provider":"jina-reader","url":"https://example.com"}`
	req := newFetchReq("/v1/web/fetch", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no active credentials); body=%s", rec.Code, rec.Body.String())
	}
}

func TestV1WebFetch_UpstreamError(t *testing.T) {
	stub := &stubWebFetchHandler{err: errBadUpstream, status: http.StatusBadGateway}
	mux, db := newWebFetchMux(t, stub)
	mustCreateConnection(t, db, "jina-reader", `{"apiKey":"k"}`)
	body := `{"provider":"jina-reader","url":"https://example.com"}`
	req := newFetchReq("/v1/web/fetch", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestV1WebFetch_CORSHeader(t *testing.T) {
	stub := &stubWebFetchHandler{body: []byte(`{}`)}
	mux, db := newWebFetchMux(t, stub)
	mustCreateConnection(t, db, "jina-reader", `{"apiKey":"k"}`)
	body := `{"provider":"jina-reader","url":"https://example.com"}`
	req := newFetchReq("/v1/web/fetch", body)
	req.Header.Set("Origin", "https://dashboard.example")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("CORS = %q, want *", got)
	}
}

func TestV1WebFetch_NotWired(t *testing.T) {
	// No WebFetch in deps → 501 after credentials resolve.
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	mustCreateConnection(t, db, "jina-reader", `{"apiKey":"k"}`)
	deps := V1Deps{
		APIKeysRepo: repo.NewAPIKeyRepo(db), SettingsRepo: repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db), ComboRepo: repo.NewComboRepo(db),
		AliasRepo: repo.NewAliasRepo(db), NodeRepo: repo.NewNodeRepo(db),
		ProxyPoolRepo: repo.NewProxyPoolRepo(db),
		Config:        config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	body := `{"provider":"jina-reader","url":"https://example.com"}`
	req := newFetchReq("/v1/web/fetch", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}

func TestV1WebFetch_DashboardPassthrough(t *testing.T) {
	// /api/v1/web/fetch must rewrite to /v1/web/fetch and dispatch via V1Dispatch.
	stub := &stubWebFetchHandler{body: []byte(`{"provider":"jina-reader"}`)}
	mux, db := newWebFetchMux(t, stub)
	mustCreateConnection(t, db, "jina-reader", `{"apiKey":"k"}`)
	api.RegisterV1Dashboard(mux, api.Deps{V1Dispatch: mux.ServeHTTP})
	body := `{"provider":"jina-reader","url":"https://example.com"}`
	req := newFetchReq("/api/v1/web/fetch", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (passthrough); body=%s", rec.Code, rec.Body.String())
	}
	if got := stub.lastReq.ProviderID; got != "jina-reader" {
		t.Errorf("ProviderID = %q, want jina-reader", got)
	}
}

// errBadUpstream is a sentinel for the upstream-error test.
var errBadUpstream = &upstreamErr{}

type upstreamErr struct{}

func (*upstreamErr) Error() string { return "upstream error: connection refused" }

// domainProv import marker (Credentials is passed through the handler).
var _ = domainProv.Credentials{}

// webfetch import marker (Params is constructed in tests).
var _ = webfetch.Params{}
