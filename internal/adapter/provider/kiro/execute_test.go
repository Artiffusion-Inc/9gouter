package kiroexec

// execute_test.go pins the wired Kiro Execute override (#103) against a real
// httptest server that serves binary AWS EventStream frames — no mocks for the
// integrity path. The mock-fetch harness rewrites the BaseExecutor's upstream
// request to the in-process server (the same pattern as cursor's execute_test),
// so Execute drains a real binary EventStream body through RunIntegrityGate and
// synthesizes OpenAI SSE.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	domain "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// newKiroExecutor builds a Kiro executor whose upstream fetch is redirected at
// the in-process EventStream test server via a mock Fetch.
func newKiroExecutor(t *testing.T, srv *httptest.Server) *Executor {
	t.Helper()
	e := New(base.Config{
		ID:        "kiro",
		BaseURL:   srv.URL,
		URLSuffix: "/generateAssistantResponse",
		Format:    "kiro",
	})
	e.Fetch = func(ctx context.Context, client *http.Client, req *http.Request, opts proxy.Options, pfo proxy.ProxyFetchOptions, fb *proxy.Fallback) (*http.Response, error) {
		srvURL, _ := url.Parse(srv.URL)
		req.URL.Scheme = srvURL.Scheme
		req.URL.Host = srvURL.Host
		return client.Do(req)
	}
	return e
}

// kiroCreds is a valid API-key Kiro credential set.
func kiroCreds() domain.Credentials {
	return domain.Credentials{
		APIKey: "sk-test-APIKEY",
		ProviderSpecificData: map[string]any{
			"authMethod": "api_key",
		},
	}
}

// eventStreamServer returns an httptest server that responds with the given
// raw binary EventStream frames (concatenated) as its body.
func eventStreamServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
}

