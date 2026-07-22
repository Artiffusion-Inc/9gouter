package cursorexec

// models_test.go pins ParseCursorUsableModels — no mocks: build a
// GetUsableModelsResponse protobuf by hand and decode it.

import (
	"strings"
	"testing"
)

func TestParseCursorUsableModels(t *testing.T) {
	// Two ModelDetails entries in field 1 of the response.
	m1 := concat(
		encodeField(modelIDField, wireLen, "gpt-5.3-codex"),
		encodeField(displayModelIDField, wireLen, "gpt-5.3-codex"),
		encodeField(displayNameField, wireLen, "GPT 5.3 Codex"),
		encodeField(displayNameShortField, wireLen, "GPT 5.3"),
	)
	m2 := concat(
		encodeField(modelIDField, wireLen, "cursor-small"),
		// Only id + display_model_id (no display_name) → name falls back.
		encodeField(displayModelIDField, wireLen, "cursor-small"),
	)
	resp := concat(
		encodeField(responseModelsField, wireLen, m1),
		encodeField(responseModelsField, wireLen, m2),
	)

	models := ParseCursorUsableModels(resp)
	if len(models) != 2 {
		t.Fatalf("got %d models want 2: %+v", len(models), models)
	}
	if models[0].ID != "gpt-5.3-codex" || models[0].Name != "GPT 5.3 Codex" {
		t.Errorf("m0 = %+v", models[0])
	}
	if models[1].ID != "cursor-small" || models[1].Name != "cursor-small" {
		t.Errorf("m1 = %+v (name should fall back to id)", models[1])
	}
}

func TestParseCursorUsableModelsDedup(t *testing.T) {
	// Two entries with the same id → one kept.
	m := encodeField(modelIDField, wireLen, "dup-id")
	resp := concat(
		encodeField(responseModelsField, wireLen, m),
		encodeField(responseModelsField, wireLen, m),
	)
	models := ParseCursorUsableModels(resp)
	if len(models) != 1 {
		t.Fatalf("dedup got %d want 1", len(models))
	}
}

func TestParseCursorUsableModelsShortNameFallback(t *testing.T) {
	// No display_name, but display_name_short present → short name used.
	m := concat(
		encodeField(modelIDField, wireLen, "x"),
		encodeField(displayNameShortField, wireLen, "X Short"),
	)
	resp := encodeField(responseModelsField, wireLen, m)
	models := ParseCursorUsableModels(resp)
	if len(models) != 1 || models[0].Name != "X Short" {
		t.Fatalf("got %+v want name X Short", models)
	}
}

func TestParseCursorUsableModelsTrims(t *testing.T) {
	m := concat(
		encodeField(modelIDField, wireLen, "  trim-me  "),
		encodeField(displayNameField, wireLen, "  Name  "),
	)
	resp := encodeField(responseModelsField, wireLen, m)
	models := ParseCursorUsableModels(resp)
	if len(models) != 1 {
		t.Fatalf("got %d", len(models))
	}
	if models[0].ID != "trim-me" || models[0].Name != "Name" {
		t.Errorf("not trimmed: %+v", models[0])
	}
	if strings.Contains(models[0].ID, " ") {
		t.Errorf("id has spaces: %q", models[0].ID)
	}
}