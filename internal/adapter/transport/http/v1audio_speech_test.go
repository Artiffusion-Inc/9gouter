package http

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9router/internal/adapter/transport/http/api"
)

// stubTtsHandler records the last request and returns a canned result.
type stubTtsHandler struct {
	lastReq TtsRequest
	body    []byte
	ct      string
	status  int
	err     error
}

func (s *stubTtsHandler) Handle(ctx context.Context, req TtsRequest) (TtsResult, error) {
	s.lastReq = req
	if s.err != nil {
		return TtsResult{StatusCode: s.status, Err: s.err}, s.err
	}
	st := s.status
	if st == 0 {
		st = http.StatusOK
	}
	return TtsResult{StatusCode: st, Body: s.body, ContentType: s.ct}, nil
}

var _ TtsHandler = (*stubTtsHandler)(nil)

func newTtsMux(t *testing.T, stub TtsHandler) (*http.ServeMux, *sql.DB) {
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
		Tts:            stub,
	}
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	return mux, db
}

func ttsReq(t *testing.T, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestV1AudioSpeech_HappyPath_ProviderPrefixStripped(t *testing.T) {
	stub := &stubTtsHandler{body: []byte("MP3BYTES"), ct: "audio/mpeg"}
	mux, db := newTtsMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k-openai"}`)

	req := ttsReq(t, `{"model":"openai/gpt-4o-mini-tts/alloy","input":"hello","response_format":"mp3"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "openai" {
		t.Errorf("ProviderID = %q, want openai", stub.lastReq.ProviderID)
	}
	if stub.lastReq.Model != "gpt-4o-mini-tts/alloy" {
		t.Errorf("Model = %q, want gpt-4o-mini-tts/alloy (prefix stripped)", stub.lastReq.Model)
	}
	if stub.lastReq.Input != "hello" {
		t.Errorf("Input = %q", stub.lastReq.Input)
	}
	if stub.lastReq.ResponseFormat != "mp3" {
		t.Errorf("ResponseFormat = %q, want mp3", stub.lastReq.ResponseFormat)
	}
	if rec.Body.String() != "MP3BYTES" {
		t.Errorf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "audio/mpeg" {
		t.Errorf("CT = %q", rec.Header().Get("Content-Type"))
	}
}

func TestV1AudioSpeech_BareModelDefaultsToOpenAI(t *testing.T) {
	stub := &stubTtsHandler{body: []byte("MP3"), ct: "audio/mpeg"}
	mux, db := newTtsMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)

	req := ttsReq(t, `{"model":"gpt-4o-mini-tts","input":"hi"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "openai" {
		t.Errorf("ProviderID = %q, want openai (bare fallback)", stub.lastReq.ProviderID)
	}
}

func TestV1AudioSpeech_GeminiProvider(t *testing.T) {
	stub := &stubTtsHandler{body: []byte("WAV"), ct: "audio/wav"}
	mux, db := newTtsMux(t, stub)
	mustCreateConnection(t, db, "gemini", `{"apiKey":"k-gem"}`)

	req := ttsReq(t, `{"model":"gemini/gemini-2.5-flash-preview-tts/Charlize","input":"hi","language":"en"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "gemini" {
		t.Errorf("ProviderID = %q, want gemini", stub.lastReq.ProviderID)
	}
	if stub.lastReq.Language != "en" {
		t.Errorf("Language = %q", stub.lastReq.Language)
	}
}

func TestV1AudioSpeech_ResponseFormatQueryFallback(t *testing.T) {
	stub := &stubTtsHandler{body: []byte("{}"), ct: "application/json"}
	mux, db := newTtsMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech?response_format=json", strings.NewReader(`{"model":"gpt-4o-mini-tts/alloy","input":"hi"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if stub.lastReq.ResponseFormat != "json" {
		t.Errorf("ResponseFormat = %q, want json (query fallback)", stub.lastReq.ResponseFormat)
	}
}

func TestV1AudioSpeech_MissingModel(t *testing.T) {
	stub := &stubTtsHandler{}
	mux, db := newTtsMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	req := ttsReq(t, `{"input":"hi"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing model)", rec.Code)
	}
}

func TestV1AudioSpeech_MissingInput(t *testing.T) {
	stub := &stubTtsHandler{}
	mux, db := newTtsMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	req := ttsReq(t, `{"model":"gpt-4o-mini-tts"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing input)", rec.Code)
	}
}

func TestV1AudioSpeech_InvalidJSON(t *testing.T) {
	stub := &stubTtsHandler{}
	mux, db := newTtsMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	req := ttsReq(t, `{not json`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (invalid json)", rec.Code)
	}
}

func TestV1AudioSpeech_NoCredentials(t *testing.T) {
	stub := &stubTtsHandler{}
	mux, _ := newTtsMux(t, stub)
	// No connection created for openai.
	req := ttsReq(t, `{"model":"gpt-4o-mini-tts","input":"hi"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no credentials)", rec.Code)
	}
}

func TestV1AudioSpeech_NotWired(t *testing.T) {
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
	req := ttsReq(t, `{"model":"gpt-4o-mini-tts","input":"hi"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 (not wired)", rec.Code)
	}
}

func TestV1AudioSpeech_UpstreamError(t *testing.T) {
	stub := &stubTtsHandler{status: http.StatusUnauthorized, err: errUpstreamBadKey}
	mux, db := newTtsMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	req := ttsReq(t, `{"model":"gpt-4o-mini-tts/alloy","input":"hi"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestV1AudioSpeech_DashboardPassthrough(t *testing.T) {
	stub := &stubTtsHandler{body: []byte("MP3"), ct: "audio/mpeg"}
	mux, db := newTtsMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	api.RegisterV1Dashboard(mux, api.Deps{V1Dispatch: mux.ServeHTTP})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/audio/speech", strings.NewReader(`{"model":"gpt-4o-mini-tts/alloy","input":"hi"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (passthrough); body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "openai" {
		t.Errorf("passthrough did not reach /v1/audio/speech")
	}
}

// satisfy unused-import guards if the file is compiled in isolation.
var _ = bytes.Compare
var _ = context.Background