func TestExecute_KiroDrainsEventStreamToOpenAISSE(t *testing.T) {
	text := makeEventFrame(t, "assistantResponseEvent", map[string]any{"content": "Hello from kiro"})
	stop := makeEventFrame(t, "messageStopEvent", map[string]any{"stopReason": "end_turn"})
	srv := eventStreamServer(t, append(text, stop...))
	defer srv.Close()

	e := newKiroExecutor(t, srv)
	resp, err := e.Execute(context.Background(), domain.ExecRequest{
		Model:       "claude-sonnet-4.5",
		Body:        []byte(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`),
		Stream:      true,
		Credentials: kiroCreds(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Response == nil || resp.Response.StatusCode != 200 {
		t.Fatalf("status=%v", resp.Response)
	}
	if ct := resp.Response.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type=%q want text/event-stream", ct)
	}
	raw, _ := io.ReadAll(resp.Response.Body)
	resp.Response.Body.Close()
	out := string(raw)
	if !strings.Contains(out, "Hello from kiro") {
		t.Errorf("missing content delta: %s", out)
	}
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Errorf("missing assistant role: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("missing stop finish: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("missing [DONE]: %s", out)
	}
}

func TestExecute_KiroNon2xxPassesThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	e := newKiroExecutor(t, srv)
	resp, err := e.Execute(context.Background(), domain.ExecRequest{
		Model:       "claude-sonnet-4.5",
		Body:        []byte(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`),
		Stream:      true,
		Credentials: kiroCreds(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Non-2xx passes through untransformed for the chat handler to classify.
	if resp.Response.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status=%d want 429 pass-through", resp.Response.StatusCode)
	}
	body, _ := io.ReadAll(resp.Response.Body)
	resp.Response.Body.Close()
	if !strings.Contains(string(body), "rate limited") {
		t.Errorf("non-2xx body not passed through: %s", body)
	}
}

func TestExecute_KiroEllipsisRetry(t *testing.T) {
	// First upstream response is an ellipsis-only final; the retry handler serves
	// a complete clean stream — but our retry goes through BaseExecutor.Execute
	// again, which hits the same server. The server serves ellipsis first, then
	// after one request serves complete, mirroring a repair that succeeds.
	ellipsis := makeEventFrame(t, "assistantResponseEvent", map[string]any{"content": "..."})
	stop1 := makeEventFrame(t, "messageStopEvent", map[string]any{"stopReason": "end_turn"})
	ellipsisStream := append(ellipsis, stop1...)

	complete := makeEventFrame(t, "assistantResponseEvent", map[string]any{"content": "The full answer."})
	stop2 := makeEventFrame(t, "messageStopEvent", map[string]any{"stopReason": "end_turn"})
	completeStream := append(complete, stop2...)

	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		if callCount == 1 {
			_, _ = w.Write(ellipsisStream)
		} else {
			_, _ = w.Write(completeStream)
		}
	}))
	defer srv.Close()

	e := newKiroExecutor(t, srv)
	resp, err := e.Execute(context.Background(), domain.ExecRequest{
		Model:       "claude-sonnet-4.5",
		Body:        []byte(`{"systemPrompt":"base","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		Stream:      true,
		Credentials: kiroCreds(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	raw, _ := io.ReadAll(resp.Response.Body)
	resp.Response.Body.Close()
	out := string(raw)
	if !strings.Contains(out, "The full answer.") {
		t.Errorf("retry did not surface the repaired content: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("missing [DONE]: %s", out)
	}
}

func TestResolveKiroUpstreamModel(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4.5":                  "claude-sonnet-4.5",
		"claude-sonnet-4.5-agentic":          "claude-sonnet-4.5",
		"claude-sonnet-4.5-thinking":         "claude-sonnet-4.5",
		"claude-sonnet-4.5-thinking-agentic": "claude-sonnet-4.5",
		"gpt-5.6":                            "gpt-5.6",
	}
	for in, want := range cases {
		if got := resolveKiroUpstreamModel(in); got != want {
			t.Errorf("resolveKiroUpstreamModel(%q)=%q want %q", in, got, want)
		}
	}
}

func TestKiroContextWindow(t *testing.T) {
	// Unknown model falls back to Default (200000).
	if w := kiroContextWindow("some-unknown-model", domain.Credentials{}); w != 200000 {
		t.Errorf("unknown model contextWindow=%d want 200000", w)
	}
	// claude-sonnet-4.5 has no exact/pattern ContextWindow overlay → Default 200000.
	if w := kiroContextWindow("claude-sonnet-4.5", domain.Credentials{}); w != 200000 {
		t.Errorf("claude-sonnet-4.5 contextWindow=%d want 200000 (no overlay)", w)
	}
	// Kiro provider override: gpt-5.6-sol = 272000 (kiroGPT56), the exact-id win
	// over the *gpt-5* pattern (400000). Ports eb9728d0/5041494e.
	if w := kiroContextWindow("gpt-5.6-sol", domain.Credentials{}); w != 272000 {
		t.Errorf("gpt-5.6-sol contextWindow=%d want 272000 (kiro override)", w)
	}
	// Bare gpt-5.6 has no kiro exact entry → falls through to *gpt-5* pattern
	// (400000), matching upstream capabilities.js behaviour.
	if w := kiroContextWindow("gpt-5.6", domain.Credentials{}); w != 400000 {
		t.Errorf("gpt-5.6 contextWindow=%d want 400000 (gpt-5 pattern)", w)
	}
	// Claude Opus 4.8 exact id → 1M context (ports eb9728d0).
	if w := kiroContextWindow("claude-opus-4.8", domain.Credentials{}); w != 1000000 {
		t.Errorf("claude-opus-4.8 contextWindow=%d want 1000000", w)
	}
	// Per-credential override always wins.
	creds := domain.Credentials{ProviderSpecificData: map[string]any{"contextWindow": float64(500000)}}
	if w := kiroContextWindow("gpt-5.6-sol", creds); w != 500000 {
		t.Errorf("override contextWindow=%d want 500000", w)
	}
}

func TestKiroRepairEnabled(t *testing.T) {
	if !kiroRepairEnabled(domain.Credentials{}) {
		t.Error("default should be repair-enabled")
	}
	creds := domain.Credentials{ProviderSpecificData: map[string]any{"kiroToolCallRepair": false}}
	if kiroRepairEnabled(creds) {
		t.Error("kiroToolCallRepair=false should disable repair")
	}
	creds2 := domain.Credentials{ProviderSpecificData: map[string]any{"kiroToolCallRepair": true}}
	if !kiroRepairEnabled(creds2) {
		t.Error("kiroToolCallRepair=true should keep repair enabled")
	}
}

// Verify the executor satisfies the domain.Executor interface at compile time.
var _ domain.Executor = (*Executor)(nil)

// Silence unused-import guards when a test is compiled in isolation.
var _ = json.Marshal
