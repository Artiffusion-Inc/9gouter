package openai

import (
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/shared"
)

// freshState returns the InitState map the response translator expects.
func freshState() map[string]any {
	return map[string]any{}
}

// chunkOf builds an event chunk with an optional nested data object.
func chunkOf(eventType string, data map[string]any) map[string]any {
	if data == nil {
		return map[string]any{"type": eventType}
	}
	return map[string]any{"type": eventType, "data": data}
}

// deltaOf extracts the delta map from the choices[0] of an emitted chunk.
func deltaOf(t *testing.T, chunk map[string]any) map[string]any {
	t.Helper()
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in chunk: %v", chunk)
	}
	d, _ := choices[0].(map[string]any)["delta"].(map[string]any)
	if d == nil {
		t.Fatalf("no delta in choice: %v", chunk)
	}
	return d
}

// finishReasonOf extracts finish_reason from choices[0].
func finishReasonOf(t *testing.T, chunk map[string]any) any {
	t.Helper()
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in chunk: %v", chunk)
	}
	return choices[0].(map[string]any)["finish_reason"]
}

// --- openaiResponsesToOpenAIResponse: text deltas ---

func TestResponsesToOpenAIResponseTextDelta(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.output_text.delta", map[string]any{"delta": "hello"}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if deltaOf(t, out[0])["content"] != "hello" {
		t.Errorf("content = %v, want hello", deltaOf(t, out[0])["content"])
	}
	if out[0]["object"] != "chat.completion.chunk" {
		t.Errorf("object = %v, want chat.completion.chunk", out[0]["object"])
	}
}

func TestResponsesToOpenAIResponseTextDeltaEmptyDropped(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.output_text.delta", map[string]any{"delta": ""}), state)
	if out != nil {
		t.Errorf("empty delta should be dropped, got %v", out)
	}
}

func TestResponsesToOpenAIResponseOutputTextDoneDropped(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.output_text.done", nil), state)
	if out != nil {
		t.Errorf("output_text.done should be dropped, got %v", out)
	}
}

// --- tool call flow ---

func TestResponsesToOpenAIResponseFunctionCallAdded(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.output_item.added", map[string]any{
		"item": map[string]any{"type": "function_call", "call_id": "c1", "name": "get_weather"},
	}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	delta := deltaOf(t, out[0])
	tcs, _ := delta["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tool name = %v, want get_weather", fn["name"])
	}
	if tc["id"] != "c1" {
		t.Errorf("tool id = %v, want c1", tc["id"])
	}
	if tc["type"] != "function" {
		t.Errorf("tool type = %v, want function", tc["type"])
	}
	if state["currentToolCallId"] != "c1" {
		t.Errorf("currentToolCallId = %v, want c1", state["currentToolCallId"])
	}
}

func TestResponsesToOpenAIResponseFunctionCallAddedNoCallIDGenerates(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.output_item.added", map[string]any{
		"item": map[string]any{"type": "function_call", "name": "f"},
	}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	id, _ := state["currentToolCallId"].(string)
	if id == "" {
		t.Errorf("expected generated call id, got empty")
	}
}

func TestResponsesToOpenAIResponseCustomToolCallAdded(t *testing.T) {
	// custom_tool_call should be treated identically to function_call.
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.output_item.added", map[string]any{
		"item": map[string]any{"type": "custom_tool_call", "call_id": "c2", "name": "ct"},
	}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	delta := deltaOf(t, out[0])
	tcs, _ := delta["tool_calls"].([]any)
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "ct" {
		t.Errorf("custom tool name = %v, want ct", fn["name"])
	}
}

