package capabilities

import (
	"testing"
)

// TestGetCapabilitiesDefaultFloor verifies the safe floor is returned for an
// unknown model and merged (Tools:true, ThinkingCanDisable:true, limits set).
func TestGetCapabilitiesDefaultFloor(t *testing.T) {
	c := GetCapabilitiesForModel("", "some-unknown-model-xyz")
	if !c.Tools {
		t.Error("default Tools should be true")
	}
	if !c.ThinkingCanDisable {
		t.Error("default ThinkingCanDisable should be true")
	}
	if c.ContextWindow != 200000 {
		t.Errorf("default ContextWindow = %d, want 200000", c.ContextWindow)
	}
	if c.MaxOutput != 64000 {
		t.Errorf("default MaxOutput = %d, want 64000", c.MaxOutput)
	}
	if c.Reasoning {
		t.Error("unknown model should not be reasoning")
	}
}

// TestGetCapabilitiesEmptyModel returns Default for empty model.
func TestGetCapabilitiesEmptyModel(t *testing.T) {
	c := GetCapabilitiesForModel("openai", "")
	if c.ContextWindow != 200000 {
		t.Errorf("empty model ContextWindow = %d, want 200000", c.ContextWindow)
	}
}

// TestQwenPatterns ports upstream 7fa2e7f0: qwen3.5/3.6/3.7 = native
// vision+video; qwen*omni = audio+video; qwen*coder/max = text-only reasoning.
func TestQwenPatterns(t *testing.T) {
	cases := []struct {
		model                   string
		wantVision, wantVideo   bool
		wantAudio               bool
		wantReasoning           bool
		wantThinking            ThinkingFormat
		wantContext, wantOutput int
	}{
		{"qwen3.5-instruct", true, true, false, true, ThinkingQwen, 1000000, 65536},
		{"qwen3.6-chat", true, true, false, true, ThinkingQwen, 1000000, 65536},
		{"qwen3.7-chat", true, true, false, true, ThinkingQwen, 1000000, 65536},
		{"qwen-omni-turbo", true, true, true, true, ThinkingQwen, 262144, 65536},
		{"qwen-coder-plus", false, false, false, true, ThinkingQwen, 1000000, 0},
		{"qwen-max-latest", false, false, false, true, ThinkingQwen, 1000000, 65536},
		{"qwen-vl-plus", true, false, false, true, ThinkingQwen, 262144, 0},
	}
	for _, c := range cases {
		got := GetCapabilitiesForModel("", c.model)
		if got.Vision != c.wantVision {
			t.Errorf("%s Vision = %v, want %v", c.model, got.Vision, c.wantVision)
		}
		if got.VideoInput != c.wantVideo {
			t.Errorf("%s VideoInput = %v, want %v", c.model, got.VideoInput, c.wantVideo)
		}
		if got.AudioInput != c.wantAudio {
			t.Errorf("%s AudioInput = %v, want %v", c.model, got.AudioInput, c.wantAudio)
		}
		if got.Reasoning != c.wantReasoning {
			t.Errorf("%s Reasoning = %v, want %v", c.model, got.Reasoning, c.wantReasoning)
		}
		if got.ThinkingFormat != c.wantThinking {
			t.Errorf("%s ThinkingFormat = %q, want %q", c.model, got.ThinkingFormat, c.wantThinking)
		}
		if c.wantContext != 0 && got.ContextWindow != c.wantContext {
			t.Errorf("%s ContextWindow = %d, want %d", c.model, got.ContextWindow, c.wantContext)
		}
		if c.wantOutput != 0 && got.MaxOutput != c.wantOutput {
			t.Errorf("%s MaxOutput = %d, want %d", c.model, got.MaxOutput, c.wantOutput)
		}
	}
}

