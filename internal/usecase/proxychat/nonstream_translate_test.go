package proxychat

import (
	"encoding/json"
	"testing"
)

func TestGeminiBodyToOpenAI(t *testing.T) {
	raw := `{"candidates":[{"content":{"parts":[{"text":"Hello!"},{"thought":true,"text":"reasoning here"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7,"thoughtsTokenCount":1},"modelVersion":"gemini-2.5-flash"}`
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	out := geminiBodyToOpenAI(body)
	if out == nil {
		t.Fatal("nil result")
	}
	choices, _ := out["choices"].([]map[string]any)
	if len(choices) != 1 {
		t.Fatalf("choices len = %d", len(choices))
	}
	msg, _ := choices[0]["message"].(map[string]any)
	if msg["content"] != "Hello!" {
		t.Fatalf("content = %v", msg["content"])
	}
	if msg["reasoning_content"] != "reasoning here" {
		t.Fatalf("reasoning = %v", msg["reasoning_content"])
	}
	if choices[0]["finish_reason"] != "stop" {
		t.Fatalf("finish = %v", choices[0]["finish_reason"])
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["prompt_tokens"] != 6 { // 5 + thoughts 1
		t.Fatalf("prompt_tokens = %v", usage["prompt_tokens"])
	}
	if usage["completion_tokens"] != 2 {
		t.Fatalf("completion_tokens = %v", usage["completion_tokens"])
	}
}

func TestClaudeBodyToOpenAI(t *testing.T) {
	raw := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-3","content":[{"type":"thinking","thinking":"pondering"},{"type":"text","text":"Hi there"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	out := claudeBodyToOpenAI(body)
	if out == nil {
		t.Fatal("nil")
	}
	choices, _ := out["choices"].([]map[string]any)
	msg, _ := choices[0]["message"].(map[string]any)
	if msg["content"] != "Hi there" {
		t.Fatalf("content = %v", msg["content"])
	}
	if msg["reasoning_content"] != "pondering" {
		t.Fatalf("reasoning = %v", msg["reasoning_content"])
	}
	if choices[0]["finish_reason"] != "stop" {
		t.Fatalf("finish = %v", choices[0]["finish_reason"])
	}
	if out["model"] != "claude-3" {
		t.Fatalf("model = %v", out["model"])
	}
}

func TestClaudeBodyToOpenAI_ToolUse(t *testing.T) {
	raw := `{"content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
	var body map[string]any
	json.Unmarshal([]byte(raw), &body)
	out := claudeBodyToOpenAI(body)
	choices, _ := out["choices"].([]map[string]any)
	if choices[0]["finish_reason"] != "tool_calls" {
		t.Fatalf("finish = %v", choices[0]["finish_reason"])
	}
	msg, _ := choices[0]["message"].(map[string]any)
	tc, _ := msg["tool_calls"].([]map[string]any)
	if len(tc) != 1 || tc[0]["id"] != "toolu_1" {
		t.Fatalf("tool_calls = %v", tc)
	}
}

func TestClaudeBodyToOpenAI_AlreadyOpenAI(t *testing.T) {
	raw := `{"choices":[{"message":{"content":"x"}}]}`
	var body map[string]any
	json.Unmarshal([]byte(raw), &body)
	if out := claudeBodyToOpenAI(body); out != nil {
		t.Fatalf("expected nil for already-openai, got %v", out)
	}
}
