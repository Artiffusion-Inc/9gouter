package gemini

// tools_dedup_test.go pins the 639f1204 function-name deduplication in
// openaiToGeminiBase: Gemini generateContent rejects duplicate tool names
// ("Tool names must be unique"). Two client tools that sanitize to the same
// name — or the same tool sent twice — must keep only the first declaration.
// Exercised via the real translator registry (init-time registration), no
// mocks.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

func TestOpenaiToGemini_DedupesDuplicateToolNames(t *testing.T) {
	// The same tool name sent twice (a client bug or a merged-tool list) must
	// keep only the first declaration — Gemini rejects "Tool names must be
	// unique". The dedup operates on the sanitized name.
	body := `{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}],"tools":[` +
		`{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}},` +
		`{"type":"function","function":{"name":"get_weather","description":"w2","parameters":{"type":"object"}}},` +
		`{"type":"function","function":{"name":"get_weather","description":"w3","parameters":{"type":"object"}}}` +
		`]}`
	out, err := translator.TranslateRequest(format.Openai, format.Gemini, "gemini-2.5-pro", json.RawMessage(body), false, "gemini")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want exactly one group", got["tools"])
	}
	group, _ := tools[0].(map[string]any)
	fds, ok := group["functionDeclarations"].([]any)
	if !ok {
		t.Fatalf("functionDeclarations missing: %v", group)
	}
	if len(fds) != 1 {
		var names []string
		for _, fd := range fds {
			if m, ok := fd.(map[string]any); ok {
				names = append(names, m["name"].(string))
			}
		}
		t.Fatalf("functionDeclarations = %d (%v), want 1 (dedup of repeated get_weather)", len(fds), names)
	}
	first, _ := fds[0].(map[string]any)
	if first["name"] != "get_weather" {
		t.Errorf("first declaration name = %v, want get_weather", first["name"])
	}
	// The first declaration's description is preserved (the dedup keeps the
	// first occurrence, not a later one).
	if first["description"] != "w" {
		t.Errorf("first declaration description = %v, want w (first occurrence kept)", first["description"])
	}
}

func TestOpenaiToGemini_KeepsDistinctToolNames(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[` +
		`{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}},` +
		`{"type":"function","function":{"name":"get_time","description":"t","parameters":{"type":"object"}}}` +
		`]}`
	out, err := translator.TranslateRequest(format.Openai, format.Gemini, "m", json.RawMessage(body), false, "gemini")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	tools, _ := got["tools"].([]any)
	group, _ := tools[0].(map[string]any)
	fds, _ := group["functionDeclarations"].([]any)
	if len(fds) != 2 {
		t.Fatalf("functionDeclarations = %d, want 2 (distinct names kept)", len(fds))
	}
	joined := ""
	for _, fd := range fds {
		if m, ok := fd.(map[string]any); ok {
			joined += m["name"].(string) + ","
		}
	}
	if !strings.Contains(joined, "get_weather") || !strings.Contains(joined, "get_time") {
		t.Errorf("declarations = %s, want both get_weather and get_time", joined)
	}
}

// TestOpenaiToGemini_SanitizeThenDedup ensures dedup operates on the SANITIZED
// name: a tool whose raw name needs leading-underscore sanitization collides
// with one already named with that sanitized form.
func TestOpenaiToGemini_SanitizeThenDedup(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[` +
		`{"type":"function","function":{"name":"123bad","description":"a","parameters":{"type":"object"}}},` +
		`{"type":"function","function":{"name":"_123bad","description":"b","parameters":{"type":"object"}}}` +
		`]}`
	out, err := translator.TranslateRequest(format.Openai, format.Gemini, "m", json.RawMessage(body), false, "gemini")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	tools, _ := got["tools"].([]any)
	group, _ := tools[0].(map[string]any)
	fds, _ := group["functionDeclarations"].([]any)
	if len(fds) != 1 {
		t.Fatalf("functionDeclarations = %d, want 1 (123bad sanitizes to _123bad, dedup)", len(fds))
	}
}
