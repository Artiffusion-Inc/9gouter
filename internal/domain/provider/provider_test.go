package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

// staticExecutor is a minimal test double implementing Executor.
type staticExecutor struct {
	url    string
	header http.Header
	body   json.RawMessage
}

func (s staticExecutor) BuildURL(model string, stream bool, urlIndex int, creds Credentials) string {
	return s.url
}

func (s staticExecutor) BuildHeaders(creds Credentials, stream bool) http.Header {
	return s.header.Clone()
}

func (s staticExecutor) TransformRequest(model string, body json.RawMessage, stream bool, creds Credentials) (json.RawMessage, error) {
	return s.body, nil
}

func (s staticExecutor) Execute(ctx context.Context, req ExecRequest) (Resp, error) {
	return Resp{URL: s.url, TransformedBody: s.body}, nil
}

// staticProvider is a minimal Provider implementation for tests.
type staticProvider struct {
	id       string
	executor Executor
}

func (p staticProvider) ID() string       { return p.id }
func (p staticProvider) Executor() Executor { return p.executor }

func TestProviderPort(t *testing.T) {
	creds := Credentials{
		APIKey:      "sk-test",
		AccessToken: "tok-test",
		ExpiresAt:   func() *time.Time { now := time.Now(); return &now }(),
		ProviderSpecificData: map[string]any{"baseUrl": "https://example.com"},
	}
	if creds.APIKey != "sk-test" {
		t.Errorf("APIKey = %q, want sk-test", creds.APIKey)
	}
	if creds.ExpiresAt == nil {
		t.Error("expected non-nil ExpiresAt")
	}
	if creds.ProviderSpecificData["baseUrl"] != "https://example.com" {
		t.Errorf("ProviderSpecificData mismatch")
	}

	prov := staticProvider{id: "openai", executor: staticExecutor{url: "https://api.openai.com/v1/chat/completions"}}
	if prov.ID() != "openai" {
		t.Errorf("ID = %q, want openai", prov.ID())
	}
	if _, ok := prov.Executor().(Executor); !ok {
		t.Error("Executor did not return the Executor interface")
	}

	// Exercise the executor methods return expected values.
	url := prov.Executor().BuildURL("gpt-4", true, 0, creds)
	if url == "" {
		t.Error("BuildURL returned empty string")
	}
	h := prov.Executor().BuildHeaders(creds, true)
	if h.Get("Authorization") != "" && h.Get("Authorization") != "Bearer tok-test" {
		t.Errorf("unexpected Authorization header: %q", h.Get("Authorization"))
	}
}

func TestStaticProviderImplementsProvider(t *testing.T) {
	var _ Provider = staticProvider{}
}

func TestExecutorTransformRequest(t *testing.T) {
	exec := staticExecutor{body: json.RawMessage(`{"ok":true}`)}
	out, err := exec.TransformRequest("gpt-4", json.RawMessage(`{}`), true, Credentials{})
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	if string(out) != `{"ok":true}` {
		t.Errorf("TransformRequest returned %q", out)
	}
}

func TestFormatImport(t *testing.T) {
	// Ensure provider package can reference format without import cycles.
	if format.Openai.String() != "openai" {
		t.Error("format import broken")
	}
}
