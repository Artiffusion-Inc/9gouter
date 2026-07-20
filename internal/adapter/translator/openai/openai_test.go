package openai

import (
	"encoding/json"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

func TestRegistryRegistersOpenAIToClaude(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := translator.TranslateRequest(format.Openai, format.Claude, "claude-opus-4-6", body, true, "claude")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if v["model"] != "claude-opus-4-6" {
		t.Errorf("model = %v, want claude-opus-4-6", v["model"])
	}
	if v["stream"] != true {
		t.Errorf("stream = %v, want true", v["stream"])
	}
	if _, ok := v["messages"]; !ok {
		t.Errorf("result missing messages")
	}
}
