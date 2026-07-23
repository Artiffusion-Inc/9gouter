package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

// mustJSON marshals v or fails the test.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestClaudeToOpenAIBasic ports the core shape: model, stream, max_tokens,
// temperature, a single user message round-trip, and system text collapsing.
func TestClaudeToOpenAIBasic(t *testing.T) {
	body := map[string]any{
		"max_tokens":  float64(512),
		"temperature": 0.5,
		"system":      "You are helpful.",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	out := claudeToOpenAIRequest("claude-opus-4.8", body, true)
	if out["model"] != "claude-opus-4.8" {
		t.Errorf("model = %v", out["model"])
	}
	if out["stream"] != true {
		t.Errorf("stream = %v", out["stream"])
	}
	if mt, _ := out["max_tokens"].(float64); mt != 512 {
		t.Errorf("max_tokens = %v", out["max_tokens"])
	}
	if temp, _ := out["temperature"].(float64); temp != 0.5 {
		t.Errorf("temperature = %v", out["temperature"])
	}
	msgs := out["messages"].([]any)
	// First is the system message; second is the user turn.
	if len(msgs) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(msgs))
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "You are helpful." {
		t.Errorf("system message = %#v", sys)
	}
	user := msgs[1].(map[string]any)
	if user["role"] != "user" || user["content"] != "hi" {
		t.Errorf("user message = %#v", user)
	}
}

// TestClaudeSystemBlocks verifies a Claude system block-array collapses to one
// system message (join of text blocks), with the billing header stripped.
func TestClaudeSystemBlocks(t *testing.T) {
	body := map[string]any{
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: foo\nYou are helpful."},
			map[string]any{"type": "text", "text": "Be terse."},
		},
		"messages": []any{},
	}
	out := claudeToOpenAIRequest("claude-opus-4.8", body, false)
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("len = %d, want 1 system message", len(msgs))
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" {
		t.Errorf("role = %v", sys["role"])
	}
	c, _ := sys["content"].(string)
	if !strings.Contains(c, "You are helpful.") || !strings.Contains(c, "Be terse.") {
		t.Errorf("system content = %q", c)
	}
	if strings.Contains(c, "x-anthropic-billing-header:") {
		t.Errorf("billing header not stripped: %q", c)
	}
}

// TestMidConversationSystemWrapsInInstructions ports upstream 749c2e3f: a
// mid-conversation role:system message becomes a user turn wrapped in
// <instructions>…</instructions>.
func TestMidConversationSystemWrapsInInstructions(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": "hi there"},
			map[string]any{"role": "system", "content": "Now answer only in haiku."},
			map[string]any{"role": "user", "content": "tell me about go"},
		},
	}
	out := claudeToOpenAIRequest("claude-opus-4.8", body, false)
	msgs := out["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("len = %d, want 4", len(msgs))
	}
	third := msgs[2].(map[string]any)
	if third["role"] != "user" {
		t.Errorf("mid-conversation system role = %v, want user", third["role"])
	}
	c, _ := third["content"].(string)
	if !strings.HasPrefix(c, "<instructions>") || !strings.HasSuffix(c, "</instructions>") {
		t.Errorf("mid-conversation system content not wrapped: %q", c)
	}
	if !strings.Contains(c, "Now answer only in haiku.") {
		t.Errorf("mid-conversation system content lost text: %q", c)
	}
}

// TestMidConversationSystemBlocks wraps a block-array system message too.
func TestMidConversationSystemBlocks(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "prefill"},
			}},
		},
	}
	out := claudeToOpenAIRequest("claude-opus-4.8", body, false)
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("len = %d, want 1", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "user" {
		t.Errorf("role = %v, want user", m["role"])
	}
	c, _ := m["content"].(string)
	if !strings.Contains(c, "prefill") || !strings.Contains(c, "<instructions>") {
		t.Errorf("content = %q", c)
	}
}

