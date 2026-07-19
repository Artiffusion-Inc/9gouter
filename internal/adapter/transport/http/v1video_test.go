package http

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9router/internal/adapter/transport/http/api"
	domainProv "github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// stubVideoHandler records the last request and returns a canned result.
type stubVideoHandler struct {
	lastReq VideoProxyRequest
	body    []byte
	ct      string
	status  int
	err     error
}

func (s *stubVideoHandler) Handle(ctx context.Context, req VideoProxyRequest) (VideoProxyResult, error) {
	s.lastReq = req
	if s.err != nil {
		return VideoProxyResult{StatusCode: s.status, Err: s.err}, s.err
	}
	st := s.status
	if st == 0 {
		st = http.StatusOK
	}
	return VideoProxyResult{StatusCode: st, Body: s.body, ContentType: s.ct, ConnectionID: req.ConnectionID}, nil
}

var _ VideoProxyHandler = (*stubVideoHandler)(nil)

func newVideoMux(t *testing.T, stub VideoProxyHandler) (*http.ServeMux, *sql.DB) {
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
		Video:          stub,
	}
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	return mux, db
}

func videoReq(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestV1Video_Create_Generations(t *testing.T) {
	stub := &stubVideoHandler{body: []byte(`{"request_id":"r1","status":"pending"}`), ct: "application/json"}
	mux, db := newVideoMux(t, stub)
	mustCreateConnection(t, db, "xai", `{"apiKey":"k-xai"}`)

	body := `{"model":"xai/grok-imagine-video","prompt":"a cat"}`
	req := videoReq("/v1/videos/generations", body)
	req.Header.Set("Idempotency-Key", "idem-1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.Action != "generations" {
		t.Errorf("Action = %q, want generations", stub.lastReq.Action)
	}
	if stub.lastReq.ProviderID != "xai" {
		t.Errorf("ProviderID = %q, want xai", stub.lastReq.ProviderID)
	}
	// Provider prefix stripped from forwarded body.
	if !bytes.Contains(stub.lastReq.Body, []byte(`"model":"grok-imagine-video"`)) {
		t.Errorf("forwarded body did not strip provider prefix: %s", stub.lastReq.Body)
	}
	if bytes.Contains(stub.lastReq.Body, []byte(`xai/`)) {
		t.Errorf("forwarded body still has xai/ prefix: %s", stub.lastReq.Body)
	}
	if stub.lastReq.IdempotencyKey != "idem-1" {
		t.Errorf("IdempotencyKey = %q", stub.lastReq.IdempotencyKey)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("x-9router-connection-id") == "" {
		t.Errorf("missing x-9router-connection-id header")
	}
}

func TestV1Video_Create_BareModelDefaultsToXai(t *testing.T) {
	stub := &stubVideoHandler{body: []byte(`{}`)}
	mux, db := newVideoMux(t, stub)
	mustCreateConnection(t, db, "xai", `{"apiKey":"k"}`)

	body := `{"model":"grok-imagine-video","prompt":"a cat"}`
	req := videoReq("/v1/videos/generations", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "xai" {
		t.Errorf("bare model should default to xai, got %q", stub.lastReq.ProviderID)
	}
}

func TestV1Video_Create_NonXaiProviderPrefix400s(t *testing.T) {
	stub := &stubVideoHandler{}
	mux, db := newVideoMux(t, stub)
	mustCreateConnection(t, db, "xai", `{"apiKey":"k"}`)

	body := `{"model":"openai/dall-e","prompt":"a cat"}`
	req := videoReq("/v1/videos/generations", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for non-xai provider prefix", rec.Code)
	}
	if stub.lastReq.ProviderID != "" {
		t.Errorf("non-xai provider should not reach usecase; got %+v", stub.lastReq)
	}
}

func TestV1Video_Create_NoCredentials(t *testing.T) {
	stub := &stubVideoHandler{}
	mux, _ := newVideoMux(t, stub)
	// No xai connection registered.
	body := `{"model":"grok-imagine-video","prompt":"a cat"}`
	req := videoReq("/v1/videos/generations", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no credentials)", rec.Code)
	}
}

func TestV1Video_GetPoll(t *testing.T) {
	stub := &stubVideoHandler{body: []byte(`{"status":"done","video":{"url":"u"}}`), ct: "application/json"}
	mux, db := newVideoMux(t, stub)
	mustCreateConnection(t, db, "xai", `{"apiKey":"k"}`)

	req := httptest.NewRequest(http.MethodGet, "/v1/videos/req-123", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.RequestID != "req-123" {
		t.Errorf("RequestID = %q, want req-123", stub.lastReq.RequestID)
	}
	if stub.lastReq.ProviderID != "xai" {
		t.Errorf("poll should fix provider to xai, got %q", stub.lastReq.ProviderID)
	}
}

func TestV1Video_NotWired(t *testing.T) {
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	mustCreateConnection(t, db, "xai", `{"apiKey":"k"}`)
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
	req := videoReq("/v1/videos/generations", `{"model":"grok-imagine-video"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}

func TestV1Video_DashboardPassthrough(t *testing.T) {
	stub := &stubVideoHandler{body: []byte(`{"request_id":"r1"}`), ct: "application/json"}
	mux, db := newVideoMux(t, stub)
	mustCreateConnection(t, db, "xai", `{"apiKey":"k"}`)
	api.RegisterV1Dashboard(mux, api.Deps{V1Dispatch: mux.ServeHTTP})

	body := `{"model":"grok-imagine-video","prompt":"a cat"}`
	req := videoReq("/api/v1/videos/generations", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (passthrough); body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.Action != "generations" {
		t.Errorf("passthrough did not reach /v1/videos/generations")
	}
}

// domainProv import marker (Credentials passed through handler).
var _ = domainProv.Credentials{}
