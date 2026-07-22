package kiroexec

// integrity_test.go pins the integrity gate + bounded retry + heuristics (#102)
// against real EventStream bodies — no mocks. The tests build raw EventStream
// frames (the same helpers as eventstream_test.go), feed them through
// RunIntegrityGate with a RetryExec callback that serves a second hand-built
// EventStream, and assert the ellipsis/short_final/invalid_tool retry path,
// the terminal-failure SSE mapping, the maxBytes bound, and the heuristics.

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// bodyOf wraps a byte slice as an io.ReadCloser for RunIntegrityGate.
func bodyOf(b []byte) io.ReadCloser { return io.NopCloser(strings.NewReader(string(b))) }

// completeStream builds a clean assistantResponse + messageStop end_turn stream.
func completeStream(t *testing.T, content string) []byte {
	t.Helper()
	text := makeEventFrame(t, "assistantResponseEvent", map[string]any{"content": content})
	stop := makeEventFrame(t, "messageStopEvent", map[string]any{"stopReason": "end_turn"})
	return append(text, stop...)
}

// noRetry is a RetryExec that fails the test if the gate should not retry.
func noRetry(t *testing.T) RetryExec {
	return func(ctx context.Context, body json.RawMessage) (io.ReadCloser, int, error) {
		t.Fatalf("retry should not be invoked")
		return nil, 0, nil
	}
}