// TestClaudeOpusDashedId ports upstream 49a3ec7a: the dashed id
// claude-opus-4-7 must resolve to 1M context + adaptive thinking, not fall
// through to the generic budget pattern (MatchPattern treats "." as literal).
func TestClaudeOpusDashedId(t *testing.T) {
	for _, id := range []string{"claude-opus-4-7", "claude-opus-4.7", "claude-opus-4-8", "claude-opus-4.8"} {
		c := GetCapabilitiesForModel("", id)
		if c.ContextWindow != 1000000 {
			t.Errorf("%s ContextWindow = %d, want 1000000", id, c.ContextWindow)
		}
		if c.MaxOutput != 128000 {
			t.Errorf("%s MaxOutput = %d, want 128000", id, c.MaxOutput)
		}
		if c.ThinkingFormat != ThinkingClaudeAdaptive {
			t.Errorf("%s ThinkingFormat = %q, want claude-adaptive", id, c.ThinkingFormat)
		}
		if !c.Vision || !c.Reasoning || !c.Search {
			t.Errorf("%s should have vision+reasoning+search", id)
		}
	}
}

// TestClaudeSonnet5 ports upstream a5363b83: claude-sonnet-5 and its
// -thinking/-agentic/-thinking-agentic variants resolve to 1M / 128k adaptive.
func TestClaudeSonnet5(t *testing.T) {
	for _, id := range []string{
		"claude-sonnet-5", "claude-sonnet-5-thinking",
		"claude-sonnet-5-agentic", "claude-sonnet-5-thinking-agentic",
	} {
		c := GetCapabilitiesForModel("", id)
		if c.ContextWindow != 1000000 {
			t.Errorf("%s ContextWindow = %d, want 1000000", id, c.ContextWindow)
		}
		if c.MaxOutput != 128000 {
			t.Errorf("%s MaxOutput = %d, want 128000", id, c.MaxOutput)
		}
		if c.ThinkingFormat != ThinkingClaudeAdaptive {
			t.Errorf("%s ThinkingFormat = %q, want claude-adaptive", id, c.ThinkingFormat)
		}
	}
}

// TestGPTImage1ToolsFalse verifies the rare Tools:false override for an
// image-only model (gpt-image-1) survives the merge over Default (Tools:true).
func TestGPTImage1ToolsFalse(t *testing.T) {
	c := GetCapabilitiesForModel("", "gpt-image-1")
	if c.Tools {
		t.Error("gpt-image-1 Tools should be false (image-only)")
	}
	if !c.ImageOutput {
		t.Error("gpt-image-1 should have ImageOutput")
	}
}

// TestThinkingCanDisableFalse verifies models that cannot turn thinking off
// resolve ThinkingCanDisable:false, while a generic reasoning model keeps
// the default true.
func TestThinkingCanDisableFalse(t *testing.T) {
	cannotDisable := []string{"qwq-32b", "kimi-k3", "gemini-3-pro", "minimax-m2.7"}
	for _, m := range cannotDisable {
		c := GetCapabilitiesForModel("", m)
		if c.ThinkingCanDisable {
			t.Errorf("%s ThinkingCanDisable should be false", m)
		}
	}
	// Generic reasoning model keeps the default true.
	c := GetCapabilitiesForModel("", "deepseek-v4-pro")
	if !c.ThinkingCanDisable {
		t.Error("deepseek-v4-pro ThinkingCanDisable should stay true (default)")
	}
}

// TestProviderOverride verifies provider-specific caps win over the pattern
// chain (nvidia forces openai thinkingFormat for GLM/MiniMax).
func TestProviderOverride(t *testing.T) {
	c := GetCapabilitiesForModel("nvidia", "z-ai/glm-5.2")
	if c.ThinkingFormat != ThinkingOpenai {
		t.Errorf("nvidia glm-5.2 ThinkingFormat = %q, want openai", c.ThinkingFormat)
	}
	if c.ContextWindow != 200000 {
		t.Errorf("nvidia glm-5.2 ContextWindow = %d, want 200000", c.ContextWindow)
	}
	// Same model without the nvidia provider resolves via the generic glm-5
	// pattern (zai thinkingFormat).
	c2 := GetCapabilitiesForModel("", "glm-5.2")
	if c2.ThinkingFormat != ThinkingZai {
		t.Errorf("glm-5.2 (no provider) ThinkingFormat = %q, want zai", c2.ThinkingFormat)
	}
}