func TestResponsesToOpenAIResponseFunctionCallArgumentsDelta(t *testing.T) {
	state := freshState()
	// First add the function_call item to initialize state.
	_ = openaiResponsesToOpenAIResponse(chunkOf("response.output_item.added", map[string]any{
		"item": map[string]any{"type": "function_call", "call_id": "c1", "name": "f"},
	}), state)
	out := openaiResponsesToOpenAIResponse(chunkOf("response.function_call_arguments.delta", map[string]any{"delta": "{\"x\":"}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	delta := deltaOf(t, out[0])
	tcs := delta["tool_calls"].([]any)
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != "{\"x\":" {
		t.Errorf("arguments = %v, want {\"x\":", fn["arguments"])
	}
}

func TestResponsesToOpenAIResponseCustomToolCallInputDelta(t *testing.T) {
	state := freshState()
	_ = openaiResponsesToOpenAIResponse(chunkOf("response.output_item.added", map[string]any{
		"item": map[string]any{"type": "custom_tool_call", "call_id": "c1", "name": "f"},
	}), state)
	out := openaiResponsesToOpenAIResponse(chunkOf("response.custom_tool_call_input.delta", map[string]any{"delta": "abc"}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	fn := deltaOf(t, out[0])["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != "abc" {
		t.Errorf("arguments = %v, want abc", fn["arguments"])
	}
}

func TestResponsesToOpenAIResponseOutputItemDoneIncrementsIndex(t *testing.T) {
	state := freshState()
	_ = openaiResponsesToOpenAIResponse(chunkOf("response.output_item.added", map[string]any{
		"item": map[string]any{"type": "function_call", "call_id": "c1", "name": "f"},
	}), state)
	out := openaiResponsesToOpenAIResponse(chunkOf("response.output_item.done", map[string]any{
		"item": map[string]any{"type": "function_call"},
	}), state)
	if out != nil {
		t.Errorf("output_item.done should emit nothing, got %v", out)
	}
	if n, _ := state["toolCallIndex"].(int); n != 1 {
		t.Errorf("toolCallIndex = %v, want 1 after one function_call done", state["toolCallIndex"])
	}
	if state["currentToolCallId"] != nil {
		t.Errorf("currentToolCallId should be cleared after done, got %v", state["currentToolCallId"])
	}
}

func TestResponsesToOpenAIResponseOutputItemAddedNonFunctionDropped(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.output_item.added", map[string]any{
		"item": map[string]any{"type": "message"},
	}), state)
	if out != nil {
		t.Errorf("non-function output_item.added should be dropped, got %v", out)
	}
}

func TestResponsesToOpenAIResponseOutputItemAddedNilItemDropped(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.output_item.added", map[string]any{}), state)
	if out != nil {
		t.Errorf("nil item should be dropped, got %v", out)
	}
}

func TestResponsesToOpenAIResponseOutputItemDoneNonFunctionNoIncrement(t *testing.T) {
	state := freshState()
	_ = openaiResponsesToOpenAIResponse(chunkOf("response.output_text.delta", map[string]any{"delta": "hi"}), state)
	out := openaiResponsesToOpenAIResponse(chunkOf("response.output_item.done", map[string]any{
		"item": map[string]any{"type": "message"},
	}), state)
	if out != nil {
		t.Errorf("non-function done should emit nothing, got %v", out)
	}
	if n, _ := state["toolCallIndex"].(int); n != 0 {
		t.Errorf("toolCallIndex = %v, want 0 (no function_call done)", n)
	}
}

func TestResponsesToOpenAIResponseArgumentsDeltaEmptyDropped(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.function_call_arguments.delta", map[string]any{"delta": ""}), state)
	if out != nil {
		t.Errorf("empty arguments delta should be dropped, got %v", out)
	}
}

// --- completed event with usage ---

func TestResponsesToOpenAIResponseCompletedUsage(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.completed", map[string]any{
		"response": map[string]any{
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 20,
			},
		},
	}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if finishReasonOf(t, out[0]) != "stop" {
		t.Errorf("finish_reason = %v, want stop", finishReasonOf(t, out[0]))
	}
	usage, ok := out[0]["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage not set")
	}
	// BuildUsage emits prompt_tokens / completion_tokens / total_tokens.
	if shared.Number(usage["prompt_tokens"]) != 10 {
		t.Errorf("usage prompt_tokens = %v, want 10", usage["prompt_tokens"])
	}
	if shared.Number(usage["completion_tokens"]) != 20 {
		t.Errorf("usage completion_tokens = %v, want 20", usage["completion_tokens"])
	}
	if shared.Number(usage["total_tokens"]) != 30 {
		t.Errorf("usage total_tokens = %v, want 30", usage["total_tokens"])
	}
}

func TestResponsesToOpenAIResponseCompletedUsagePromptKeys(t *testing.T) {
	// When input_tokens/output_tokens are zero, the translator falls back to
	// prompt_tokens/completion_tokens inside the upstream usage block.
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.completed", map[string]any{
		"response": map[string]any{
			"usage": map[string]any{
				"prompt_tokens":     5,
				"completion_tokens": 7,
			},
		},
	}), state)
	usage, ok := out[0]["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage not set")
	}
	if shared.Number(usage["prompt_tokens"]) != 5 {
		t.Errorf("usage prompt_tokens = %v, want 5", usage["prompt_tokens"])
	}
	if shared.Number(usage["completion_tokens"]) != 7 {
		t.Errorf("usage completion_tokens = %v, want 7", usage["completion_tokens"])
	}
}

