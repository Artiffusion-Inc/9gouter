package http

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/http/api"
)

// stubSttHandler records the last request and returns a canned result.
type stubSttHandler struct {
	lastReq SttRequest
	body    []byte
	ct      string
	status  int
	err     error
}

func (s *stubSttHandler) Handle(ctx context.Context, req SttRequest) (SttResult, error) {
	s.lastReq = req
	if s.err != nil {
		return SttResult{StatusCode: s.status, Err: s.err}, s.err
	}
	st := s.status
	if st == 0 {
		st = http.StatusOK
	}
	return SttResult{StatusCode: st, Body: s.body, ContentType: s.ct}, nil
}

var _ SttHandler = (*stubSttHandler)(nil)

func newSttMux(t *testing.T, stub SttHandler) (*http.ServeMux, *sql.DB) {
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
		Stt:            stub,
	}
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	return mux, db
}

// sttMultipartBody builds a multipart/form-data body with a `file` part
// (audio bytes) and the given form fields. Returns the body and Content-Type.
func sttMultipartBody(t *testing.T, fields map[string]string, fileBytes []byte) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", "clip.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(fileBytes); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), w.FormDataContentType()
}

func sttReq(t *testing.T, body, ct string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", ct)
	return req
}

func TestV1Audio_HappyPath_ProviderPrefixStripped(t *testing.T) {
	stub := &stubSttHandler{body: []byte(`{"text":"hello"}`), ct: "application/json"}
	mux, db := newSttMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k-openai"}`)

	body, ct := sttMultipartBody(t, map[string]string{"model": "openai/whisper-1", "language": "en"}, []byte("AUDIO"))
	req := sttReq(t, body, ct)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "openai" {
		t.Errorf("ProviderID = %q, want openai", stub.lastReq.ProviderID)
	}
	if stub.lastReq.Model != "whisper-1" {
		t.Errorf("Model = %q, want whisper-1 (prefix stripped)", stub.lastReq.Model)
	}
	if string(stub.lastReq.File) != "AUDIO" {
		t.Errorf("File = %q, want AUDIO", stub.lastReq.File)
	}
	if stub.lastReq.FormFields["language"] != "en" {
		t.Errorf("language field = %q", stub.lastReq.FormFields["language"])
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("response CT = %q", rec.Header().Get("Content-Type"))
	}
	if rec.Body.String() != `{"text":"hello"}` {
		t.Errorf("response body = %q", rec.Body.String())
	}
}

func TestV1Audio_BareModelDefaultsToOpenAI(t *testing.T) {
	stub := &stubSttHandler{body: []byte(`{"text":"x"}`), ct: "application/json"}
	mux, db := newSttMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)

	body, ct := sttMultipartBody(t, map[string]string{"model": "whisper-1"}, []byte("AUD"))
	req := sttReq(t, body, ct)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "openai" {
		t.Errorf("bare model should default to openai, got %q", stub.lastReq.ProviderID)
	}
	if stub.lastReq.Model != "whisper-1" {
		t.Errorf("bare model preserved verbatim: %q", stub.lastReq.Model)
	}
}

func TestV1Audio_GroqProvider(t *testing.T) {
	stub := &stubSttHandler{body: []byte(`{"text":"x"}`), ct: "application/json"}
	mux, db := newSttMux(t, stub)
	mustCreateConnection(t, db, "groq", `{"apiKey":"k-groq"}`)

	body, ct := sttMultipartBody(t, map[string]string{"model": "groq/whisper-large-v3"}, []byte("AUD"))
	req := sttReq(t, body, ct)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "groq" {
		t.Errorf("ProviderID = %q, want groq", stub.lastReq.ProviderID)
	}
}

func TestV1Audio_MissingModel(t *testing.T) {
	stub := &stubSttHandler{}
	mux, _ := newSttMux(t, stub)
	body, ct := sttMultipartBody(t, map[string]string{}, []byte("AUD"))
	req := sttReq(t, body, ct)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing model)", rec.Code)
	}
	if stub.lastReq.ProviderID != "" {
		t.Errorf("should not reach usecase: %+v", stub.lastReq)
	}
}

func TestV1Audio_MissingFile(t *testing.T) {
	stub := &stubSttHandler{}
	mux, _ := newSttMux(t, stub)
	// multipart with only the model field, no file part.
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", "openai/whisper-1")
	w.Close()
	req := sttReq(t, buf.String(), w.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing file)", rec.Code)
	}
}

func TestV1Audio_NoCredentials(t *testing.T) {
	stub := &stubSttHandler{}
	mux, _ := newSttMux(t, stub)
	body, ct := sttMultipartBody(t, map[string]string{"model": "openai/whisper-1"}, []byte("AUD"))
	req := sttReq(t, body, ct)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no credentials)", rec.Code)
	}
}

func TestV1Audio_InvalidMultipart(t *testing.T) {
	stub := &stubSttHandler{}
	mux, _ := newSttMux(t, stub)
	req := sttReq(t, "not multipart", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (invalid multipart)", rec.Code)
	}
}

func TestV1Audio_NotWired(t *testing.T) {
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	deps := V1Deps{
		APIKeysRepo: repo.NewAPIKeyRepo(db), SettingsRepo: repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db), ComboRepo: repo.NewComboRepo(db),
		AliasRepo: repo.NewAliasRepo(db), NodeRepo: repo.NewNodeRepo(db),
		ProxyPoolRepo: repo.NewProxyPoolRepo(db),
		Config:        config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:        slogDiscard(),
	}
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	body, ct := sttMultipartBody(t, map[string]string{"model": "openai/whisper-1"}, []byte("AUD"))
	req := sttReq(t, body, ct)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}

func TestV1Audio_UpstreamError(t *testing.T) {
	stub := &stubSttHandler{status: http.StatusUnauthorized, err: errUpstreamBadKey}
	mux, db := newSttMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	body, ct := sttMultipartBody(t, map[string]string{"model": "openai/whisper-1"}, []byte("AUD"))
	req := sttReq(t, body, ct)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestV1Audio_DashboardPassthrough(t *testing.T) {
	stub := &stubSttHandler{body: []byte(`{"text":"x"}`), ct: "application/json"}
	mux, db := newSttMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	api.RegisterV1Dashboard(mux, api.Deps{V1Dispatch: mux.ServeHTTP})

	body, ct := sttMultipartBody(t, map[string]string{"model": "openai/whisper-1"}, []byte("AUD"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/audio/transcriptions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (passthrough); body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "openai" {
		t.Errorf("passthrough did not reach /v1/audio/transcriptions")
	}
}

// errUpstreamBadKey is a sentinel upstream error for the NotWired/upstream tests.
var errUpstreamBadKey = errUpstream("Invalid API key")

type errUpstream string

func (e errUpstream) Error() string { return string(e) }

// slogDiscard returns a discard logger.
func slogDiscard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }
