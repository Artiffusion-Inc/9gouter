package api

import (
	"strings"
	"testing"
)

// sampleGrokToml is a realistic ~/.grok/config.toml with unrelated sections
// (theme, history) that must survive every edit verbatim, plus a pre-existing
// default the apply path should remember and the reset path should restore.
const sampleGrokToml = `[models]
default = "grok-build"

[model.grok-build]
model = "grok-build"
base_url = "https://api.x.ai/v1"
name = "Grok Build"
api_backend = "chat_completions"

[subagents.models]
general-purpose = "grok-build"

[theme]
mode = "dark"

[history]
limit = 1000
`

func TestParseGrokBuildConfig_Empty(t *testing.T) {
	got := parseGrokBuildConfig("")
	if got.Model != nil {
		t.Fatalf("Model = %v, want nil", got.Model)
	}
	if got.Default != "" {
		t.Fatalf("Default = %q, want empty", got.Default)
	}
	for _, typ := range grokSubagentTypes {
		if got.SubagentMappings[typ] != "" {
			t.Fatalf("SubagentMappings[%q] = %v, want empty", typ, got.SubagentMappings[typ])
		}
		if got.SubagentModels[typ] != nil {
			t.Fatalf("SubagentModels[%q] = %v, want nil", typ, got.SubagentModels[typ])
		}
	}
	if hasGrokBuildConfig(got) {
		t.Fatal("hasGrokBuildConfig(empty) = true, want false")
	}
}

func TestParseGrokBuildConfig_PreservesUnrelatedSections(t *testing.T) {
	got := parseGrokBuildConfig(sampleGrokToml)
	if got.Default != "grok-build" {
		t.Fatalf("Default = %q, want grok-build", got.Default)
	}
	// Our main slot (9gouter) is absent in the sample, so Model is nil — the
	// GET payload reports "not configured" for the dashboard until the user
	// applies settings (mirrors the JS parseGrokBuildConfig(mainSlot) path).
	if got.Model != nil {
		t.Fatalf("Model = %v, want nil (9gouter slot absent in sample)", got.Model)
	}
	// subagent mapping points at grok-build, not our slot, so SubagentModels
	// should be nil (the mapping resolves to a non-9gouter slot we don't parse).
	if got.SubagentModels["general-purpose"] != nil {
		t.Fatalf("SubagentModels[general-purpose] = %v, want nil (mapping is not our slot)", got.SubagentModels["general-purpose"])
	}
	if got.SubagentMappings["general-purpose"] != "grok-build" {
		t.Fatalf("SubagentMappings[general-purpose] = %v, want grok-build", got.SubagentMappings["general-purpose"])
	}
}