func TestResponsesToOpenAIResponseCompletedNoUsageNoUsageKey(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.completed", map[string]any{
		"response": map[string]any{},
	}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if _, ok := out[0]["usage"]; ok {
		t.Errorf("usage should be absent when upstream has none")
	}
}

func TestResponsesToOpenAIResponseDoneAlias(t *testing.T) {
	// response.done should be treated the same as response.completed.
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.done", map[string]any{
		"response": map[string]any{},
	}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1 (response.done alias)", len(out))
	}
	if finishReasonOf(t, out[0]) != "stop" {
		t.Errorf("finish_reason = %v, want stop", finishReasonOf(t, out[0]))
	}
}

func TestResponsesToOpenAIResponseCompletedToolCallsFinishReason(t *testing.T) {
	state := freshState()
	// Add + done a function call to set toolCallIndex=1.
	_ = openaiResponsesToOpenAIResponse(chunkOf("response.output_item.added", map[string]any{
		"item": map[string]any{"type": "function_call", "call_id": "c1", "name": "f"},
	}), state)
	_ = openaiResponsesToOpenAIResponse(chunkOf("response.output_item.done", map[string]any{
		"item": map[string]any{"type": "function_call"},
	}), state)
	out := openaiResponsesToOpenAIResponse(chunkOf("response.completed", map[string]any{
		"response": map[string]any{},
	}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if finishReasonOf(t, out[0]) != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls (toolCallIndex>0)", finishReasonOf(t, out[0]))
	}
}

func TestResponsesToOpenAIResponseCompletedCurrentToolCallFinishReason(t *testing.T) {
	// toolCallIndex still 0 (output_item.done not yet received) but
	// currentToolCallId present → tool_calls.
	state := freshState()
	_ = openaiResponsesToOpenAIResponse(chunkOf("response.output_item.added", map[string]any{
		"item": map[string]any{"type": "function_call", "call_id": "c1", "name": "f"},
	}), state)
	out := openaiResponsesToOpenAIResponse(chunkOf("response.completed", map[string]any{
		"response": map[string]any{},
	}), state)
	if finishReasonOf(t, out[0]) != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls (currentToolCallId set)", finishReasonOf(t, out[0]))
	}
}

func TestResponsesToOpenAIResponseCompletedDedupFinishReason(t *testing.T) {
	// Second response.completed after finishReasonSent → dropped.
	state := freshState()
	_ = openaiResponsesToOpenAIResponse(chunkOf("response.completed", map[string]any{"response": map[string]any{}}), state)
	out := openaiResponsesToOpenAIResponse(chunkOf("response.completed", map[string]any{"response": map[string]any{}}), state)
	if out != nil {
		t.Errorf("second completed should be dropped (finishReasonSent), got %v", out)
	}
}

// --- cached tokens / alternate usage keys ---

func TestResponsesToOpenAIResponseCachedTokens(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.completed", map[string]any{
		"response": map[string]any{
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 20,
				"input_tokens_details": map[string]any{
					"cached_tokens": 5,
				},
			},
		},
	}), state)
	usage, ok := out[0]["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage not set")
	}
	// BuildUsage surfaces cached_tokens inside prompt_tokens_details.
	details, ok := usage["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("prompt_tokens_details not set, got %v", usage)
	}
	if shared.Number(details["cached_tokens"]) != 5 {
		t.Errorf("cached_tokens = %v, want 5", details["cached_tokens"])
	}
}

func TestResponsesToOpenAIResponseCacheReadInputTokens(t *testing.T) {
	// Fallback cached token source: usage["cache_read_input_tokens"].
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.completed", map[string]any{
		"response": map[string]any{
			"usage": map[string]any{
				"input_tokens":            10,
				"output_tokens":           20,
				"cache_read_input_tokens": 3,
			},
		},
	}), state)
	usage, _ := out[0]["usage"].(map[string]any)
	details, _ := usage["prompt_tokens_details"].(map[string]any)
	if shared.Number(details["cached_tokens"]) != 3 {
		t.Errorf("cached_tokens from cache_read_input_tokens = %v, want 3", details["cached_tokens"])
	}
}

// --- error event ---

