package ollama

import (
	"encoding/json"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9router/internal/domain/format"
)

// chunkToOpenAI runs one ollama NDJSON chunk through the registered
// ollama->openai response translator with a fresh state map.
func chunkToOpenAI(t *testing.T, raw string, state map[string]any) []map[string]any {
	t.Helper()
	if state == nil {
		state = map[string]any{}
	}
	out, err := translator.TranslateResponse(format.Ollama, format.Openai, json.RawMessage(raw), state)
	if err != nil {
		t.Fatalf("TranslateResponse: %v", err)
	}
	results := make([]map[string]any, 0, len(out))
	for _, b := range out {
		if len(b) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		results = append(results, m)
	}
	return results
}

func deltaContent(m map[string]any) string {
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	c, _ := choices[0].(map[string]any)
	d, _ := c["delta"].(map[string]any)
	s, _ := d["content"].(string)
	return s
}

func deltaReasoning(m map[string]any) string {
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	c, _ := choices[0].(map[string]any)
	d, _ := c["delta"].(map[string]any)
	s, _ := d["reasoning_content"].(string)
	return s
}

func finishReason(m map[string]any) string {
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	c, _ := choices[0].(map[string]any)
	r, _ := c["finish_reason"].(string)
	return r
}

// TestFinalChunkCarriesContent is the regression test for upstream issue
// decolua/9router #2694: when the upstream delivers the final content in the
// SAME chunk as "done": true (e.g. {"message":{"content":"!"},"done":true}),
// the translator must not drop it. Before the fix the done branch returned an
// empty delta immediately, cutting off the last token.
func TestFinalChunkCarriesContent(t *testing.T) {
	chunk := `{"model":"gpt-oss:20b","created_at":"2026-07-18T18:00:00Z","message":{"role":"assistant","content":"!"},"done":true,"done_reason":"stop","eval_count":1,"prompt_eval_count":1,"total_duration":1}`
	out := chunkToOpenAI(t, chunk, nil)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d: %v", len(out), out)
	}
	if got := deltaContent(out[0]); got != "!" {
		t.Fatalf("final-chunk content dropped: got delta.content=%q, want %q", got, "!")
	}
	if got := finishReason(out[0]); got != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got)
	}
	if _, ok := out[0]["usage"]; !ok {
		t.Fatalf("final chunk missing usage")
	}
}

// TestFinalChunkCarriesThinking covers the same cutoff for the thinking field
// when it arrives in the final done chunk.
func TestFinalChunkCarriesThinking(t *testing.T) {
	chunk := `{"model":"kimi","message":{"role":"assistant","thinking":"final thought"},"done":true,"done_reason":"stop"}`
	out := chunkToOpenAI(t, chunk, nil)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}
	if got := deltaReasoning(out[0]); got != "final thought" {
		t.Fatalf("final-chunk thinking dropped: got %q, want %q", got, "final thought")
	}
}

// TestFinalChunkEmptyDoneStillWorks ensures the normal "done" terminator with
// no content still emits a finish chunk with empty delta and usage.
func TestFinalChunkEmptyDoneStillWorks(t *testing.T) {
	state := map[string]any{"ollama": map[string]any{"id": "chatcmpl-1", "created": 0, "model": "ollama"}, "model": "ollama"}
	chunk := `{"model":"ollama","done":true,"done_reason":"stop","eval_count":1,"prompt_eval_count":1}`
	out := chunkToOpenAI(t, chunk, state)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}
	if got := deltaContent(out[0]); got != "" {
		t.Fatalf("expected empty delta content, got %q", got)
	}
	if got := finishReason(out[0]); got != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got)
	}
}

// TestMidStreamChunk exercises a normal non-final content delta.
func TestMidStreamChunk(t *testing.T) {
	state := map[string]any{}
	chunk := `{"model":"ollama","message":{"role":"assistant","content":"Hello"},"done":false}`
	out := chunkToOpenAI(t, chunk, state)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}
	if got := deltaContent(out[0]); got != "Hello" {
		t.Fatalf("delta content = %q, want Hello", got)
	}
	if got := finishReason(out[0]); got != "" {
		t.Fatalf("non-final chunk should have no finish_reason, got %q", got)
	}
}