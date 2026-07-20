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

// stubImageHandler records the last request and returns a canned result.
type stubImageHandler struct {
	lastReq ImageRequest
	body    []byte
	ct      string
	status  int
	err     error
}

func (s *stubImageHandler) Handle(ctx context.Context, req ImageRequest) (ImageResult, error) {
	s.lastReq = req
	if s.err != nil {
		return ImageResult{StatusCode: s.status, Err: s.err}, s.err
	}
	st := s.status
	if st == 0 {
		st = http.StatusOK
	}
	return ImageResult{StatusCode: st, Body: s.body, ContentType: s.ct}, nil
}

var _ ImageHandler = (*stubImageHandler)(nil)

func newImageMux(t *testing.T, stub ImageHandler) (*http.ServeMux, *sql.DB) {
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
		Logger:         slogDiscard(),
		Image:          stub,
	}
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	return mux, db
}

func imageReq(t *testing.T, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestV1Images_HappyPath_ProviderPrefixStripped(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[{"url":"https://x/a.png"}]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k-openai"}`)

	req := imageReq(t, `{"model":"openai/dall-e-3","prompt":"cat","n":1,"size":"1024x1024"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "openai" {
		t.Errorf("ProviderID = %q, want openai", stub.lastReq.ProviderID)
	}
	if stub.lastReq.Model != "dall-e-3" {
		t.Errorf("Model = %q, want dall-e-3 (prefix stripped)", stub.lastReq.Model)
	}
	if stub.lastReq.Prompt != "cat" {
		t.Errorf("Prompt = %q", stub.lastReq.Prompt)
	}
	if stub.lastReq.N != 1 {
		t.Errorf("N = %d, want 1", stub.lastReq.N)
	}
}

func TestV1Images_ConnectionIdHeader(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)

	req := imageReq(t, `{"model":"dall-e-3","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("x-9gouter-connection-id") == "" {
		t.Error("expected x-9gouter-connection-id header for image gen")
	}
}

func TestV1Images_BareModelDefaultsToOpenAI(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)

	req := imageReq(t, `{"model":"dall-e-3","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if stub.lastReq.ProviderID != "openai" {
		t.Errorf("ProviderID = %q, want openai (bare fallback)", stub.lastReq.ProviderID)
	}
}

func TestV1Images_GeminiProvider(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[{"b64_json":"x"}]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "gemini", `{"apiKey":"k-gem"}`)

	req := imageReq(t, `{"model":"gemini/gemini-2.5-flash-image","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if stub.lastReq.ProviderID != "gemini" {
		t.Errorf("ProviderID = %q, want gemini", stub.lastReq.ProviderID)
	}
}

func TestV1Images_ResponseFormatQueryFallback(t *testing.T) {
	stub := &stubImageHandler{body: []byte("RAW"), ct: "image/png"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations?response_format=binary", strings.NewReader(`{"model":"dall-e-3","prompt":"cat","output_format":"png"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if stub.lastReq.ResponseFormat != "binary" {
		t.Errorf("ResponseFormat = %q, want binary (query fallback)", stub.lastReq.ResponseFormat)
	}
}

func TestV1Images_MissingModel(t *testing.T) {
	stub := &stubImageHandler{}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	req := imageReq(t, `{"prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing model)", rec.Code)
	}
}

func TestV1Images_MissingPrompt(t *testing.T) {
	stub := &stubImageHandler{}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	req := imageReq(t, `{"model":"dall-e-3"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing prompt)", rec.Code)
	}
}

func TestV1Images_InvalidJSON(t *testing.T) {
	stub := &stubImageHandler{}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	req := imageReq(t, `{not json`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (invalid json)", rec.Code)
	}
}

func TestV1Images_NoCredentials(t *testing.T) {
	stub := &stubImageHandler{}
	mux, _ := newImageMux(t, stub)
	// No connection created for openai.
	req := imageReq(t, `{"model":"dall-e-3","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no credentials)", rec.Code)
	}
}

func TestV1Images_NotWired(t *testing.T) {
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
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
	req := imageReq(t, `{"model":"dall-e-3","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 (not wired)", rec.Code)
	}
}

func TestV1Images_UpstreamError(t *testing.T) {
	stub := &stubImageHandler{status: http.StatusUnauthorized, err: errUpstreamBadKey}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	req := imageReq(t, `{"model":"dall-e-3","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestV1Images_DashboardPassthrough(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	api.RegisterV1Dashboard(mux, api.Deps{V1Dispatch: mux.ServeHTTP})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/images/generations", strings.NewReader(`{"model":"dall-e-3","prompt":"cat"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (passthrough); body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "openai" {
		t.Errorf("passthrough did not reach /v1/images/generations")
	}
}