func TestResponsesToOpenAIResponseErrorEvent(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("error", map[string]any{
		"error": map[string]any{"message": "boom"},
	}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if finishReasonOf(t, out[0]) != "stop" {
		t.Errorf("error finish_reason = %v, want stop", finishReasonOf(t, out[0]))
	}
	if deltaOf(t, out[0])["content"] != "[Error] boom" {
		t.Errorf("error content = %v, want [Error] boom", deltaOf(t, out[0])["content"])
	}
}

func TestResponsesToOpenAIResponseFailedAlias(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.failed", map[string]any{
		"error": map[string]any{"message": "boom2"},
	}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1 (response.failed alias)", len(out))
	}
	if deltaOf(t, out[0])["content"] != "[Error] boom2" {
		t.Errorf("error content = %v, want [Error] boom2", deltaOf(t, out[0])["content"])
	}
}

func TestResponsesToOpenAIResponseErrorNestedInResponse(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("error", map[string]any{
		"response": map[string]any{"error": map[string]any{"message": "deep"}},
	}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if deltaOf(t, out[0])["content"] != "[Error] deep" {
		t.Errorf("error content = %v, want [Error] deep", deltaOf(t, out[0])["content"])
	}
}

func TestResponsesToOpenAIResponseErrorNoMessageMarshalsObject(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("error", map[string]any{
		"error": map[string]any{"code": "rate_limited"},
	}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	content, _ := deltaOf(t, out[0])["content"].(string)
	if content == "[Error] " || content == "" {
		t.Errorf("expected marshaled error object, got %q", content)
	}
	if shared.ExtractReasoningText(map[string]any{}) != "" {
		t.Errorf("ExtractReasoningText sanity: empty map should return empty")
	}
}

func TestResponsesToOpenAIResponseErrorNoErrorObjectDropped(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("error", map[string]any{}), state)
	if out != nil {
		t.Errorf("error event with no error object should be dropped, got %v", out)
	}
}

func TestResponsesToOpenAIResponseErrorAfterFinishDropped(t *testing.T) {
	state := freshState()
	_ = openaiResponsesToOpenAIResponse(chunkOf("response.completed", map[string]any{"response": map[string]any{}}), state)
	out := openaiResponsesToOpenAIResponse(chunkOf("error", map[string]any{"error": map[string]any{"message": "x"}}), state)
	if out != nil {
		t.Errorf("error after finishReasonSent should be dropped, got %v", out)
	}
}

// --- reasoning summary delta ---

func TestResponsesToOpenAIResponseReasoningSummaryDelta(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.reasoning_summary_text.delta", map[string]any{"delta": "thinking"}), state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if deltaOf(t, out[0])["reasoning_content"] != "thinking" {
		t.Errorf("reasoning_content = %v, want thinking", deltaOf(t, out[0])["reasoning_content"])
	}
}

func TestResponsesToOpenAIResponseReasoningSummaryDeltaEmptyDropped(t *testing.T) {
	state := freshState()
	out := openaiResponsesToOpenAIResponse(chunkOf("response.reasoning_summary_text.delta", map[string]any{"delta": ""}), state)
	if out != nil {
		t.Errorf("empty reasoning delta should be dropped, got %v", out)
	}
}

// --- nil chunk flush ---

func TestResponsesToOpenAIResponseNilFlush(t *testing.T) {
	state := freshState()
	// Start the stream with a text delta so state["started"]=true.
	_ = openaiResponsesToOpenAIResponse(chunkOf("response.output_text.delta", map[string]any{"delta": "hi"}), state)
	out := openaiResponsesToOpenAIResponse(nil, state)
	if len(out) != 1 {
		t.Fatalf("nil flush len = %d, want 1", len(out))
	}
	if finishReasonOf(t, out[0]) != "stop" {
		t.Errorf("nil flush finish_reason = %v, want stop", finishReasonOf(t, out[0]))
	}
}

func TestResponsesToOpenAIResponseNilFlushNotStartedDropped(t *testing.T) {
	// nil flush before the stream started → nothing.
	state := freshState()
	if out := openaiResponsesToOpenAIResponse(nil, state); out != nil {
		t.Errorf("nil flush before started should be nil, got %v", out)
	}
}

func TestResponsesToOpenAIResponseNilFlushAfterFinishDropped(t *testing.T) {
	state := freshState()
	_ = openaiResponsesToOpenAIResponse(chunkOf("response.completed", map[string]any{"response": map[string]any{}}), state)
	if out := openaiResponsesToOpenAIResponse(nil, state); out != nil {
		t.Errorf("nil flush after finishReasonSent should be nil, got %v", out)
	}
}

