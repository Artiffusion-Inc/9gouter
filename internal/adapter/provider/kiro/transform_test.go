package kiroexec

// transform_test.go pins the event dispatch + terminal state machine (#101)
// against hand-built real EventStream frames — no mocks. The tests feed
// assembled frames through Transformer.ProcessBytes / Finish and assert the
// OpenAI SSE output, stop-reason taxonomy, thinking-strip, tool-call
// assembly, and the terminal fail-closed paths, 1:1 with upstream kiro.js.

import (
	"encoding/json"
	"strings"
	"testing"
)

// makeEventFrame builds a complete EventStream frame for a given :event-type +
// JSON payload, with the standard :message-type=event header.
func makeEventFrame(t *testing.T, eventType string, payload map[string]any) []byte {
	t.Helper()
	hdrs := encodeHeader(nil, ":message-type", 7, []byte("event"))
	hdrs = encodeHeader(hdrs, ":event-type", 7, []byte(eventType))
	var payloadBytes []byte
	if payload != nil {
		payloadBytes, _ = json.Marshal(payload)
	}
	return buildFrame(t, hdrs, payloadBytes)
}

// runTransformer feeds a sequence of frames through one Transformer attempt and
// calls Finish, returning the SSE output + terminal state.
func runTransformer(t *testing.T, frames ...[]byte) (*Transformer, string, *TerminalState) {
	t.Helper()
	tr := NewTransformer("chatcmpl-test", 1700000000, "kiro-test", 200000)
	stream := NewFrameStream()
	for _, f := range frames {
		if !tr.ProcessBytes(f, stream) {
			break
		}
	}
	tr.Finish(stream.Remainder())
	return tr, string(tr.Bytes()), tr.Terminal()
}

func TestTransform_AssistantResponseEmitsTextDelta(t *testing.T) {
	f := makeEventFrame(t, "assistantResponseEvent", map[string]any{"content": "Hello"})
	_, out, term := runTransformer(t, f)
	if !strings.Contains(out, `"content":"Hello"`) {
		t.Errorf("missing content delta: %s", out)
	}
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Errorf("first chunk should carry role:assistant: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("missing clean stop finish: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("missing [DONE]: %s", out)
	}
	if term == nil || term.Code != "" {
		t.Errorf("terminal=%v want clean (empty code)", term)
	}
	if term.StopDisposition != "complete" {
		t.Errorf("disposition=%q want complete", term.StopDisposition)
	}
}

func TestTransform_ThinkingStripped(t *testing.T) {
	// One frame opens <thinking>, a second closes it; only the post-thinking
	// content should be emitted. The opening frame keeps text before <thinking>.
	f1 := makeEventFrame(t, "assistantResponseEvent", map[string]any{"content": "before<thinking>secret"})
	f2 := makeEventFrame(t, "assistantResponseEvent", map[string]any{"content": "</thinking>\nafter"})
	_, out, _ := runTransformer(t, f1, f2)
	if strings.Contains(out, "secret") {
		t.Errorf("thinking content leaked into SSE: %s", out)
	}
	if !strings.Contains(out, "before") {
		t.Errorf("pre-thinking text dropped: %s", out)
	}
	if !strings.Contains(out, "after") {
		t.Errorf("post-thinking text dropped: %s", out)
	}
}

func TestTransform_ReasoningAndCodeEvents(t *testing.T) {
	reasoning := makeEventFrame(t, "reasoningContentEvent", map[string]any{
		"reasoningContentEvent": map[string]any{"text": "thinking hard"},
	})
	code := makeEventFrame(t, "codeEvent", map[string]any{"content": "print(1)"})
	_, out, term := runTransformer(t, reasoning, code)
	if !strings.Contains(out, `"reasoning_content":"thinking hard"`) {
		t.Errorf("missing reasoning delta: %s", out)
	}
	if !strings.Contains(out, "print(1)") {
		t.Errorf("missing code delta: %s", out)
	}
	if term.ResponseState != "text_reasoning" {
		t.Errorf("response_state=%q want text_reasoning", term.ResponseState)
	}
}

func TestTransform_MessageStopReason(t *testing.T) {
	text := makeEventFrame(t, "assistantResponseEvent", map[string]any{"content": "hi"})
	stop := makeEventFrame(t, "messageStopEvent", map[string]any{"stopReason": "end_turn"})
	_, out, term := runTransformer(t, text, stop)
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("missing stop finish: %s", out)
	}
	if term.StopReason != "end_turn" {
		t.Errorf("stopReason=%q want end_turn", term.StopReason)
	}
	if term.Provenance != "message_stop_event" {
		t.Errorf("provenance=%q want message_stop_event", term.Provenance)
	}
}

func TestTransform_MaxTokensFinishReason(t *testing.T) {
	text := makeEventFrame(t, "assistantResponseEvent", map[string]any{"content": "partial"})
	stop := makeEventFrame(t, "messageStopEvent", map[string]any{"stopReason": "max_tokens"})
	_, out, term := runTransformer(t, text, stop)
	if !strings.Contains(out, `"finish_reason":"length"`) {
		t.Errorf("missing length finish: %s", out)
	}
	if term.StopDisposition != "length" {
		t.Errorf("disposition=%q want length", term.StopDisposition)
	}
}