// TestKiroGPT56 verifies the kiro provider override for the gpt-5.6 family.
func TestKiroGPT56(t *testing.T) {
	for _, id := range []string{"gpt-5.6-sol", "gpt-5.6-terra-thinking", "gpt-5.6-luna-agentic"} {
		c := GetCapabilitiesForModel("kiro", id)
		if c.ContextWindow != 272000 {
			t.Errorf("kiro %s ContextWindow = %d, want 272000", id, c.ContextWindow)
		}
		if c.MaxOutput != 128000 {
			t.Errorf("kiro %s MaxOutput = %d, want 128000", id, c.MaxOutput)
		}
		if c.ThinkingFormat != ThinkingOpenai {
			t.Errorf("kiro %s ThinkingFormat = %q, want openai", id, c.ThinkingFormat)
		}
	}
}

// TestCodexGPT56SolContextWindow verifies the codex provider override: sol
// variants report 372000, terra/luna report 272000 (#2720).
func TestCodexGPT56SolContextWindow(t *testing.T) {
	sol := GetCapabilitiesForModel("codex", "gpt-5.6-sol")
	if sol.ContextWindow != 372000 {
		t.Errorf("codex gpt-5.6-sol ContextWindow = %d, want 372000", sol.ContextWindow)
	}
	terra := GetCapabilitiesForModel("codex", "gpt-5.6-terra")
	if terra.ContextWindow != 272000 {
		t.Errorf("codex gpt-5.6-terra ContextWindow = %d, want 272000", terra.ContextWindow)
	}
}

// TestVendorPrefixStripped verifies "anthropic/claude-opus-4.7" resolves like
// the bare id (the / prefix is stripped before exact lookup).
func TestVendorPrefixStripped(t *testing.T) {
	c := GetCapabilitiesForModel("", "anthropic/claude-opus-4.7")
	if c.ContextWindow != 1000000 {
		t.Errorf("anthropic/claude-opus-4.7 ContextWindow = %d, want 1000000", c.ContextWindow)
	}
}

// TestGemini25ThinkingRange verifies the gemini-2.5 budget range clamp.
func TestGemini25ThinkingRange(t *testing.T) {
	c := GetCapabilitiesForModel("", "gemini-2.5-pro")
	if c.ThinkingRange == nil {
		t.Fatal("gemini-2.5 ThinkingRange should be set")
	}
	if c.ThinkingRange.Min != 0 || c.ThinkingRange.Max != 24576 {
		t.Errorf("gemini-2.5 ThinkingRange = %+v, want {0 24576}", c.ThinkingRange)
	}
	if c.ThinkingFormat != ThinkingGeminiBudget {
		t.Errorf("gemini-2.5 ThinkingFormat = %q, want gemini-budget", c.ThinkingFormat)
	}
}

// TestFromServiceKind ports capabilitiesFromServiceKind.
func TestFromServiceKind(t *testing.T) {
	if c := FromServiceKind("imageToText"); c == nil || !c.Vision {
		t.Error("imageToText should set Vision")
	}
	if c := FromServiceKind("embedding"); c == nil || c.Tools {
		t.Error("embedding should set Tools:false")
	}
	if c := FromServiceKind("stt"); c == nil || !c.AudioInput {
		t.Error("stt should set AudioInput")
	}
	if c := FromServiceKind("tts"); c == nil || !c.AudioOutput {
		t.Error("tts should set AudioOutput")
	}
	if FromServiceKind("unknown") != nil {
		t.Error("unknown kind should return nil")
	}
}
