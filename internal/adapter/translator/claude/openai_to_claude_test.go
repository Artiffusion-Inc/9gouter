package claude

import (
	"encoding/json"
	"testing"
)

// chunkWithToolCalls builds a minimal OpenAI Chat Completions streaming chunk
// with a single choice whose delta carries the given tool_calls array. The
// body is round-tripped through JSON so []map[string]any becomes []any (the
// shape json.Unmarshal produces), matching what the translator asserts.
func chunkWithToolCalls(toolCalls []map[string]any) map[string]any {
	raw, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-glm-test-1234567890",
		"model":   "glm-4.6",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": toolCalls}}},
	})
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		panic(err)
	}
	return body
}

// countContentBlockStartToolUse counts content_block_start events whose
// content_block type is tool_use.
func countContentBlockStartToolUse(results []map[string]any) int {
	n := 0
	for _, r := range results {
		if r["type"] != "content_block_start" {
			continue
		}
		if cb, ok := r["content_block"].(map[string]any); ok && cb["type"] == claudeBlockToolUse {
			n++
		}
	}
	return n
}

// TestOpenaiToClaudeResponseGLMRepeatsID ports upstream 52623587: GLM/fireworks
// repeat id+null-name on every argument chunk. The OpenAI→Claude response
// translator must open the tool_use content_block exactly ONCE per index — a
// repeated id on a later arg chunk must NOT emit a second content_block_start.
func TestOpenaiToClaudeResponseGLMRepeatsID(t *testing.T) {
	state := map[string]any{}

	// First chunk: opens tool_call index 0 with id "call_1" and name "get_weather".
	c1 := chunkWithToolCalls([]map[string]any{{
		"index": float64(0), "id": "call_1",
		"function": map[string]any{"name": "get_weather", "arguments": ""},
	}})
	r1 := openaiToClaudeResponse(c1, state)
	if got := countContentBlockStartToolUse(r1); got != 1 {
		t.Fatalf("chunk 1: expected 1 tool_use content_block_start, got %d", got)
	}

	// Second chunk: GLM repeats id "call_1" with a null/absent name and an
	// argument delta. Pre-fix this emitted a SECOND content_block_start.
	c2 := chunkWithToolCalls([]map[string]any{{
		"index": float64(0), "id": "call_1",
		"function": map[string]any{"arguments": `{"city":"Paris"}`},
	}})
	r2 := openaiToClaudeResponse(c2, state)
	if got := countContentBlockStartToolUse(r2); got != 0 {
		t.Fatalf("chunk 2 (GLM repeated id): expected 0 new tool_use content_block_start, got %d", got)
	}

	// Third chunk: a DIFFERENT tool index opens a new block (id "call_2").
	c3 := chunkWithToolCalls([]map[string]any{{
		"index": float64(1), "id": "call_2",
		"function": map[string]any{"name": "get_time", "arguments": ""},
	}})
	r3 := openaiToClaudeResponse(c3, state)
	if got := countContentBlockStartToolUse(r3); got != 1 {
		t.Fatalf("chunk 3 (new index): expected 1 tool_use content_block_start, got %d", got)
	}

	// Verify the argument from chunk 2 was still appended to the buffer for idx 0.
	bufs, ok := state["toolArgBuffers"].(map[string]any)
	if !ok {
		t.Fatalf("expected toolArgBuffers in state, got %T", state["toolArgBuffers"])
	}
	if got, _ := bufs["0"].(string); got != `{"city":"Paris"}` {
		t.Fatalf("toolArgBuffers[0] = %q, want %q", got, `{"city":"Paris"}`)
	}
}

// TestOpenaiToClaudeResponseNormalToolCalls ensures the once-per-index gate does
// not break the standard OpenAI shape (id present once, then arg-only chunks).
func TestOpenaiToClaudeResponseNormalToolCalls(t *testing.T) {
	state := map[string]any{}
	c1 := chunkWithToolCalls([]map[string]any{{
		"index": float64(0), "id": "call_x",
		"function": map[string]any{"name": "do_thing", "arguments": ""},
	}})
	r1 := openaiToClaudeResponse(c1, state)
	if got := countContentBlockStartToolUse(r1); got != 1 {
		t.Fatalf("normal chunk 1: expected 1 content_block_start, got %d", got)
	}
	// arg-only chunk (no id) — still no new block.
	c2 := chunkWithToolCalls([]map[string]any{{
		"index": float64(0),
		"function": map[string]any{"arguments": `{"a":1}`},
	}})
	r2 := openaiToClaudeResponse(c2, state)
	if got := countContentBlockStartToolUse(r2); got != 0 {
		t.Fatalf("normal chunk 2: expected 0 content_block_start, got %d", got)
	}
}

// ensure the package compiles with the json import used by potential future cases.
var _ = json.Marshal