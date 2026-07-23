package codexec

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// codex_sse_test.go ports the regression coverage for decolua/9router #2452
// (0c55d49a part B): the SSE transient-error peek + re-assemble. Tests drive
// peekSseTransientError directly over io.ReadCloser bodies (no mock) and the
// full Execute path through a real httptest.Server upstream.

func bodyRC(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }

func TestPeek_CapacityAccountFallback(t *testing.T) {
	text := "event: error\ndata: {\"error\":{\"message\":\"Selected model is at capacity. Please try a different model.\"}}\n\n"
	peek := peekSseTransientError(bodyRC(text))
	if !peek.accountFallback {
		t.Fatalf("accountFallback = false, want true")
	}
	if peek.matched == "" {
		t.Fatal("matched empty for capacity body")
	}
	if peek.message != codexModelCapacityMessage {
		t.Errorf("message = %q, want canonical capacity message", peek.message)
	}
	if peek.replacementBody != nil {
		t.Error("capacity match must not return a replacement body")
	}
}

func TestPeek_CapacityPatternOnlyFallback(t *testing.T) {
	// "model_at_capacity" without a structured message → canonical fallback.
	peek := peekSseTransientError(bodyRC("data: {\"type\":\"model_at_capacity\"}\n\n"))
	if !peek.accountFallback {
		t.Fatalf("accountFallback = false, want true (model_at_capacity)")
	}
}

func TestPeek_OverloadedRetryNotAccountFallback(t *testing.T) {
	text := "data: {\"error\":{\"message\":\"server_is_overloaded\"}}\n\n"
	peek := peekSseTransientError(bodyRC(text))
	if peek.accountFallback {
		t.Fatal("overloaded must NOT set accountFallback (retry same account)")
	}
	if peek.matched != "server_is_overloaded" {
		t.Errorf("matched = %q, want server_is_overloaded", peek.matched)
	}
	if peek.message == "" {
		t.Error("message should be extracted, got empty")
	}
}

func TestPeek_ServiceUnavailableRetry(t *testing.T) {
	peek := peekSseTransientError(bodyRC("data: {\"error\":{\"message\":\"service_unavailable_error\"}}\n\n"))
	if peek.accountFallback {
		t.Fatal("service_unavailable must NOT set accountFallback")
	}
	if peek.matched != "service_unavailable_error" {
		t.Errorf("matched = %q, want service_unavailable_error", peek.matched)
	}
}

func TestPeek_UserOutputBreakReassembles(t *testing.T) {
	text := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"
	peek := peekSseTransientError(bodyRC(text))
	if peek.matched != "" {
		t.Fatalf("user-output body matched %q, want no match", peek.matched)
	}
	if peek.replacementBody == nil {
		t.Fatal("no-match must return a replacement body to re-assemble")
	}
	got, _ := io.ReadAll(peek.replacementBody)
	if string(got) != text {
		t.Errorf("re-assembled body = %q, want original %q", string(got), text)
	}
}

func TestPeek_FunctionCallArgsDeltaBreakReassembles(t *testing.T) {
	text := "data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"x\"}\n\n"
	peek := peekSseTransientError(bodyRC(text))
	if peek.matched != "" {
		t.Fatalf("function-call-args body matched %q, want no match", peek.matched)
	}
	got, _ := io.ReadAll(peek.replacementBody)
	if string(got) != text {
		t.Errorf("re-assembled body = %q, want original %q", string(got), text)
	}
}

func TestPeek_EmptyBodyNoReplacement(t *testing.T) {
	// An empty body (EOF immediately) yields no prefix and no remaining body —
	// replacementBody is nil so the caller leaves the response as-is.
	peek := peekSseTransientError(bodyRC(""))
	if peek.matched != "" {
		t.Fatalf("empty body matched %q, want none", peek.matched)
	}
	// Empty prefix + EOF body: re-assemble produces an empty reader (nil is
	// acceptable too). Either way it must read as empty.
	if peek.replacementBody != nil {
		got, _ := io.ReadAll(peek.replacementBody)
		if len(got) != 0 {
			t.Errorf("empty body re-assembled to %q, want empty", string(got))
		}
	}
}

