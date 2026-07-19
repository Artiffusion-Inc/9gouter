package http

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
)

// newResponsesHandler wires a v1Handler with a stub chat handler and an active
// ollama connection so handleChat's model/credential resolution succeeds.
func newResponsesHandler(t *testing.T, chat ChatHandler) (*v1Handler, *http.ServeMux) {
	t.Helper()
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	mustCreateConnection(t, db, "ollama", `{"apiKey":"k"}`)
	deps := V1Deps{
		APIKeysRepo:    repo.NewAPIKeyRepo(db),
		SettingsRepo:   repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db),
		ComboRepo:      repo.NewComboRepo(db),
		AliasRepo:      repo.NewAliasRepo(db),
		NodeRepo:       repo.NewNodeRepo(db),
		ProxyPoolRepo:  repo.NewProxyPoolRepo(db),
		Chat:           chat,
		Config:         config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	return newV1Handler(deps), mux
}

// TestResponsesCompact_InjectsFlag verifies the handler rewrites the endpoint
// to /v1/responses and injects _compact:true into the request body before
// dispatching through the chat pipeline.
func TestResponsesCompact_InjectsFlag(t *testing.T) {
	stub := &stubChatHandler{}
	_, mux := newResponsesHandler(t, stub)

	body := `{"model":"ollama/gpt-oss:120b","input":"hello"}`
	req := httptest.NewRequest("POST", "/v1/responses/compact", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if stub.got == nil {
		t.Fatal("chat handler not invoked")
	}
	if stub.got.Endpoint != "/v1/responses" {
		t.Errorf("endpoint = %q, want /v1/responses", stub.got.Endpoint)
	}
	var got map[string]any
	if err := json.Unmarshal(stub.got.Body, &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if v, ok := got["_compact"].(bool); !ok || !v {
		t.Errorf("_compact = %v, want true", got["_compact"])
	}
	// Original input preserved.
	if got["input"] != "hello" {
		t.Errorf("input = %v, want hello", got["input"])
	}
}

// TestResponsesCompact_OverwritesExistingFlag verifies a client-supplied
// _compact:false is forced to true.
func TestResponsesCompact_OverwritesExistingFlag(t *testing.T) {
	stub := &stubChatHandler{}
	_, mux := newResponsesHandler(t, stub)

	body := `{"model":"ollama/gpt-oss:120b","input":"hi","_compact":false}`
	req := httptest.NewRequest("POST", "/v1/responses/compact", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var got map[string]any
	_ = json.Unmarshal(stub.got.Body, &got)
	if v, ok := got["_compact"].(bool); !ok || !v {
		t.Errorf("_compact = %v, want true (forced)", got["_compact"])
	}
}

// TestResponsesGet_501NotImplemented verifies GET /v1/responses/{id} returns
// an honest 501 (the RetrieveResponse poll pipeline is not implemented — no
// upstream provider returns Responses-API LRO state) rather than a 404 that
// would read as "route does not exist".
func TestResponsesGet_501NotImplemented(t *testing.T) {
	stub := &stubChatHandler{}
	_, mux := newResponsesHandler(t, stub)

	req := httptest.NewRequest("GET", "/v1/responses/resp_abc123", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
	if stub.got != nil {
		t.Errorf("chat handler must not be invoked for GET poll")
	}
	var errBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("error body not JSON: %v", err)
	}
	errObj, _ := errBody["error"].(map[string]any)
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "resp_abc123") {
		t.Errorf("error message does not echo the id: %q", msg)
	}
}