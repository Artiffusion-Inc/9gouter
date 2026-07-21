package ollama

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

func TestOpenaiToOllamaRequest_ContentArrayToString(t *testing.T) {
	body := `{"model":"ollama/minimax-m3","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}],"max_tokens":16,"temperature":0.5,"top_p":0.9}`
	out, err := translator.TranslateRequest(format.Openai, format.Ollama, "minimax-m3", json.RawMessage(body), false, "ollama")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d", len(msgs))
	}
	msg, _ := msgs[0].(map[string]any)
	// content must be a STRING, not an array — this is the 400 root cause.
	if c, ok := msg["content"].([]any); ok {
		t.Fatalf("content still an array: %v", c)
	}
	if msg["content"] != "hello" {
		t.Fatalf("content = %v want hello", msg["content"])
	}
	// image moved to images[] as raw base64 (no data: prefix).
	images, _ := msg["images"].([]any)
	if len(images) != 1 || images[0] != "QUJD" {
		t.Fatalf("images = %v", images)
	}
	opts, _ := got["options"].(map[string]any)
	if opts["num_predict"] != float64(16) {
		t.Fatalf("num_predict = %v", opts["num_predict"])
	}
	if opts["temperature"] != 0.5 {
		t.Fatalf("temperature = %v", opts["temperature"])
	}
	if opts["top_p"] != 0.9 {
		t.Fatalf("top_p = %v", opts["top_p"])
	}
	if got["model"] != "minimax-m3" {
		t.Fatalf("model = %v", got["model"])
	}
}

func TestOpenaiToOllamaRequest_ToolMessage(t *testing.T) {
	body := `{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"72F"}]}`
	out, err := translator.TranslateRequest(format.Openai, format.Ollama, "m", json.RawMessage(body), false, "ollama")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(out, &got)
	msgs, _ := got["messages"].([]any)
	// assistant with tool_calls + tool result mapped to tool_name
	var toolMsg map[string]any
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "tool" {
			toolMsg = mm
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool message")
	}
	if toolMsg["tool_name"] != "get_weather" {
		t.Fatalf("tool_name = %v", toolMsg["tool_name"])
	}
	if toolMsg["content"] != "72F" {
		t.Fatalf("content = %v", toolMsg["content"])
	}
}

func TestOpenaiToOllamaRequest_StringContent(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"plain string"}]}`
	out, _ := translator.TranslateRequest(format.Openai, format.Ollama, "m", json.RawMessage(body), false, "ollama")
	if !strings.Contains(string(out), `"content":"plain string"`) {
		t.Fatalf("output missing string content: %s", out)
	}
}