func TestPeek_ReassemblesPrefixAndRest(t *testing.T) {
	// Body larger than a single read: prefix chunks accumulate and the
	// remaining body (after the peek) is appended. Use a user-output break so
	// the peek stops and re-assembles the whole thing.
	text := strings.Repeat("a", 9000) + "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\"}\n\n" + strings.Repeat("b", 5000)
	peek := peekSseTransientError(bodyRC(text))
	if peek.matched != "" {
		t.Fatalf("matched %q, want none", peek.matched)
	}
	if peek.replacementBody == nil {
		t.Fatal("expected replacement body")
	}
	got, _ := io.ReadAll(peek.replacementBody)
	if string(got) != text {
		t.Errorf("re-assembled length = %d, want %d", len(got), len(text))
	}
}

func TestPeek_NestedResponseErrorMessageExtracted(t *testing.T) {
	// error.message nested under response.error.message (the JS path).
	text := "data: {\"response\":{\"error\":{\"message\":\"Selected model is at capacity. Please try a different model.\"}}}\n\n"
	peek := peekSseTransientError(bodyRC(text))
	if !peek.accountFallback {
		t.Fatal("accountFallback = false, want true (capacity via response.error)")
	}
	if peek.message != codexModelCapacityMessage {
		t.Errorf("message = %q, want canonical capacity message", peek.message)
	}
}

func TestFindNestedMessage(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"plain string", "hi", ""},
		{"error.message", map[string]any{"error": map[string]any{"message": "boom"}}, "boom"},
		{"response.error.message", map[string]any{"response": map[string]any{"error": map[string]any{"message": "kaput"}}}, "kaput"},
		{"top message", map[string]any{"message": "top"}, "top"},
		{"nested in array", []any{map[string]any{"error": map[string]any{"message": "in-array"}}}, "in-array"},
		{"deep nested", map[string]any{"a": map[string]any{"b": map[string]any{"error": map[string]any{"message": "deep"}}}}, "deep"},
		{"empty message ignored", map[string]any{"error": map[string]any{"message": "  "}, "message": "real"}, "real"},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		if got := findNestedMessage(c.v, 0); got != c.want {
			t.Errorf("%s: findNestedMessage = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFindNestedMessage_DepthLimit(t *testing.T) {
	// Build a value nested deeper than the depth-6 cap.
	v := map[string]any{"message": "too-deep"}
	for i := 0; i < 10; i++ {
		v = map[string]any{"child": v}
	}
	if got := findNestedMessage(v, 0); got != "" {
		t.Errorf("deeply nested (past cap) returned %q, want empty", got)
	}
}

func TestExtractSseErrorMessage_FallbackToPattern(t *testing.T) {
	// No data: lines, no canonical message → falls back to the matched pattern.
	if got := extractSseErrorMessage("random bytes\nnot sse", "server_is_overloaded"); got != "server_is_overloaded" {
		t.Errorf("extractSseErrorMessage fallback = %q, want server_is_overloaded", got)
	}
}

func TestCodexSseErrorResponse_Shape(t *testing.T) {
	resp := codexSseErrorResponse(http.StatusServiceUnavailable, "boom")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj["message"] != "boom" {
		t.Errorf("error.message = %v, want boom", errObj["message"])
	}
	if errObj["code"] != "service_unavailable" {
		t.Errorf("error.code = %v, want service_unavailable", errObj["code"])
	}
	if errObj["type"] != "server_error" {
		t.Errorf("error.type = %v, want server_error", errObj["type"])
	}
}

// TestExecute_CapityReturns503 runs the full Execute path through a real
// httptest.Server that streams a capacity-error SSE body with HTTP 200, and
// verifies Execute returns a synthetic 503 (so account fallback rotates).
func TestExecute_CapacityReturns503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: error\ndata: {\"error\":{\"message\":\"Selected model is at capacity. Please try a different model.\"}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	e := New(base.Config{BaseURLs: []string{upstream.URL}, TimeoutMs: 5000})
	req := provider.ExecRequest{
		Model:       "gpt-5.3-codex",
		Body:        json.RawMessage(`{"input":"hi"}`),
		Stream:      true,
		Credentials: provider.Credentials{APIKey: "x"},
	}
	resp, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	defer func() {
		if resp.Response != nil {
			resp.Response.Body.Close()
		}
		if resp.Done != nil {
			resp.Done()
		}
	}()
	if resp.Response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (capacity → account fallback)", resp.Response.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Response.Body).Decode(&out)
	if msg, _ := out["error"].(map[string]any)["message"].(string); msg != codexModelCapacityMessage {
		t.Errorf("error.message = %v, want canonical capacity message", out)
	}
}

// TestExecute_NormalStreamReassembles runs the full Execute path through a
// real upstream streaming a normal completion (user-output delta) and verifies
// the body is re-assembled byte-for-byte (status stays 200).
func TestExecute_NormalStreamReassembles(t *testing.T) {
	body := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	e := New(base.Config{BaseURLs: []string{upstream.URL}, TimeoutMs: 5000})
	req := provider.ExecRequest{
		Model:       "gpt-5.3-codex",
		Body:        json.RawMessage(`{"input":"hi"}`),
		Stream:      true,
		Credentials: provider.Credentials{APIKey: "x"},
	}
	resp, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	defer func() {
		if resp.Response != nil {
			resp.Response.Body.Close()
		}
		if resp.Done != nil {
			resp.Done()
		}
	}()
	if resp.Response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (normal stream passthrough)", resp.Response.StatusCode)
	}
	got, _ := io.ReadAll(resp.Response.Body)
	if !bytes.Contains(got, []byte("response.output_text.delta")) {
		t.Errorf("re-assembled body missing the output delta: %q", string(got))
	}
}

