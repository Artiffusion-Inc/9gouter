package thinking

import (
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/capabilities"
)

// TestEffortToThinkingLevel ports effortToThinkingLevel: the clamp mapping.
func TestEffortToThinkingLevel(t *testing.T) {
	cases := map[string]string{
		"max":     "high",
		"xhigh":   "high",
		"high":    "high",
		"medium":  "medium",
		"low":     "low",
		"minimal": "minimal",
		"none":    "minimal",
		"off":     "minimal",
		"":        "",
		"  Max  ": "high", // case-insensitive + trimmed
	}
	for in, want := range cases {
		if got := EffortToThinkingLevel(in); got != want {
			t.Errorf("EffortToThinkingLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestToGeminiThinkingLevel ports toGeminiThinkingLevel: auto→high, empty→high,
// then clamp.
func TestToGeminiThinkingLevel(t *testing.T) {
	cases := map[string]string{
		"auto":   "high",
		"":       "high",
		"max":    "high",
		"xhigh":  "high",
		"high":   "high",
		"medium": "medium",
		"none":   "minimal",
	}
	for in, want := range cases {
		if got := ToGeminiThinkingLevel(in); got != want {
			t.Errorf("ToGeminiThinkingLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestApplyGeminiLevelClampsMax ports the core c4f80d30 behavior: a body with
// reasoning_effort "max" routed to a gemini-level model gets a clamped
// thinkingLevel "high" + includeThoughts true + an output floor raise.
func TestApplyGeminiLevelClampsMax(t *testing.T) {
	body := map[string]any{
		"reasoning_effort": "max",
	}
	caps := capabilities.Capabilities{ThinkingFormat: capabilities.ThinkingGeminiLevel, MaxOutput: 200000}
	ApplyGeminiLevelThinking(body, "gemini-3-pro", caps)

	gc, ok := body["generationConfig"].(map[string]any)
	if !ok {
		t.Fatal("generationConfig not set")
	}
	tc, _ := gc["thinkingConfig"].(map[string]any)
	if tc["thinkingLevel"] != "high" {
		t.Errorf("thinkingLevel = %v, want high (max clamped)", tc["thinkingLevel"])
	}
	if tc["includeThoughts"] != true {
		t.Errorf("includeThoughts = %v, want true", tc["includeThoughts"])
	}
	if mt, _ := gc["maxOutputTokens"].(float64); mt != 65535 {
		t.Errorf("maxOutputTokens = %v, want 65535 (high floor)", mt)
	}
}

// TestApplyGeminiLevelNoneMapsMinimal ports the none branch: thinking disabled
// → minimal + includeThoughts false. Gemini 3 cannot fully disable thinking.
func TestApplyGeminiLevelNoneMapsMinimal(t *testing.T) {
	body := map[string]any{
		"reasoning_effort": "none",
	}
	caps := capabilities.Capabilities{ThinkingFormat: capabilities.ThinkingGeminiLevel, MaxOutput: 200000}
	ApplyGeminiLevelThinking(body, "gemini-3-pro", caps)
	gc := body["generationConfig"].(map[string]any)
	tc := gc["thinkingConfig"].(map[string]any)
	if tc["thinkingLevel"] != "minimal" {
		t.Errorf("thinkingLevel = %v, want minimal", tc["thinkingLevel"])
	}
	if tc["includeThoughts"] != false {
		t.Errorf("includeThoughts = %v, want false", tc["includeThoughts"])
	}
	if mt, _ := gc["maxOutputTokens"].(float64); mt != 4096 {
		t.Errorf("maxOutputTokens = %v, want 4096 (minimal floor)", mt)
	}
}

// TestApplyGeminiLevelAutoMapsHigh ports auto→high.
func TestApplyGeminiLevelAutoMapsHigh(t *testing.T) {
	body := map[string]any{"reasoning_effort": "auto"}
	caps := capabilities.Capabilities{ThinkingFormat: capabilities.ThinkingGeminiLevel}
	ApplyGeminiLevelThinking(body, "gemini-3-pro", caps)
	tc := body["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if tc["thinkingLevel"] != "high" {
		t.Errorf("auto → %v, want high", tc["thinkingLevel"])
	}
}

// TestApplyGeminiLevelOutputFloorCappedByMaxOutput verifies the floor is capped
// by the model's advertised maxOutput when it is lower than the floor.
func TestApplyGeminiLevelOutputFloorCappedByMaxOutput(t *testing.T) {
	body := map[string]any{"reasoning_effort": "high"}
	// high floor = 65535, but model maxOutput = 10000 → capped to 10000.
	caps := capabilities.Capabilities{ThinkingFormat: capabilities.ThinkingGeminiLevel, MaxOutput: 10000}
	ApplyGeminiLevelThinking(body, "gemini-3-pro", caps)
	if mt, _ := body["generationConfig"].(map[string]any)["maxOutputTokens"].(float64); mt != 10000 {
		t.Errorf("maxOutputTokens = %v, want 10000 (floor capped by maxOutput)", mt)
	}
}

// TestApplyGeminiLevelDoesNotLowerExistingOutput verifies the floor only raises,
// never lowers, an existing maxOutputTokens.
func TestApplyGeminiLevelDoesNotLowerExistingOutput(t *testing.T) {
	body := map[string]any{
		"reasoning_effort": "minimal", // floor 4096
		"generationConfig": map[string]any{"maxOutputTokens": float64(50000)},
	}
	caps := capabilities.Capabilities{ThinkingFormat: capabilities.ThinkingGeminiLevel, MaxOutput: 200000}
	ApplyGeminiLevelThinking(body, "gemini-3-pro", caps)
	if mt, _ := body["generationConfig"].(map[string]any)["maxOutputTokens"].(float64); mt != 50000 {
		t.Errorf("maxOutputTokens lowered to %v, want 50000 (floor never lowers)", mt)
	}
}

// TestApplyGeminiLevelEnvelope targets the request.generationConfig envelope
// used by gemini-cli/antigravity, not the top-level generationConfig.
func TestApplyGeminiLevelEnvelope(t *testing.T) {
	body := map[string]any{
		"reasoning_effort": "high",
		"request":          map[string]any{},
	}
	caps := capabilities.Capabilities{ThinkingFormat: capabilities.ThinkingGeminiLevel, MaxOutput: 200000}
	ApplyGeminiLevelThinking(body, "gemini-3-pro", caps)
	req := body["request"].(map[string]any)
	gc := req["generationConfig"].(map[string]any)
	tc := gc["thinkingConfig"].(map[string]any)
	if tc["thinkingLevel"] != "high" {
		t.Errorf("envelope thinkingLevel = %v, want high", tc["thinkingLevel"])
	}
	// Top-level generationConfig should NOT be created when the envelope exists.
	if _, has := body["generationConfig"]; has {
		t.Error("top-level generationConfig should not be created with envelope present")
	}
}

// TestApplyGeminiLevelNoOpWithoutReasoningEffort verifies the spot-fix does not
// invent a thinkingConfig when the body carries no reasoning_effort (a
// passthrough Gemini body that already set its own thinkingConfig is left alone).
func TestApplyGeminiLevelNoOpWithoutReasoningEffort(t *testing.T) {
	body := map[string]any{}
	caps := capabilities.Capabilities{ThinkingFormat: capabilities.ThinkingGeminiLevel}
	ApplyGeminiLevelThinking(body, "gemini-3-pro", caps)
	if _, has := body["generationConfig"]; has {
		t.Error("no-op body should not get generationConfig")
	}
}

// TestGetGeminiGenerationConfigAllocates verifies both envelope and top-level
// paths allocate the nested map.
func TestGetGeminiGenerationConfigAllocates(t *testing.T) {
	// Top-level.
	b1 := map[string]any{}
	gc1 := getGeminiGenerationConfig(b1)
	gc1["thinkingConfig"] = map[string]any{"thinkingLevel": "high"}
	if b1["generationConfig"].(map[string]any)["thinkingConfig"] == nil {
		t.Error("top-level generationConfig not allocated")
	}

	// Envelope.
	b2 := map[string]any{"request": map[string]any{}}
	gc2 := getGeminiGenerationConfig(b2)
	gc2["thinkingConfig"] = map[string]any{"thinkingLevel": "low"}
	if b2["request"].(map[string]any)["generationConfig"] == nil {
		t.Error("envelope generationConfig not allocated")
	}
}

// TestGeminiLevelFloor covers the floor lookup + high fallback.
func TestGeminiLevelFloor(t *testing.T) {
	cases := map[string]int{
		"minimal": 4096,
		"low":     8192,
		"medium":  16384,
		"high":    65535,
		"bogus":   65535, // fallback
		"":        65535,
	}
	for level, want := range cases {
		if got := GeminiLevelFloor(level); got != want {
			t.Errorf("GeminiLevelFloor(%q) = %d, want %d", level, got, want)
		}
	}
}