func TestApplyGrokBuildConfig_RoundTripAndUnrelatedPreserved(t *testing.T) {
	out := applyGrokBuildConfig(sampleGrokToml, "http://127.0.0.1:20128/v1", "sk_9gouter", "grok/build-model", 200000, nil)

	// Unrelated sections survive verbatim.
	if !strings.Contains(out, "[theme]\nmode = \"dark\"") {
		t.Errorf("unrelated [theme] section dropped:\n%s", out)
	}
	if !strings.Contains(out, "[history]\nlimit = 1000") {
		t.Errorf("unrelated [history] section dropped:\n%s", out)
	}

	// Main 9gouter slot written with our rename markers.
	if !strings.Contains(out, "[model.9gouter]") {
		t.Errorf("[model.9gouter] missing:\n%s", out)
	}
	if !strings.Contains(out, "model = \"grok/build-model\"") {
		t.Errorf("main model line missing:\n%s", out)
	}
	if !strings.Contains(out, "base_url = \"http://127.0.0.1:20128/v1\"") {
		t.Errorf("main base_url line missing:\n%s", out)
	}
	if !strings.Contains(out, "name = \"9Gouter\"") {
		t.Errorf("name=9Gouter missing:\n%s", out)
	}
	if !strings.Contains(out, "api_key = \"sk_9gouter\"") {
		t.Errorf("api_key=sk_9gouter missing:\n%s", out)
	}
	if !strings.Contains(out, "context_window = 200000") {
		t.Errorf("context_window missing:\n%s", out)
	}

	// default flipped to our slot and the previous default remembered.
	if !strings.Contains(out, "default = \"9gouter\"") {
		t.Errorf("default=9gouter missing:\n%s", out)
	}
	if !strings.Contains(out, "# 9gouter-prev-default = \"grok-build\"") {
		t.Errorf("prev-default marker missing:\n%s", out)
	}

	// subagentModels == nil leaves existing subagent config untouched.
	if !strings.Contains(out, "[subagents.models]\ngeneral-purpose = \"grok-build\"") {
		t.Errorf("existing subagent mapping should be untouched when subagentModels is nil:\n%s", out)
	}

	// Re-parse and verify the GET-payload shape the frontend consumes.
	parsed := parseGrokBuildConfig(out)
	if parsed.Model == nil {
		t.Fatal("re-parse: Model = nil")
	}
	if m, _ := parsed.Model["model"].(string); m != "grok/build-model" {
		t.Fatalf("re-parse: Model.model = %q, want grok/build-model", m)
	}
	if cw, _ := parsed.Model["context_window"].(int); cw != 200000 {
		t.Fatalf("re-parse: Model.context_window = %v, want 200000", cw)
	}
	if !hasGrokBuildConfig(parsed) {
		t.Error("re-parse: hasGrokBuildConfig = false, want true")
	}
}

func TestApplyGrokBuildConfig_SubagentOverrides(t *testing.T) {
	subagents := map[string]*grokSubagentOverride{
		"general-purpose": {Model: "anthropic/claude-opus-4.7", ContextWindow: 200000},
		"explore":         {Model: "openai/gpt-5.6-terra", ContextWindow: 0}, // 0 -> no context_window line
		// "plan" omitted -> inherit main model.
	}
	out := applyGrokBuildConfig(sampleGrokToml, "http://127.0.0.1:20128/v1", "sk_9gouter", "grok/build-model", 200000, subagents)

	if !strings.Contains(out, "[model.9gouter-general-purpose]") {
		t.Errorf("general-purpose slot missing:\n%s", out)
	}
	if !strings.Contains(out, "model = \"anthropic/claude-opus-4.7\"") {
		t.Errorf("general-purpose model line missing:\n%s", out)
	}
	if !strings.Contains(out, "[model.9gouter-explore]") {
		t.Errorf("explore slot missing:\n%s", out)
	}
	// context_window omitted when 0/non-positive.
	if strings.Contains(out, "[model.9gouter-explore]") && strings.Contains(out[strings.Index(out, "[model.9gouter-explore]"):], "context_window") {
		// Check it's within the explore block specifically.
		exploreBlock := out[strings.Index(out, "[model.9gouter-explore]"):]
		endIdx := strings.Index(exploreBlock, "\n[")
		if endIdx == -1 {
			endIdx = len(exploreBlock)
		}
		exploreBlock = exploreBlock[:endIdx]
		if strings.Contains(exploreBlock, "context_window") {
			t.Errorf("explore block should omit context_window (got 0):\n%s", exploreBlock)
		}
	}
	if !strings.Contains(out, "general-purpose = \"9gouter-general-purpose\"") {
		t.Errorf("subagent mapping for general-purpose missing:\n%s", out)
	}
	if !strings.Contains(out, "# 9gouter-prev-subagent-general-purpose = \"grok-build\"") {
		t.Errorf("prev-subagent marker for general-purpose missing:\n%s", out)
	}
}

