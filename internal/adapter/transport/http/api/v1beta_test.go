package api

import (
	"testing"
)

func TestBuildGeminiModelsList_OllamaPrefixed(t *testing.T) {
	models := buildGeminiModelsList()
	if len(models) == 0 {
		t.Fatalf("no models returned; ollama catalog should be present")
	}
	var foundOllama, foundGpt bool
	for _, m := range models {
		if m.Name == "models/ollama/gpt-oss:120b" {
			foundOllama = true
			if m.DisplayName != "GPT OSS 120B" {
				t.Errorf("displayName = %q, want %q", m.DisplayName, "GPT OSS 120B")
			}
			if m.InputTokenLimit != 128000 || m.OutputTokenLimit != 8192 {
				t.Errorf("limits = %d/%d", m.InputTokenLimit, m.OutputTokenLimit)
			}
			if !contains(m.SupportedMethods, "generateContent") {
				t.Errorf("missing generateContent: %v", m.SupportedMethods)
			}
		}
	}
	_ = foundGpt
	if !foundOllama {
		t.Fatalf("models/ollama/gpt-oss:120b not present")
	}
}

// TestBuildGeminiModelsList_GeminiDoubleEntry verifies the Gemini-specific
// bare `models/<id>` entry with stream-capable methods.
func TestBuildGeminiModelsList_GeminiDoubleEntry(t *testing.T) {
	models := buildGeminiModelsList()
	var foundPrefixed, foundBare bool
	for _, m := range models {
		if m.Name == "models/gemini/gemini-2.5-pro" {
			foundPrefixed = true
			if !contains(m.SupportedMethods, "generateContent") {
				t.Errorf("prefixed gemini missing generateContent: %v", m.SupportedMethods)
			}
		}
		if m.Name == "models/gemini-2.5-pro" {
			foundBare = true
			if !contains(m.SupportedMethods, "streamGenerateContent") {
				t.Errorf("bare gemini missing streamGenerateContent: %v", m.SupportedMethods)
			}
		}
		// non-gemini providers must never produce a bare models/<id> entry.
		if m.Name == "models/gpt-oss:120b" {
			t.Fatalf("non-gemini provider produced bare entry: %s", m.Name)
		}
	}
	if !foundPrefixed {
		t.Fatalf("models/gemini/gemini-2.5-pro not present (catalog missing?)")
	}
	if !foundBare {
		t.Fatalf("models/gemini-2.5-pro (bare Gemini entry) not present")
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}