func TestTransform_TerminalRefusalFails(t *testing.T) {
	text := makeEventFrame(t, "assistantResponseEvent", map[string]any{"content": "x"})
	stop := makeEventFrame(t, "messageStopEvent", map[string]any{"stopReason": "refusal"})
	_, out, term := runTransformer(t, text, stop)
	if term == nil || term.Code != "kiro_terminal_refusal" {
		t.Errorf("terminal=%v want kiro_terminal_refusal", term)
	}
	if !strings.Contains(out, `"code":"kiro_terminal_refusal"`) {
		t.Errorf("missing refusal error SSE: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("missing [DONE] after error: %s", out)
	}
}

func TestTransform_EmptyResponseFails(t *testing.T) {
	// A stream with only a contextUsageEvent (no text, no stop) → empty_response_eof.
	f := makeEventFrame(t, "contextUsageEvent", map[string]any{"contextUsagePercentage": 50})
	_, out, term := runTransformer(t, f)
	if term == nil || term.Code != "kiro_missing_terminal" {
		t.Errorf("terminal=%v want kiro_missing_terminal", term)
	}
	if !strings.Contains(out, "ended without model output") {
		t.Errorf("missing empty-response error: %s", out)
	}
}

func TestTransform_UpstreamErrorEventFails(t *testing.T) {
	hdrs := encodeHeader(nil, ":message-type", 7, []byte("error"))
	hdrs = encodeHeader(hdrs, ":event-type", 7, []byte("error"))
	payload, _ := json.Marshal(map[string]any{"message": "upstream boom"})
	f := buildFrame(t, hdrs, payload)
	_, out, term := runTransformer(t, f)
	if term == nil || term.Code != "kiro_upstream_eventstream_error" {
		t.Errorf("terminal=%v want kiro_upstream_eventstream_error", term)
	}
	if !strings.Contains(out, "upstream boom") {
		t.Errorf("missing upstream error message: %s", out)
	}
}

func TestTransform_ToolUseAssemblesToolCalls(t *testing.T) {
	// Two fragments for one tool: input split across string fragments, then a
	// messageStopEvent with stop_reason=tool_use.
	tool1 := makeEventFrame(t, "toolUseEvent", map[string]any{
		"name": "search", "toolUseId": "call_1", "input": `{"q": "go"`,
	})
	tool2 := makeEventFrame(t, "toolUseEvent", map[string]any{
		"name": "search", "toolUseId": "call_1", "input": `,"limit": 5}`,
	})
	stop := makeEventFrame(t, "messageStopEvent", map[string]any{"stopReason": "tool_use"})
	_, out, term := runTransformer(t, tool1, tool2, stop)
	if !strings.Contains(out, `"name":"search"`) {
		t.Errorf("missing tool_call name in header delta: %s", out)
	}
	// The assembled arguments are the JSON of {"q":"go","limit":5}; json.Marshal
	// sorts map keys, so the wire order is limit,q.
	if !strings.Contains(out, `"arguments":"{\"limit\":5,\"q\":\"go\"}"`) {
		t.Errorf("missing assembled tool arguments: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("missing tool_calls finish: %s", out)
	}
	if term.StopDisposition != "tool_use" {
		t.Errorf("disposition=%q want tool_use", term.StopDisposition)
	}
}

func TestTransform_ToolUseNestedMCPValidatesName(t *testing.T) {
	// A tool named "tool_call" must carry a nested name+arguments in its input.
	tool := makeEventFrame(t, "toolUseEvent", map[string]any{
		"name": "tool_call", "toolUseId": "call_1", "input": map[string]any{"name": ""},
	})
	stop := makeEventFrame(t, "messageStopEvent", map[string]any{"stopReason": "tool_use"})
	_, out, term := runTransformer(t, tool, stop)
	if term == nil || term.Code != "invalid_kiro_tool_call" {
		t.Errorf("terminal=%v want invalid_kiro_tool_call", term)
	}
	if !strings.Contains(out, "missing nested MCP tool name") {
		t.Errorf("missing nested-name error: %s", out)
	}
}

func TestTransform_CorruptFrameFails(t *testing.T) {
	// Feed a frame with a corrupt prelude CRC directly via ProcessBytes.
	stream := NewFrameStream()
	bad := make([]byte, 16)
	bad[0] = 0
	bad[1] = 0
	bad[2] = 0
	bad[3] = 16 // totalLength=16
	bad[4] = 0
	bad[5] = 0
	bad[6] = 0
	bad[7] = 0 // headersLength=0
	// prelude CRC at [8:12] left zero — won't match.
	tr := NewTransformer("id", 1, "m", 200000)
	if tr.ProcessBytes(bad, stream) {
		t.Fatal("ProcessBytes should return false on corrupt prelude")
	}
	term := tr.Terminal()
	if term == nil || term.Code != "kiro_missing_terminal" {
		t.Errorf("terminal=%v want kiro_missing_terminal", term)
	}
}

func TestTransform_MeteringAndContextUsageSynthesizeUsage(t *testing.T) {
	// metricsEvent would normally set tokens, but with only metering + context
	// usage + text + a stop, the finish synthesizes token counts from
	// contextUsagePct * contextWindow and content length / 4.
	text := makeEventFrame(t, "assistantResponseEvent", map[string]any{"content": "12345678"})
	metering := makeEventFrame(t, "meteringEvent", map[string]any{"usage": 12.0, "unit": "credit"})
	ctxUsage := makeEventFrame(t, "contextUsageEvent", map[string]any{"contextUsagePercentage": 50})
	stop := makeEventFrame(t, "messageStopEvent", map[string]any{"stopReason": "end_turn"})
	_, out, term := runTransformer(t, text, metering, ctxUsage, stop)
	if !strings.Contains(out, `"kiro_credits":12`) {
		t.Errorf("missing kiro_credits: %s", out)
	}
	// prompt = 50% * 200000 = 100000; completion = 8/4 = 2.
	if !strings.Contains(out, `"prompt_tokens":100000`) {
		t.Errorf("missing synthesized prompt_tokens: %s", out)
	}
	if !strings.Contains(out, `"completion_tokens":2`) {
		t.Errorf("missing synthesized completion_tokens: %s", out)
	}
	if term.Usage == nil || term.Usage["total_tokens"] != 100002 {
		t.Errorf("usage=%v want total_tokens=100002", term.Usage)
	}
}

func TestNormalizeStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "end_turn",
		"endTurn":       "end_turn",
		"stop":          "end_turn",
		"stop_sequence": "end_turn",
		"tool_use":      "tool_use",
		"toolUse":       "tool_use",
		"tool_calls":    "tool_use",
		"max_tokens":    "max_tokens",
		"maxTokens":     "max_tokens",
		"length":        "max_tokens",
		"refusal":       "refusal",
		"":              "",
	}
	for in, want := range cases {
		if got := normalizeStopReason(in); got != want {
			t.Errorf("normalizeStopReason(%q)=%q want %q", in, got, want)
		}
	}
}