func TestResponsesToOpenAIResponseNilFlushToolCallsFinishReason(t *testing.T) {
	state := freshState()
	_ = openaiResponsesToOpenAIResponse(chunkOf("response.output_item.added", map[string]any{
		"item": map[string]any{"type": "function_call", "call_id": "c1", "name": "f"},
	}), state)
	_ = openaiResponsesToOpenAIResponse(chunkOf("response.output_item.done", map[string]any{
		"item": map[string]any{"type": "function_call"},
	}), state)
	out := openaiResponsesToOpenAIResponse(nil, state)
	if len(out) != 1 {
		t.Fatalf("nil flush len = %d, want 1", len(out))
	}
	if finishReasonOf(t, out[0]) != "tool_calls" {
		t.Errorf("nil flush finish_reason = %v, want tool_calls", finishReasonOf(t, out[0]))
	}
}

// --- unknown event dropped ---

func TestResponsesToOpenAIResponseUnknownEventDropped(t *testing.T) {
	state := freshState()
	if out := openaiResponsesToOpenAIResponse(chunkOf("some.unknown.event", map[string]any{"delta": "x"}), state); out != nil {
		t.Errorf("unknown event should be dropped, got %v", out)
	}
}

// --- event field fallback (chunk["event"] instead of chunk["type"]) ---

func TestResponsesToOpenAIResponseEventFieldFallback(t *testing.T) {
	state := freshState()
	chunk := map[string]any{
		"event": "response.output_text.delta",
		"data":  map[string]any{"delta": "via-event"},
	}
	out := openaiResponsesToOpenAIResponse(chunk, state)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1 (event field fallback)", len(out))
	}
	if deltaOf(t, out[0])["content"] != "via-event" {
		t.Errorf("content = %v, want via-event", deltaOf(t, out[0])["content"])
	}
}

// --- state initialization side effects ---

func TestResponsesToOpenAIResponseInitializesState(t *testing.T) {
	state := freshState()
	_ = openaiResponsesToOpenAIResponse(chunkOf("response.output_text.delta", map[string]any{"delta": "hi"}), state)
	if state["started"] != true {
		t.Errorf("started not set")
	}
	if _, ok := state["chatId"].(string); !ok || state["chatId"] == "" {
		t.Errorf("chatId not initialized, got %v", state["chatId"])
	}
	if state["toolCallIndex"] != 0 {
		t.Errorf("toolCallIndex = %v, want 0", state["toolCallIndex"])
	}
}

// --- buildResponsesOpenAIChunk ---

func TestBuildResponsesOpenAIChunk(t *testing.T) {
	state := map[string]any{
		"chatId":  "chatcmpl-test",
		"created": 42,
		"model":   "gpt-4o",
	}
	c := buildResponsesOpenAIChunk(state, map[string]any{"content": "x"}, "stop")
	if c["id"] != "chatcmpl-test" {
		t.Errorf("id = %v, want chatcmpl-test", c["id"])
	}
	if c["model"] != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", c["model"])
	}
	if c["created"] != 42 {
		t.Errorf("created = %v, want 42", c["created"])
	}
	if c["object"] != "chat.completion.chunk" {
		t.Errorf("object = %v, want chat.completion.chunk", c["object"])
	}
	if finishReasonOf(t, c) != "stop" {
		t.Errorf("finish_reason = %v, want stop", finishReasonOf(t, c))
	}
}

func TestBuildResponsesOpenAIChunkFallbackID(t *testing.T) {
	// Empty chatId → shared.FallbackChatID().
	state := map[string]any{"model": "m"}
	c := buildResponsesOpenAIChunk(state, map[string]any{}, nil)
	id, _ := c["id"].(string)
	if id == "" {
		t.Errorf("expected fallback chat id, got empty")
	}
	if c["model"] != "m" {
		t.Errorf("model = %v, want m", c["model"])
	}
}

func TestBuildResponsesOpenAIChunkDefaultModel(t *testing.T) {
	state := map[string]any{"chatId": "c"}
	c := buildResponsesOpenAIChunk(state, map[string]any{}, nil)
	if c["model"] != "unknown" {
		t.Errorf("model = %v, want unknown (default)", c["model"])
	}
}

func TestBuildResponsesOpenAIChunkNilFinishReason(t *testing.T) {
	state := map[string]any{"chatId": "c"}
	c := buildResponsesOpenAIChunk(state, map[string]any{"content": "x"}, nil)
	// finish_reason should be nil (not "stop") for a mid-stream delta.
	if finishReasonOf(t, c) != nil {
		t.Errorf("finish_reason = %v, want nil for mid-stream delta", finishReasonOf(t, c))
	}
}