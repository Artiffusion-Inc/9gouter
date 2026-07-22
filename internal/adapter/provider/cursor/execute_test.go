package cursorexec

// execute_test.go pins the executeAgent dispatch + loop (#99) against the
// same in-process h2 server used by h2stream_test.go. No mocks for the
// AgentService path: the duplex transport exercises a real h2 server. The
// legacy tool-turn fallback path injects a mock Fetch (same harness as
// executor_stream_test.go) and a direct httptest HTTP server.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	domain "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// newAgentExecutor builds a Cursor executor whose AgentService transport + base
// URL point at the in-process h2 test server, so Execute dispatches the text
// turn there instead of agent.api5.cursor.sh.
func newAgentExecutor(t *testing.T, srv *httptest.Server) *Executor {
	t.Helper()
	tr := &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			return tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})
		},
	}
	e := New(base.Config{
		ID:        "cursor",
		BaseURL:   "https://api2.cursor.sh",
		URLSuffix: "/aiserver.v1.ChatService/StreamUnifiedChatWithTools",
		Format:    "cursor",
	})
	e.agentTransport = tr
	srvURL, _ := url.Parse(srv.URL)
	e.agentBaseURL = srvURL.Scheme + "://" + srvURL.Host
	return e
}

// cursorTextCreds is a valid Cursor credential set for the AgentService path.
func cursorTextCreds() domain.Credentials {
	return domain.Credentials{
		AccessToken: "tok-test-ACCESS",
		ProviderSpecificData: map[string]any{
			"machineId": "machine-123",
		},
	}
}