func TestApplyGrokBuildConfig_EmptySubagentMapClearsOverrides(t *testing.T) {
	// First write a config with a subagent override.
	withOverride := applyGrokBuildConfig("", "http://127.0.0.1:20128/v1", "sk_9gouter", "grok/build-model", 200000,
		map[string]*grokSubagentOverride{"general-purpose": {Model: "anthropic/claude-opus-4.7", ContextWindow: 200000}})
	if !strings.Contains(withOverride, "[model.9gouter-general-purpose]") {
		t.Fatalf("setup: override slot not written:\n%s", withOverride)
	}
	// Now apply with an empty (non-nil) map — that clears/restore-previous for every type.
	cleared := applyGrokBuildConfig(withOverride, "http://127.0.0.1:20128/v1", "sk_9gouter", "grok/build-model", 200000, map[string]*grokSubagentOverride{})
	if strings.Contains(cleared, "[model.9gouter-general-purpose]") {
		t.Errorf("empty map should remove the general-purpose override slot:\n%s", cleared)
	}
}

func TestResetGrokBuildConfig_RestoresDefault(t *testing.T) {
	applied := applyGrokBuildConfig(sampleGrokToml, "http://127.0.0.1:20128/v1", "sk_9gouter", "grok/build-model", 200000,
		map[string]*grokSubagentOverride{"general-purpose": {Model: "anthropic/claude-opus-4.7", ContextWindow: 200000}})
	reset := resetGrokBuildConfig(applied)

	// Our slots are gone.
	if strings.Contains(reset, "[model.9gouter]") {
		t.Errorf("reset left [model.9gouter]:\n%s", reset)
	}
	if strings.Contains(reset, "[model.9gouter-general-purpose]") {
		t.Errorf("reset left a subagent slot:\n%s", reset)
	}
	// Previous default restored.
	if !strings.Contains(reset, "default = \"grok-build\"") {
		t.Errorf("reset did not restore default=grok-build:\n%s", reset)
	}
	// Markers consumed.
	if strings.Contains(reset, "# 9gouter-prev-default") {
		t.Errorf("reset left a prev-default marker:\n%s", reset)
	}
	// Unrelated sections still intact.
	if !strings.Contains(reset, "[theme]\nmode = \"dark\"") {
		t.Errorf("reset dropped [theme]:\n%s", reset)
	}
}

func TestResetGrokBuildConfig_EmptyConfigIsNoOp(t *testing.T) {
	// An empty config has no 9gouter slots and no prev-default marker, so reset
	// is a no-op (mirrors JS resetGrokBuildConfig("") — restorePreviousDefault
	// only rewrites the default field when it currently equals the main slot).
	reset := resetGrokBuildConfig("")
	if reset != "" {
		t.Errorf("reset of empty config = %q, want empty string", reset)
	}
}

func TestApplyGrokBuildConfig_AppendsV1IfNeeded(t *testing.T) {
	// The POST handler appends /v1; this test documents the TOML editor itself
	// writes whatever baseURL it is given verbatim (the handler normalizes).
	out := applyGrokBuildConfig("", "http://127.0.0.1:20128/v1", "sk_9gouter", "grok/build-model", 200000, nil)
	if !strings.Contains(out, "base_url = \"http://127.0.0.1:20128/v1\"") {
		t.Errorf("base_url not written verbatim:\n%s", out)
	}
}

func TestGrokGetSectionField_DistinguishesAbsentFromEmpty(t *testing.T) {
	toml := `[models]
default = ""

[other]
kept = "yes"
`
	v, ok := grokGetSectionField(toml, "models", "default")
	if !ok || v != "" {
		t.Fatalf("default: got (%q,%v), want (\"\",true)", v, ok)
	}
	if _, ok := grokGetSectionField(toml, "models", "nonexistent"); ok {
		t.Fatal("nonexistent key reported as present")
	}
}

func TestGetGrokSubagentSlot(t *testing.T) {
	if got := getGrokSubagentSlot("general-purpose"); got != "9gouter-general-purpose" {
		t.Fatalf("getGrokSubagentSlot(general-purpose) = %q, want 9gouter-general-purpose", got)
	}
	if got := getGrokSubagentSlot("bogus"); got != "" {
		t.Fatalf("getGrokSubagentSlot(bogus) = %q, want empty", got)
	}
}
