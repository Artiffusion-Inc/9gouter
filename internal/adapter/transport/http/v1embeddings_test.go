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

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
	domainProv "github.com/Artiffusion-Inc/9router/internal/domain/provider"
	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
)

// embedCapture is a fake EmbeddingsHandler that records the request it received
// and returns a fixed OpenAI-shaped body.
type embedCapture struct {
	got      *EmbeddingsRequest
	body     []byte
	status   int
	err      error
	called   bool
}

func (c *embedCapture) Handle(ctx context.Context, req EmbeddingsRequest) (EmbeddingsResult, error) {
	c.called = true
	reqCopy := req
	c.got = &reqCopy
	return EmbeddingsResult{StatusCode: c.status, Body: c.body, Err: c.err}, nil
}

func newEmbeddingsHandler(t *testing.T, embed EmbeddingsHandler) (*v1Handler, *sql.DB) {
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
		DisabledModels: repo.NewDisabledModelsRepo(db),
		Config:         config.Config{},
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Embeddings:     embed,
	}
	return newV1Handler(deps), db
}

// TestHandleEmbeddings_HappyPath verifies the route resolves provider/model,
// pulls credentials from the active connection, passes them to the embeddings
// usecase, and writes the normalized body back to the client.
func TestHandleEmbeddings_HappyPath(t *testing.T) {
	cap := &embedCapture{
		status: http.StatusOK,
		body:   []byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"model":"openai/text-embedding-3-small","usage":{"prompt_tokens":5,"total_tokens":5}}`),
	}
	h, db := newEmbeddingsHandler(t, cap)
	mustCreateConnection(t, db, "openai", `{"apiKey":"sk-abc"}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader([]byte(`{"model":"openai/text-embedding-3-small","input":"hello"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.handleEmbeddings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !cap.called {
		t.Fatal("embeddings handler not invoked")
	}
	if cap.got.ProviderID != "openai" {
		t.Errorf("ProviderID = %q", cap.got.ProviderID)
	}
	if cap.got.Model != "text-embedding-3-small" {
		t.Errorf("Model = %q, want text-embedding-3-small", cap.got.Model)
	}
	if cap.got.Credentials.APIKey != "sk-abc" {
		t.Errorf("Credentials.APIKey = %q", cap.got.Credentials.APIKey)
	}
	if cap.got.Endpoint != "/v1/embeddings" {
		t.Errorf("Endpoint = %q", cap.got.Endpoint)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Body.String() != string(cap.body) {
		t.Errorf("body mismatch: got %s", rec.Body.String())
	}
}

// TestHandleEmbeddings_MissingModel verifies the 400 path before the usecase
// is reached (the handler must not invoke the embeddings handler with no model).
func TestHandleEmbeddings_MissingModel(t *testing.T) {
	cap := &embedCapture{status: http.StatusOK, body: []byte(`{}`)}
	h, db := newEmbeddingsHandler(t, cap)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader([]byte(`{"input":"hi"}`)))
	rec := httptest.NewRecorder()
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	h.handleEmbeddings(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if cap.called {
		t.Fatal("embeddings handler must not be called when model is missing")
	}
}

// TestHandleEmbeddings_NoCredentials verifies a 404 when the provider has no
// active connection.
func TestHandleEmbeddings_NoCredentials(t *testing.T) {
	cap := &embedCapture{status: http.StatusOK}
	h, _ := newEmbeddingsHandler(t, cap)
	// No connection created for "openai".

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader([]byte(`{"model":"openai/m","input":"hi"}`)))
	rec := httptest.NewRecorder()
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	h.handleEmbeddings(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if cap.called {
		t.Fatal("embeddings handler must not be called when no credentials")
	}
}

// TestHandleEmbeddings_NotWired verifies the graceful 501 when no embeddings
// handler is injected (e.g. partial wiring / dashboard-only build).
func TestHandleEmbeddings_NotWired(t *testing.T) {
	h, db := newEmbeddingsHandler(t, nil)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader([]byte(`{"model":"openai/m","input":"hi"}`)))
	rec := httptest.NewRecorder()
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	h.handleEmbeddings(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}

// TestHandleEmbeddings_PassesUpstreamError verifies a non-2xx Result.Err is
// surfaced as the upstream status with the usecase's error message.
func TestHandleEmbeddings_PassesUpstreamError(t *testing.T) {
	cap := &embedCapture{status: http.StatusTooManyRequests, err: errFmt("upstream returned 429: rate limited")}
	h, db := newEmbeddingsHandler(t, cap)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader([]byte(`{"model":"openai/m","input":"hi"}`)))
	rec := httptest.NewRecorder()
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	h.handleEmbeddings(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["message"] == nil {
		t.Fatalf("error body missing message: %s", rec.Body.String())
	}
}

// errFmt is a tiny helper to build an error with a format string.
func errFmt(format string, args ...any) error {
	return &simpleErr{msg: format}
}

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

// Compile-time: ensure embedCapture satisfies the interface.
var _ EmbeddingsHandler = (*embedCapture)(nil)

// silence unused-import guards in package-shared helpers.
var (
	_ = domainProv.Credentials{}
	_ = settings.ProviderConnection{}
)