func TestExecute_TextStream_EmitsOpenAISSE(t *testing.T) {
	srv := newAgentTestServer(t, nil)

	// Stamp deterministic id/timestamp values.
	oldTS, oldUnix := nowTimestamp, nowUnix
	nowTimestamp = func() string { return "123" }
	nowUnix = func() int { return 1700000000 }
	defer func() { nowTimestamp, nowUnix = oldTS, oldUnix }()

	e := newAgentExecutor(t, srv)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	resp, err := e.Execute(context.Background(), domain.ExecRequest{
		Model:       "gpt-5.3-codex",
		Body:        body,
		Stream:      true,
		Credentials: cursorTextCreds(),
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
	if !strings.Contains(out, "data: ") {
		t.Errorf("missing SSE data frame: %s", out)
	}
	if !strings.Contains(out, "Hello from agent") {
		t.Errorf("missing text delta: %s", out)
	}
	if !strings.Contains(out, "chatcmpl-msg_123") {
		t.Errorf("missing response id: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("missing [DONE]: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("missing stop finish_reason: %s", out)
	}
}

func TestExecute_TextNonStream_EmitsChatCompletion(t *testing.T) {
	srv := newAgentTestServer(t, nil)

	oldTS, oldUnix := nowTimestamp, nowUnix
	nowTimestamp = func() string { return "456" }
	nowUnix = func() int { return 1700000001 }
	defer func() { nowTimestamp, nowUnix = oldTS, oldUnix }()

	e := newAgentExecutor(t, srv)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	resp, err := e.Execute(context.Background(), domain.ExecRequest{
		Model:       "gpt-5.3-codex",
		Body:        body,
		Stream:      false,
		Credentials: cursorTextCreds(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Response == nil || resp.Response.StatusCode != 200 {
		t.Fatalf("status=%v", resp.Response)
	}
	if ct := resp.Response.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type=%q want application/json", ct)
	}
	var comp map[string]any
	if err := json.NewDecoder(resp.Response.Body).Decode(&comp); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	resp.Response.Body.Close()
	if comp["object"] != "chat.completion" {
		t.Errorf("object=%v want chat.completion", comp["object"])
	}
	choices, _ := comp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices=%v want 1", choices)
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg["content"] != "Hello from agent" {
		t.Errorf("content=%v want Hello from agent", msg["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v want stop", choice["finish_reason"])
	}
	usage, _ := comp["usage"].(map[string]any)
	if usage == nil || usage["estimated"] != true {
		t.Errorf("usage=%v want estimated:true", usage)
	}
}

func TestExecute_Non200_ReturnsApiError(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	e := newAgentExecutor(t, srv)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	resp, err := e.Execute(context.Background(), domain.ExecRequest{
		Model:       "m",
		Body:        body,
		Stream:      true,
		Credentials: cursorTextCreds(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Response == nil {
		t.Fatal("nil response")
	}
	if resp.Response.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status=%d want 429", resp.Response.StatusCode)
	}
	var errBody map[string]any
	if err := json.NewDecoder(resp.Response.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	resp.Response.Body.Close()
	errObj, _ := errBody["error"].(map[string]any)
	if errObj["type"] != "api_error" {
		t.Errorf("error.type=%v want api_error", errObj["type"])
	}
	if !strings.Contains(errObj["message"].(string), "429") {
		t.Errorf("error.message=%v want 429 mention", errObj["message"])
	}
}

func TestExecute_MissingAccessToken_ReturnsConnectionError(t *testing.T) {
	srv := newAgentTestServer(t, nil)
	e := newAgentExecutor(t, srv)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	resp, err := e.Execute(context.Background(), domain.ExecRequest{
		Model:  "m",
		Body:   body,
		Stream: true,
		Credentials: domain.Credentials{
			ProviderSpecificData: map[string]any{"machineId": "machine-123"},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Response.StatusCode != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", resp.Response.StatusCode)
	}
	var errBody map[string]any
	_ = json.NewDecoder(resp.Response.Body).Decode(&errBody)
	resp.Response.Body.Close()
	errObj, _ := errBody["error"].(map[string]any)
	if errObj["type"] != "connection_error" {
		t.Errorf("error.type=%v want connection_error", errObj["type"])
	}
}

// TestExecute_ToolTurn_FallsBackToLegacyChatService verifies that a non-text
// turn (tool_calls) bypasses the AgentService path and runs the inherited
// BaseExecutor.Execute against the legacy ChatService URL, via a mock Fetch
// that rewrites the request to a local httptest server.
func TestExecute_ToolTurn_FallsBackToLegacyChatService(t *testing.T) {
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// A tool-call turn does not go through the AgentService synthesizer, so
		// the legacy upstream's own SSE passes straight through.
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"legacy\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(legacy.Close)

	e := New(base.Config{
		ID:        "cursor",
		BaseURL:   legacy.URL,
		URLSuffix: "/aiserver.v1.ChatService/StreamUnifiedChatWithTools",
		Format:    "cursor",
	})
	// Inject a mock Fetch so the inherited BaseExecutor.Execute hits the local
	// legacy server instead of the real api2.cursor.sh.
	baseExec := e.BaseExecutor
	baseExec.Fetch = func(ctx context.Context, client *http.Client, req *http.Request, opts proxy.Options, pfo proxy.ProxyFetchOptions, fb *proxy.Fallback) (*http.Response, error) {
		return client.Do(req)
	}

	body := []byte(`{"messages":[{"role":"user","content":"hi","tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}]}],"stream":true}`)
	resp, err := e.Execute(context.Background(), domain.ExecRequest{
		Model:       "m",
		Body:        body,
		Stream:      true,
		Credentials: cursorTextCreds(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	raw, _ := io.ReadAll(resp.Response.Body)
	resp.Response.Body.Close()
	out := string(raw)
	if !strings.Contains(out, "legacy") {
		t.Errorf("tool turn should pass through legacy upstream SSE: %s", out)
	}
}

// TestExecute_RequestContextDuplexWrite verifies the executor answers a
// mid-stream request_context_args from the server (the duplex write that
// distinguishes AgentService from the retired ChatService) and still returns
// the final text.
func TestExecute_RequestContextDuplexWrite(t *testing.T) {
	// The default newAgentTestServer already emits a request_context_args frame
	// before the text+done; this test just asserts the full round-trip surfaces
	// the text, which can only happen if the executor wrote the context
	// response (otherwise the server handler would have errored and closed).
	srv := newAgentTestServer(t, nil)
	e := newAgentExecutor(t, srv)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := e.Execute(ctx, domain.ExecRequest{
		Model:       "m",
		Body:        body,
		Stream:      true,
		Credentials: cursorTextCreds(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	raw, _ := io.ReadAll(resp.Response.Body)
	resp.Response.Body.Close()
	if !strings.Contains(string(raw), "Hello from agent") {
		t.Errorf("duplex round-trip did not surface final text: %s", raw)
	}
}