// TestExecute_OverloadedRetriesThen503 verifies the overloaded path retries the
// same account up to codexSseRetryAttempts, then surfaces 503.
func TestExecute_OverloadedRetriesThen503(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"server_is_overloaded\"}}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	e := New(base.Config{BaseURLs: []string{upstream.URL}, TimeoutMs: 5000})
	// Speed up the retry sleep by using a tiny delay via Retry config keyed on 503.
	e.Config.Retry = map[int]base.RetryEntry{http.StatusServiceUnavailable: {Attempts: codexSseRetryAttempts, DelayMs: 1}}
	req := provider.ExecRequest{
		Model:       "gpt-5.3-codex",
		Body:        json.RawMessage(`{"input":"hi"}`),
		Stream:      true,
		Credentials: provider.Credentials{APIKey: "x"},
	}
	resp, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	defer func() {
		if resp.Response != nil {
			resp.Response.Body.Close()
		}
		if resp.Done != nil {
			resp.Done()
		}
	}()
	if resp.Response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 after retries exhausted", resp.Response.StatusCode)
	}
	// 1 initial attempt + codexSseRetryAttempts retries.
	wantCalls := 1 + codexSseRetryAttempts
	if calls != wantCalls {
		t.Errorf("upstream calls = %d, want %d (1 + %d retries)", calls, wantCalls, codexSseRetryAttempts)
	}
}

// TestExecute_NonStreamingPassthrough verifies the peek is skipped for
// non-streaming requests (req.Stream=false).
func TestExecute_NonStreamingPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	e := New(base.Config{BaseURLs: []string{upstream.URL}, TimeoutMs: 5000})
	req := provider.ExecRequest{
		Model:       "gpt-5.3-codex",
		Body:        json.RawMessage(`{"input":"hi"}`),
		Stream:      false,
		Credentials: provider.Credentials{APIKey: "x"},
	}
	resp, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	defer func() {
		if resp.Response != nil {
			resp.Response.Body.Close()
		}
		if resp.Done != nil {
			resp.Done()
		}
	}()
	got, _ := io.ReadAll(resp.Response.Body)
	if string(got) != `{"ok":true}` {
		t.Errorf("non-streaming body = %q, want {\"ok\":true}", string(got))
	}
}