func TestStopDisposition(t *testing.T) {
	cases := []struct {
		reason string
		tools  bool
		want   string
	}{
		{"malformed_model_output", false, "retryable_protocol_failure"},
		{"cancelled", false, "terminal_incomplete"},
		{"model_context_window_exceeded", false, "terminal_incomplete"},
		{"refusal", false, "terminal_refusal"},
		{"content_filter", false, "terminal_refusal"},
		{"guardrail_block", false, "terminal_refusal"},
		{"max_tokens", false, "length"},
		{"max_tokens", true, "terminal_incomplete"},
		{"end_turn", false, "complete"},
		{"", false, "complete"},
		{"tool_use", false, "tool_use"},
		{"", true, "tool_use"},
		{"weird_reason", false, "unknown_failure"},
	}
	for _, c := range cases {
		if got := stopDisposition(c.reason, c.tools); got != c.want {
			t.Errorf("stopDisposition(%q, %v)=%q want %q", c.reason, c.tools, got, c.want)
		}
	}
}

func TestMergeStopReasonSeverity(t *testing.T) {
	// refusal (6) beats terminal_incomplete (5) beats unknown_failure (4) etc.
	if got := mergeStopReason("max_tokens", "refusal"); got != "refusal" {
		t.Errorf("merge(max_tokens,refusal)=%q want refusal", got)
	}
	if got := mergeStopReason("refusal", "max_tokens"); got != "refusal" {
		t.Errorf("merge(refusal,max_tokens)=%q want refusal", got)
	}
	if got := mergeStopReason("", "end_turn"); got != "end_turn" {
		t.Errorf("merge(empty,end_turn)=%q want end_turn", got)
	}
	if got := mergeStopReason("end_turn", ""); got != "end_turn" {
		t.Errorf("merge(end_turn,empty)=%q want end_turn", got)
	}
}

func TestTruncatedFrameFails(t *testing.T) {
	// Feed half a valid frame so the buffer holds a partial frame at EOF.
	hdrs := encodeHeader(nil, ":message-type", 7, []byte("event"))
	hdrs = encodeHeader(hdrs, ":event-type", 7, []byte("assistantResponseEvent"))
	payload, _ := json.Marshal(map[string]any{"content": "hi"})
	full := buildFrame(t, hdrs, payload)
	half := full[:len(full)/2]
	tr := NewTransformer("id", 1, "m", 200000)
	stream := NewFrameStream()
	tr.ProcessBytes(half, stream)
	tr.Finish(stream.Remainder())
	term := tr.Terminal()
	if term == nil || term.Code != "kiro_missing_terminal" {
		t.Errorf("terminal=%v want kiro_missing_terminal", term)
	}
	if term.TransportState != "incomplete_frame" {
		t.Errorf("transport_state=%q want incomplete_frame", term.TransportState)
	}
	if term.IncompleteBytes != len(half) {
		t.Errorf("incomplete_bytes=%d want %d", term.IncompleteBytes, len(half))
	}
}
