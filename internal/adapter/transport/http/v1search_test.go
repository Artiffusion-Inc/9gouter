package http

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/http/api"
)

// stubSearchHandler records the last request and returns a canned result.
type stubSearchHandler struct {
	lastReq SearchRequest
	body    []byte
	ct      string
	status  int
	err     error
}

func (s *stubSearchHandler) Handle(ctx context.Context, req SearchRequest) (SearchResult, error) {
	s.lastReq = req
	if s.err != nil {
		return SearchResult{StatusCode: s.status, Err: s.err}, s.err
	}
	st := s.status
	if st == 0 {
		st = http.StatusOK
	}
	return SearchResult{StatusCode: st, Body: s.body, ContentType: s.ct}, nil
}

var _ SearchHandler = (*stubSearchHandler)(nil)

func newSearchMux(t *testing.T, stub SearchHandler) (*http.ServeMux, *sql.DB) {
	t.Helper()
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	deps := V1Deps{
		APIKeysRepo:    repo.NewAPIKeyRepo(db),
		SettingsRepo:   repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db),
		ComboRepo:      repo.NewComboRepo(db),
		AliasRepo:       repo.NewAliasRepo(db),
		NodeRepo:        repo.NewNodeRepo(db),
		ProxyPoolRepo:  repo.NewProxyPoolRepo(db),
		Config:         config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:         slogDiscard(),
		Search:         stub,
	}
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	return mux, db
}

func searchReq(t *testing.T, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestV1Search_HappyPath_AliasResolved(t *testing.T) {
	stub := &stubSearchHandler{body: []byte(`{"provider":"serper","query":"q","results":[],"answer":null,"usage":{},"metrics":{},"errors":[]}`), ct: "application/json"}
	mux, db := newSearchMux(t, stub)
	mustCreateConnection(t, db, "serper", `{"apiKey":"k-serper"}`)

	// "pplx" is an alias for perplexity; use "serper" directly (no alias needed).
	req := searchReq(t, `{"provider":"serper","query":"hello","max_results":5}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "serper" {
		t.Errorf("ProviderID = %q, want serper", stub.lastReq.ProviderID)
	}
	if stub.lastReq.Query != "hello" {
		t.Errorf("Query = %q", stub.lastReq.Query)
	}
	if stub.lastReq.MaxResults != 5 {
		t.Errorf("MaxResults = %d, want 5", stub.lastReq.MaxResults)
	}
}

func TestV1Search_ModelFallbackForProvider(t *testing.T) {
	stub := &stubSearchHandler{body: []byte(`{"provider":"gemini","query":"q","results":[]}`), ct: "application/json"}
	mux, db := newSearchMux(t, stub)
	mustCreateConnection(t, db, "gemini", `{"apiKey":"k-gem"}`)

	// UI sends `model` (provider IS the model for webSearch).
	req := searchReq(t, `{"model":"gemini","query":"q"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "gemini" {
		t.Errorf("ProviderID = %q, want gemini (from model field)", stub.lastReq.ProviderID)
	}
}

func TestV1Search_AliasResolution(t *testing.T) {
	stub := &stubSearchHandler{body: []byte(`{"provider":"brave-search","query":"q","results":[]}`), ct: "application/json"}
	mux, db := newSearchMux(t, stub)
	mustCreateConnection(t, db, "brave-search", `{"apiKey":"k-brave"}`)

	// "brave" is an alias for "brave-search".
	req := searchReq(t, `{"provider":"brave","query":"q"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "brave-search" {
		t.Errorf("ProviderID = %q, want brave-search (alias resolved)", stub.lastReq.ProviderID)
	}
}

func TestV1Search_MissingQuery(t *testing.T) {
	stub := &stubSearchHandler{}
	mux, db := newSearchMux(t, stub)
	mustCreateConnection(t, db, "serper", `{"apiKey":"k"}`)
	req := searchReq(t, `{"provider":"serper"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing query)", rec.Code)
	}
}

func TestV1Search_MissingProvider(t *testing.T) {
	stub := &stubSearchHandler{}
	mux, _ := newSearchMux(t, stub)
	req := searchReq(t, `{"query":"q"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing provider/model)", rec.Code)
	}
}

func TestV1Search_UnknownProvider(t *testing.T) {
	stub := &stubSearchHandler{}
	mux, _ := newSearchMux(t, stub)
	req := searchReq(t, `{"provider":"nope","query":"q"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (unknown provider)", rec.Code)
	}
}

func TestV1Search_InvalidJSON(t *testing.T) {
	stub := &stubSearchHandler{}
	mux, _ := newSearchMux(t, stub)
	req := searchReq(t, `{not json`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (invalid json)", rec.Code)
	}
}

func TestV1Search_NoCredentials(t *testing.T) {
	stub := &stubSearchHandler{}
	mux, _ := newSearchMux(t, stub)
	// No connection created for serper.
	req := searchReq(t, `{"provider":"serper","query":"q"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no credentials)", rec.Code)
	}
}

func TestV1Search_NotWired(t *testing.T) {
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	mustCreateConnection(t, db, "serper", `{"apiKey":"k"}`)
	deps := V1Deps{
		APIKeysRepo:    repo.NewAPIKeyRepo(db), SettingsRepo: repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db), ComboRepo: repo.NewComboRepo(db),
		AliasRepo: repo.NewAliasRepo(db), NodeRepo: repo.NewNodeRepo(db),
		ProxyPoolRepo: repo.NewProxyPoolRepo(db),
		Config:       config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:       slogDiscard(),
	}
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	req := searchReq(t, `{"provider":"serper","query":"q"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 (not wired)", rec.Code)
	}
}

func TestV1Search_UpstreamError(t *testing.T) {
	stub := &stubSearchHandler{status: http.StatusUnauthorized, err: errUpstreamBadKey}
	mux, db := newSearchMux(t, stub)
	mustCreateConnection(t, db, "serper", `{"apiKey":"k"}`)
	req := searchReq(t, `{"provider":"serper","query":"q"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestV1Search_DashboardPassthrough(t *testing.T) {
	stub := &stubSearchHandler{body: []byte(`{"provider":"serper","query":"q","results":[]}`), ct: "application/json"}
	mux, db := newSearchMux(t, stub)
	mustCreateConnection(t, db, "serper", `{"apiKey":"k"}`)
	api.RegisterV1Dashboard(mux, api.Deps{V1Dispatch: mux.ServeHTTP})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/search", strings.NewReader(`{"provider":"serper","query":"q"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (passthrough); body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "serper" {
		t.Errorf("passthrough did not reach /v1/search")
	}
}