// TestCarryReasoningEffort ports upstream 3a866fe1: reasoning_effort and
// reasoning carry into the OpenAI result.
func TestCarryReasoningEffort(t *testing.T) {
	// Direct reasoning_effort.
	body := map[string]any{
		"reasoning_effort": "high",
		"messages":         []any{},
	}
	out := claudeToOpenAIRequest("claude-opus-4.8", body, false)
	if re, _ := out["reasoning_effort"].(string); re != "high" {
		t.Errorf("reasoning_effort = %v, want high", out["reasoning_effort"])
	}

	// reasoning.effort fallback.
	body2 := map[string]any{
		"reasoning": map[string]any{"effort": "low"},
		"messages":  []any{},
	}
	out2 := claudeToOpenAIRequest("claude-opus-4.8", body2, false)
	if re, _ := out2["reasoning_effort"].(string); re != "low" {
		t.Errorf("reasoning.effort fallback = %v, want low", out2["reasoning_effort"])
	}
}

// TestToolUseAndResultConversion verifies a tool_use assistant turn becomes an
// assistant message with tool_calls, and a tool_result becomes a tool reply.
func TestToolUseAndResultConversion(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "text", "text": "Looking that up."},
					map[string]any{
						"type":  "tool_use",
						"id":    "tu_1",
						"name":  "search",
						"input": map[string]any{"q": "go 1.26"},
					},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "tu_1",
						"content":     "go 1.26 released",
					},
				},
			},
		},
	}
	out := claudeToOpenAIRequest("claude-opus-4.8", body, false)
	msgs := out["messages"].([]any)
	// Assistant message with tool_calls.
	asst := msgs[0].(map[string]any)
	if asst["role"] != "assistant" {
		t.Errorf("assistant role = %v", asst["role"])
	}
	if c, _ := asst["content"].(string); c != "Looking that up." {
		t.Errorf("assistant content = %#v, want \"Looking that up.\"", asst["content"])
	}
	calls, ok := asst["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %#v", asst["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if call["id"] != "tu_1" || call["type"] != "function" {
		t.Errorf("tool_call = %#v", call)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "search" {
		t.Errorf("function.name = %v", fn["name"])
	}
	if args, _ := fn["arguments"].(string); !strings.Contains(args, "go 1.26") {
		t.Errorf("arguments = %q", args)
	}
	// Tool result -> tool reply.
	tool := msgs[1].(map[string]any)
	if tool["role"] != "tool" {
		t.Errorf("tool role = %v", tool["role"])
	}
	if tool["tool_call_id"] != "tu_1" {
		t.Errorf("tool_call_id = %v", tool["tool_call_id"])
	}
	if c, _ := tool["content"].(string); c != "go 1.26 released" {
		t.Errorf("tool content = %v", tool["content"])
	}
}

// TestImageBase64Conversion verifies a Claude base64 image block becomes an
// OpenAI image_url with a data: URI.
func TestImageBase64Conversion(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "what is this?"},
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/png",
							"data":       "iVBORw0KGgo=",
						},
					},
				},
			},
		},
	}
	out := claudeToOpenAIRequest("claude-opus-4.8", body, false)
	msgs := out["messages"].([]any)
	user := msgs[0].(map[string]any)
	if user["role"] != "user" {
		t.Errorf("role = %v", user["role"])
	}
	// Mixed content (text + image) stays as an array.
	parts, ok := user["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v, want 2-part array", user["content"])
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Errorf("image type = %v", img["type"])
	}
	iu, _ := img["image_url"].(map[string]any)
	url, _ := iu["url"].(string)
	want := "data:image/png;base64,iVBORw0KGgo="
	if url != want {
		t.Errorf("url = %q, want %q", url, want)
	}
}