func TestIntegrity_CompleteAttemptNoRetry(t *testing.T) {
	first := completeStream(t, "Hello world")
	res := RunIntegrityGate(context.Background(), bodyOf(first), noRetry(t), "kiro-test", 200000, []byte(`{"messages":[]}`), DefaultIntegrityOptions(true))
	if res.Status != 200 {
		t.Errorf("status=%d want 200", res.Status)
	}
	out := string(res.Bytes)
	if !strings.Contains(out, "Hello world") {
		t.Errorf("missing content: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("missing clean finish: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("missing [DONE]: %s", out)
	}
}

func TestIntegrity_EllipsisTriggersRetry(t *testing.T) {
	// First attempt: a text delta of "..." then end_turn → isEllipsisOnly → retry.
	first := completeStream(t, "...")
	// Retry attempt: a complete clean stream.
	retry := completeStream(t, "The full answer is 42.")
	var retryBody json.RawMessage
	var retryCalled bool
	retryFn := func(ctx context.Context, body json.RawMessage) (io.ReadCloser, int, error) {
		retryCalled = true
		retryBody = body
		return bodyOf(retry), 200, nil
	}
	res := RunIntegrityGate(context.Background(), bodyOf(first), retryFn, "kiro-test", 200000, []byte(`{"systemPrompt":"base","messages":[]}`), DefaultIntegrityOptions(true))
	if !retryCalled {
		t.Fatal("retry was not invoked for ellipsis")
	}
	out := string(res.Bytes)
	if !strings.Contains(out, "The full answer is 42.") {
		t.Errorf("missing retry content: %s", out)
	}
	// Verify the repair instruction was appended to systemPrompt.
	var obj map[string]any
	if err := json.Unmarshal(retryBody, &obj); err != nil {
		t.Fatalf("retry body unmarshal: %v", err)
	}
	sp, _ := obj["systemPrompt"].(string)
	if !strings.Contains(sp, "ended with only an ellipsis") {
		t.Errorf("repair instruction not appended: %q", sp)
	}
	if !strings.HasPrefix(sp, "base") {
		t.Errorf("base systemPrompt lost: %q", sp)
	}
}

func TestIntegrity_ShortFinalTriggersRetry(t *testing.T) {
	// First attempt: a short future-action final ("Let me check the status.") with end_turn.
	// Per isShortFutureAction: englishFutureAction matches, but englishResultClause
	// does NOT (no result clause) → returns true → short_final → retry.
	first := completeStream(t, "Let me check the status.")
	retry := completeStream(t, "Status is healthy: all green.")
	retryFn := func(ctx context.Context, body json.RawMessage) (io.ReadCloser, int, error) {
		return bodyOf(retry), 200, nil
	}
	res := RunIntegrityGate(context.Background(), bodyOf(first), retryFn, "kiro-test", 200000, []byte(`{"messages":[]}`), DefaultIntegrityOptions(true))
	out := string(res.Bytes)
	if !strings.Contains(out, "Status is healthy") {
		t.Errorf("missing retry content: %s", out)
	}
}

func TestIntegrity_EnglishFutureActionWithResultClauseIsComplete(t *testing.T) {
	// "Let me check the status. It is healthy." — englishFutureAction matches AND
	// englishResultClause matches (". It" + "status is") → isShortFutureAction
	// returns false → complete, no retry.
	content := "Let me check the status. It is healthy."
	first := completeStream(t, content)
	res := RunIntegrityGate(context.Background(), bodyOf(first), noRetry(t), "kiro-test", 200000, []byte(`{"messages":[]}`), DefaultIntegrityOptions(true))
	if !strings.Contains(string(res.Bytes), content) {
		t.Errorf("should pass through as complete: %s", res.Bytes)
	}
}

func TestIntegrity_RepairDisabledInvalidToolNoRetry(t *testing.T) {
	// First attempt: a tool_call with an empty nested name → invalid_tool.
	tool := makeEventFrame(t, "toolUseEvent", map[string]any{
		"name": "tool_call", "toolUseId": "call_1", "input": map[string]any{"name": ""},
	})
	stop := makeEventFrame(t, "messageStopEvent", map[string]any{"stopReason": "tool_use"})
	first := append(tool, stop...)
	opts := DefaultIntegrityOptions(false) // repair disabled
	res := RunIntegrityGate(context.Background(), bodyOf(first), noRetry(t), "kiro-test", 200000, []byte(`{"messages":[]}`), opts)
	if !strings.Contains(string(res.Bytes), "invalid_kiro_tool_call") {
		t.Errorf("missing invalid_kiro_tool_call error: %s", res.Bytes)
	}
}

func TestIntegrity_InvalidToolRetryFailsAgain(t *testing.T) {
	tool := makeEventFrame(t, "toolUseEvent", map[string]any{
		"name": "tool_call", "toolUseId": "call_1", "input": map[string]any{"name": ""},
	})
	stop := makeEventFrame(t, "messageStopEvent", map[string]any{"stopReason": "tool_use"})
	first := append(tool, stop...)
	retry := append(tool, stop...) // retry also invalid
	retryFn := func(ctx context.Context, body json.RawMessage) (io.ReadCloser, int, error) {
		return bodyOf(retry), 200, nil
	}
	res := RunIntegrityGate(context.Background(), bodyOf(first), retryFn, "kiro-test", 200000, []byte(`{"messages":[]}`), DefaultIntegrityOptions(true))
	if !strings.Contains(string(res.Bytes), "kiro_tool_call_repair_retry_failed") {
		t.Errorf("missing retry-failed code: %s", res.Bytes)
	}
}

func TestIntegrity_TerminalRefusalNoRetry(t *testing.T) {
	// Build a stream directly: text "x" then a refusal stop reason.
	text := makeEventFrame(t, "assistantResponseEvent", map[string]any{"content": "x"})
	stop := makeEventFrame(t, "messageStopEvent", map[string]any{"stopReason": "refusal"})
	first := append(text, stop...)
	res := RunIntegrityGate(context.Background(), bodyOf(first), noRetry(t), "kiro-test", 200000, []byte(`{"messages":[]}`), DefaultIntegrityOptions(true))
	if !strings.Contains(string(res.Bytes), "kiro_terminal_refusal") {
		t.Errorf("missing refusal error: %s", res.Bytes)
	}
}

func TestIntegrity_RetryUpstreamError(t *testing.T) {
	// First attempt is ellipsis; retry returns HTTP 502.
	first := completeStream(t, "...")
	retryFn := func(ctx context.Context, body json.RawMessage) (io.ReadCloser, int, error) {
		return bodyOf([]byte("upstream 502 body")), 502, nil
	}
	res := RunIntegrityGate(context.Background(), bodyOf(first), retryFn, "kiro-test", 200000, []byte(`{"messages":[]}`), DefaultIntegrityOptions(true))
	out := string(res.Bytes)
	if !strings.Contains(out, "kiro_integrity_retry_upstream_error") {
		t.Errorf("missing retry-upstream-error code: %s", out)
	}
	if !strings.Contains(out, "upstream 502 body") {
		t.Errorf("missing drained retry body: %s", out)
	}
}

func TestIntegrity_MaxBytesBoundFails(t *testing.T) {
	// A huge single text frame exceeds the small maxBytes bound.
	big := makeEventFrame(t, "assistantResponseEvent", map[string]any{"content": strings.Repeat("x", 10000)})
	opts := DefaultIntegrityOptions(true)
	opts.MaxBytes = 1000
	res := RunIntegrityGate(context.Background(), bodyOf(big), noRetry(t), "kiro-test", 200000, []byte(`{"messages":[]}`), opts)
	// The buffer-exceeded path returns a terminal_stop → integrityFailureSSE.
	out := string(res.Bytes)
	if !strings.Contains(out, "kiro_integrity_buffer_exceeded") && !strings.Contains(out, "buffer exceeded") {
		t.Errorf("expected buffer-exceeded failure: %s", out)
	}
}

func TestIsEllipsisOnly(t *testing.T) {
	if !isEllipsisOnly("...") {
		t.Error("... should be ellipsis-only")
	}
	if !isEllipsisOnly("  … ") {
		t.Error("… (trimmed) should be ellipsis-only")
	}
	if isEllipsisOnly("..") || isEllipsisOnly("....") || isEllipsisOnly("text") {
		t.Error("non-ellipsis values should not match")
	}
}

func TestIsShortFutureAction(t *testing.T) {
	cases := map[string]bool{
		"Let me check the status.":                                      true,  // future action, no result clause
		"Let me check the status. It is ok.":                            false, // has result clause
		"I will verify the checksum: abc123":                            false, // has result clause (colon + value)
		"已完成所有检查":                                                       false, // completed_final
		"Please confirm before I proceed":                               false, // user_wait
		"The result shows success":                                      false, // result_evidence
		"Full complete answer with details about everything done here.": false, // too long / completed
	}
	for in, want := range cases {
		if got := isShortFutureAction(in); got != want {
			t.Errorf("isShortFutureAction(%q)=%v want %v", in, got, want)
		}
	}
}

func TestAppendRepairInstruction(t *testing.T) {
	// Appends to an existing systemPrompt.
	out := appendRepairInstruction([]byte(`{"systemPrompt":"base","messages":[]}`), "ellipsis")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sp, _ := obj["systemPrompt"].(string)
	if !strings.HasPrefix(sp, "base\n\n") {
		t.Errorf("base not preserved+separated: %q", sp)
	}
	if !strings.Contains(sp, "ellipsis") {
		t.Errorf("instruction missing: %q", sp)
	}

	// Creates systemPrompt when absent.
	out2 := appendRepairInstruction([]byte(`{"messages":[]}`), "tool")
	if err := json.Unmarshal(out2, &obj); err != nil {
		t.Fatalf("unmarshal2: %v", err)
	}
	sp2, _ := obj["systemPrompt"].(string)
	if !strings.Contains(sp2, "tool_call") || !strings.Contains(sp2, "non-empty name") {
		t.Errorf("tool instruction missing: %q", sp2)
	}

	// Empty body → object with just the instruction.
	out3 := appendRepairInstruction(nil, "short_final")
	if err := json.Unmarshal(out3, &obj); err != nil {
		t.Fatalf("unmarshal3: %v", err)
	}
	sp3, _ := obj["systemPrompt"].(string)
	if !strings.Contains(sp3, "future action") {
		t.Errorf("short_final instruction missing: %q", sp3)
	}
}

func TestInspectSSEChunk(t *testing.T) {
	tr := NewTransformer("id", 1, "m", 200000)
	tr.emitDelta(map[string]any{"content": "hello"})
	tr.emitDelta(map[string]any{"reasoning_content": "thinking"})
	// Build a tool_calls delta by hand.
	toolChunk := tr.sseChunk(map[string]any{
		"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"name": "f"}}},
	}, nil, nil)
	out := &sseInspection{}
	inspectSSEChunk(tr.out, out)
	if out.content != "hello" {
		t.Errorf("content=%q want hello", out.content)
	}
	if out.reasoning != "thinking" {
		t.Errorf("reasoning=%q want thinking", out.reasoning)
	}
	// out was built from content+reasoning deltas only → no tool calls.
	if out.hasToolCalls {
		t.Errorf("content/reasoning-only stream should not set hasToolCalls")
	}
	// Inspect the tool chunk directly.
	out2 := &sseInspection{}
	inspectSSEChunk(toolChunk, out2)
	if !out2.hasToolCalls {
		t.Errorf("tool chunk should set hasToolCalls")
	}
}

func TestSafeDiagnostics(t *testing.T) {
	// nil diag → defaults.
	safe := safeDiagnostics(nil, "initial")
	if safe.Provenance != "missing_terminal_diagnostics" {
		t.Errorf("provenance=%q want missing_terminal_diagnostics", safe.Provenance)
	}
	if safe.StopDisposition != "terminal_incomplete" {
		t.Errorf("disposition=%q want terminal_incomplete", safe.StopDisposition)
	}
	if safe.EventCounts == nil {
		t.Error("EventCounts should be non-nil")
	}
}
