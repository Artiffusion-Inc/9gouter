package http

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	dbschema "github.com/Artiffusion-Inc/9router/internal/adapter/db"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/sqlite"
	domainProv "github.com/Artiffusion-Inc/9router/internal/domain/provider"
	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
)

// stubChatHandler is a usecase stand-in that records the request and returns
// either a streaming or non-streaming outcome.
type stubChatHandler struct {
	got      *ChatRequest
	streamed bool
	err      error
}

func (s *stubChatHandler) Handle(ctx context.Context, req ChatRequest, w http.ResponseWriter, sse *Writer) (ChatResult, error) {
	s.got = &req
	if s.err != nil {
		return ChatResult{StatusCode: http.StatusBadGateway, Err: s.err}, s.err
	}
	if s.streamed {
		return ChatResult{StatusCode: http.StatusOK, Streamed: true}, nil
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4","choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	return ChatResult{StatusCode: http.StatusOK}, nil
}

func TestV1_MissingAPIKey(t *testing.T) {
	db := mustOpenDB(t)
	defer db.Close()

	deps := V1Deps{
		APIKeysRepo:    repo.NewAPIKeyRepo(db),
		SettingsRepo:   repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db),
		ComboRepo:      repo.NewComboRepo(db),
		AliasRepo:      repo.NewAliasRepo(db),
		NodeRepo:       repo.NewNodeRepo(db),
		ProxyPoolRepo:  repo.NewProxyPoolRepo(db),
		Chat:           &stubChatHandler{},
		Config:         config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	RegisterV1(mux, deps)

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Force requireApiKey via remote IP simulation (not loopback, no proxy stamp).
	req.RemoteAddr = "9.9.9.9:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want %d; body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func TestV1_InvalidAPIKey(t *testing.T) {
	db := mustOpenDB(t)
	defer db.Close()

	deps := V1Deps{
		APIKeysRepo:    repo.NewAPIKeyRepo(db),
		SettingsRepo:   repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db),
		ComboRepo:      repo.NewComboRepo(db),
		AliasRepo:      repo.NewAliasRepo(db),
		NodeRepo:       repo.NewNodeRepo(db),
		ProxyPoolRepo:  repo.NewProxyPoolRepo(db),
		Chat:           &stubChatHandler{},
		Config:         config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	RegisterV1(mux, deps)

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer bad-key")
	req.RemoteAddr = "9.9.9.9:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid key status = %d, want %d; body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func TestV1_ChatCompletions_Streamed(t *testing.T) {
	db := mustOpenDB(t)
	defer db.Close()
	mustCreateConnection(t, db, "openai", `{"apiKey":"sk-test","providerSpecificData":{"connectionProxyEnabled":false}}`)

	stub := &stubChatHandler{streamed: true}
	deps := V1Deps{
		APIKeysRepo:    repo.NewAPIKeyRepo(db),
		SettingsRepo:   repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db),
		ComboRepo:      repo.NewComboRepo(db),
		AliasRepo:      repo.NewAliasRepo(db),
		NodeRepo:       repo.NewNodeRepo(db),
		ProxyPoolRepo:  repo.NewProxyPoolRepo(db),
		Chat:           stub,
		Config:         config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	RegisterV1(mux, deps)

	body := `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if stub.got == nil {
		t.Fatal("chat handler was not called")
	}
	if stub.got.ProviderID != "openai" || stub.got.Model != "gpt-4" {
		t.Fatalf("unexpected request provider/model: %s/%s", stub.got.ProviderID, stub.got.Model)
	}
	if !stub.got.Stream {
		t.Fatalf("expected stream=true, got %v", stub.got.Stream)
	}
}

func TestV1_Messages_NonStreaming(t *testing.T) {
	db := mustOpenDB(t)
	defer db.Close()
	mustCreateConnection(t, db, "anthropic", `{"apiKey":"sk-ant","providerSpecificData":{"connectionProxyEnabled":false}}`)

	stub := &stubChatHandler{streamed: false}
	deps := V1Deps{
		APIKeysRepo:    repo.NewAPIKeyRepo(db),
		SettingsRepo:   repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db),
		ComboRepo:      repo.NewComboRepo(db),
		AliasRepo:      repo.NewAliasRepo(db),
		NodeRepo:       repo.NewNodeRepo(db),
		ProxyPoolRepo:  repo.NewProxyPoolRepo(db),
		Chat:           stub,
		Config:         config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	RegisterV1(mux, deps)

	body := `{"model":"anthropic/claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if stub.got == nil {
		t.Fatal("chat handler was not called")
	}
	if stub.got.Endpoint != "/v1/messages" {
		t.Fatalf("endpoint = %q, want /v1/messages", stub.got.Endpoint)
	}
	if stub.got.ProviderID != "anthropic" {
		t.Fatalf("provider = %q, want anthropic", stub.got.ProviderID)
	}
	if stub.got.Stream {
		t.Fatalf("expected non-streaming, got stream=%v", stub.got.Stream)
	}
}

func TestV1_Responses_MissingModel(t *testing.T) {
	db := mustOpenDB(t)
	defer db.Close()

	deps := V1Deps{
		APIKeysRepo:    repo.NewAPIKeyRepo(db),
		SettingsRepo:   repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db),
		ComboRepo:      repo.NewComboRepo(db),
		AliasRepo:      repo.NewAliasRepo(db),
		NodeRepo:       repo.NewNodeRepo(db),
		ProxyPoolRepo:  repo.NewProxyPoolRepo(db),
		Chat:           &stubChatHandler{},
		Config:         config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	RegisterV1(mux, deps)

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestV1_ChatCompletions_PropagatesCredentials(t *testing.T) {
	db := mustOpenDB(t)
	defer db.Close()

	mustCreateConnection(t, db, "openai", `{"apiKey":"sk-test","providerSpecificData":{"connectionProxyEnabled":false}}`)

	stub := &stubChatHandler{streamed: true}
	deps := V1Deps{
		APIKeysRepo:    repo.NewAPIKeyRepo(db),
		SettingsRepo:   repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db),
		ComboRepo:      repo.NewComboRepo(db),
		AliasRepo:      repo.NewAliasRepo(db),
		NodeRepo:       repo.NewNodeRepo(db),
		ProxyPoolRepo:  repo.NewProxyPoolRepo(db),
		Chat:           stub,
		Config:         config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	RegisterV1(mux, deps)

	body := `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if stub.got == nil {
		t.Fatal("chat handler was not called")
	}
	if stub.got.Credentials.APIKey != "sk-test" {
		t.Fatalf("api key = %q, want sk-test", stub.got.Credentials.APIKey)
	}
}

func mustOpenDB(t *testing.T) *sql.DB {
	t.Helper()
	dir, err := os.MkdirTemp("", "9router-v1-test-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	db, err := sqlite.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := dbschema.SyncSchema(db); err != nil {
		t.Fatalf("sync schema: %v", err)
	}
	return db
}

func mustCreateConnection(t *testing.T, db *sql.DB, provider, data string) {
	t.Helper()
	connRepo := repo.NewConnectionRepo(db)
	if err := connRepo.Create(context.Background(), settings.ProviderConnection{
		ID:       provider + "-conn",
		Provider: provider,
		AuthType: "apiKey",
		IsActive: true,
		Data:     json.RawMessage(data),
	}); err != nil {
		t.Fatalf("create connection: %v", err)
	}
}

// Ensure stubChatHandler implements ChatHandler.
var _ ChatHandler = (*stubChatHandler)(nil)

// Ensure Credentials from domain provider can be assigned to ChatRequest.
var _ = domainProv.Credentials{}