// TestFixMissingToolResponses verifies the insert path: an assistant tool_call
// without a following tool reply gets a synthetic "[No response received]" reply.
func TestFixMissingToolResponses(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type":  "tool_use",
						"id":    "tu_orphan",
						"name":  "search",
						"input": map[string]any{"q": "x"},
					},
				},
			},
			map[string]any{"role": "user", "content": "next"},
		},
	}
	out := claudeToOpenAIRequest("claude-opus-4.8", body, false)
	msgs := out["messages"].([]any)
	// Expected: assistant(tool_calls), tool("[No response received]"), user.
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3 (missing tool reply inserted)", len(msgs))
	}
	tool := msgs[1].(map[string]any)
	if tool["role"] != "tool" {
		t.Errorf("inserted role = %v, want tool", tool["role"])
	}
	if tool["tool_call_id"] != "tu_orphan" {
		t.Errorf("inserted tool_call_id = %v", tool["tool_call_id"])
	}
	if c, _ := tool["content"].(string); c != "[No response received]" {
		t.Errorf("inserted content = %q", c)
	}
}

// TestFixMissingToolResponsesPartial verifies that a partial response (one of
// two tool_calls answered) inserts a reply only for the unanswered call.
func TestFixMissingToolResponsesPartial(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": "a", "name": "f", "input": map[string]any{}},
					map[string]any{"type": "tool_use", "id": "b", "name": "g", "input": map[string]any{}},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "a", "content": "ok"},
				},
			},
		},
	}
	out := claudeToOpenAIRequest("claude-opus-4.8", body, false)
	msgs := out["messages"].([]any)
	// assistant(tool_calls), tool(a), tool(b-missing).
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3", len(msgs))
	}
	if msgs[1].(map[string]any)["tool_call_id"] != "a" {
		t.Errorf("first reply id = %v", msgs[1].(map[string]any)["tool_call_id"])
	}
	miss := msgs[2].(map[string]any)
	if miss["tool_call_id"] != "b" || miss["content"] != "[No response received]" {
		t.Errorf("inserted missing reply = %#v", miss)
	}
}

// TestConvertToolChoiceVariants covers each branch of convertToolChoice.
func TestConvertToolChoiceVariants(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want any
	}{
		{"nil", nil, "auto"},
		{"string-auto", "auto", "auto"},
		{"string-required", "required", "required"},
		{"obj-auto", map[string]any{"type": "auto"}, "auto"},
		{"obj-any", map[string]any{"type": "any"}, "required"},
		{"obj-tool", map[string]any{"type": "tool", "name": "lookup"},
			map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}},
		{"obj-unknown", map[string]any{"type": "weird"}, "auto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := convertToolChoice(tc.in)
			wantJSON, _ := json.Marshal(tc.want)
			gotJSON, _ := json.Marshal(got)
			if string(wantJSON) != string(gotJSON) {
				t.Errorf("convertToolChoice(%v) = %s, want %s", tc.in, gotJSON, wantJSON)
			}
		})
	}
}

// TestToolsMapping verifies Claude tools (name/description/input_schema) become
// OpenAI function tools with parameters = input_schema.
func TestToolsMapping(t *testing.T) {
	body := map[string]any{
		"messages": []any{},
		"tools": []any{
			map[string]any{
				"name":        "get_weather",
				"description": "Get the weather",
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"city": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
	out := claudeToOpenAIRequest("claude-opus-4.8", body, false)
	tools, ok := out["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", out["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("type = %v", tool["type"])
	}
	fn := tool["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("name = %v", fn["name"])
	}
	if fn["description"] != "Get the weather" {
		t.Errorf("description = %v", fn["description"])
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Errorf("parameters = %#v", fn["parameters"])
	}
	if params["type"] != "object" {
		t.Errorf("parameters.type = %v", params["type"])
	}
}

// TestRegistryTranslation runs the registered translator end-to-end, proving
// the init() registration works and the whole pipe produces valid JSON.
func TestRegistryTranslation(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"max_tokens": float64(1024),
		"system":     "You are helpful.",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	out, err := translator.TranslateRequest(format.Claude, format.Openai, "claude-opus-4.8", body, true, "")
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res["model"] != "claude-opus-4.8" {
		t.Errorf("model = %v", res["model"])
	}
	if res["stream"] != true {
		t.Errorf("stream = %v", res["stream"])
	}
	msgs, ok := res["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %#v", res["messages"])
	}
}
