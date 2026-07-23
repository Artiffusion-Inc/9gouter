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

// TestStripThinkingSuffix ports stripThinkingSuffix (b10b8070): the UI appends
// a "(level)" suffix to copied model names; the server strips it from upstream
// body.model so providers do not reject the request for an unknown model id.
func TestStripThinkingSuffix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"claude-opus-4-8(high)", "claude-opus-4-8"},
		{"gpt-5(xhigh)", "gpt-5"},
		{"gemini-3-pro(none)", "gemini-3-pro"},
		{"model(8192)", "model"},                            // numeric budget suffix is also stripped
		{"model(auto)", "model"},                            // auto suffix stripped
		{"claude-opus-4-8", "claude-opus-4-8"},              // no suffix → unchanged
		{"plain", "plain"},                                  // no parens → unchanged
		{"", ""},                                            // empty → unchanged
		{"a/b/c(high)", "a/b/c"},                            // slashes preserved, suffix stripped
		{"weird (not) suffix (high)", "weird (not) suffix"}, // only trailing parens stripped
		{"trailing-space(high)   ", "trailing-space"},       // trailing whitespace trimmed
		{"no-close(gpt", "no-close(gpt"},                    // no closing paren → unchanged
	}
	for _, c := range cases {
		if got := StripThinkingSuffix(c.in); got != c.want {
			t.Errorf("StripThinkingSuffix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestEffortToBudget ports effortToBudget (thinking.js): level → token budget
// via LEVEL_TO_BUDGET; empty/unrecognized → not-ok.
func TestEffortToBudget(t *testing.T) {
	cases := []struct {
		in     string
		budget int
		ok     bool
	}{
		{"none", 0, true},
		{"minimal", 512, true},
		{"low", 1024, true},
		{"medium", 8192, true},
		{"high", 24576, true},
		{"xhigh", 32768, true},
		{"max", 128000, true},
		{"  High  ", 24576, true}, // trimmed + lowercased
		{"", 0, false},
		{"auto", 0, false}, // not in LEVEL_TO_BUDGET → dynamic path
		{"bogus", 0, false},
	}
	for _, c := range cases {
		b, ok := EffortToBudget(c.in)
		if ok != c.ok || (ok && b != c.budget) {
			t.Errorf("EffortToBudget(%q) = (%d, %v), want (%d, %v)", c.in, b, ok, c.budget, c.ok)
		}
	}
}

// TestGeminiBudgetFloor ports geminiBudgetOutputFloor (7610f28f): the
// budget-derived maxOutputTokens floor for gemini-budget models.
func TestGeminiBudgetFloor(t *testing.T) {
	cases := map[int]int{
		-1:     32768, // auto / no budget
		0:      8192,  // 0 ≤ 1024
		1024:   8192,  // boundary low end
		1025:   16384, // > 1024, ≤ 8192
		8192:   16384, // boundary
		8193:   32768, // > 8192, ≤ 24576
		24576:  32768, // boundary
		24577:  65535, // > 24576
		128000: 65535,
	}
	for budget, want := range cases {
		if got := GeminiBudgetFloor(budget); got != want {
			t.Errorf("GeminiBudgetFloor(%d) = %d, want %d", budget, got, want)
		}
	}
}

// TestApplyGeminiBudgetThinkingHigh ports the gemini-budget happy path
// (7610f28f): reasoning_effort high → budget 24576, clamped by the model's
// ThinkingRange (max 24576), thinkingConfig.thinkingBudget=24576,
// includeThoughts=true, and maxOutputTokens raised to GeminiBudgetFloor(24576)
// = 32768 (capped by caps.MaxOutput 65536, so 32768 wins).
func TestApplyGeminiBudgetThinkingHigh(t *testing.T) {
	body := map[string]any{"reasoning_effort": "high"}
	caps := capabilities.Capabilities{
		ThinkingFormat: capabilities.ThinkingGeminiBudget,
		ThinkingRange:  &capabilities.ThinkingRange{Min: 0, Max: 24576},
		MaxOutput:      65536,
	}
	ApplyGeminiBudgetThinking(body, "gemini-2.5-pro", caps)
	tc := body["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if tc["thinkingBudget"] != int(24576) {
		t.Errorf("thinkingBudget = %v, want 24576 (high, clamped by range max)", tc["thinkingBudget"])
	}
	if tc["includeThoughts"] != true {
		t.Errorf("includeThoughts = %v, want true", tc["includeThoughts"])
	}
	if mt, _ := body["generationConfig"].(map[string]any)["maxOutputTokens"].(float64); mt != 32768 {
		t.Errorf("maxOutputTokens = %v, want 32768 (GeminiBudgetFloor(24576))", mt)
	}
}

// TestApplyGeminiBudgetThinkingMaxClampedByRange verifies the budget is clamped
// down by the model's ThinkingRange max, then the floor follows the clamped
// budget.
func TestApplyGeminiBudgetThinkingMaxClampedByRange(t *testing.T) {
	body := map[string]any{"reasoning_effort": "max"} // budget 128000
	caps := capabilities.Capabilities{
		ThinkingFormat: capabilities.ThinkingGeminiBudget,
		ThinkingRange:  &capabilities.ThinkingRange{Min: 0, Max: 24576},
		MaxOutput:      65536,
	}
	ApplyGeminiBudgetThinking(body, "gemini-2.5-pro", caps)
	tc := body["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if tc["thinkingBudget"] != int(24576) {
		t.Errorf("thinkingBudget = %v, want 24576 (max clamped by range max 24576)", tc["thinkingBudget"])
	}
	if mt, _ := body["generationConfig"].(map[string]any)["maxOutputTokens"].(float64); mt != 32768 {
		t.Errorf("maxOutputTokens = %v, want 32768 (floor of clamped budget)", mt)
	}
}

// TestApplyGeminiBudgetThinkingAutoDynamic verifies auto (no resolvable budget)
// → thinkingBudget=-1 (dynamic) with the 32768 floor.
func TestApplyGeminiBudgetThinkingAutoDynamic(t *testing.T) {
	body := map[string]any{"reasoning_effort": "auto"}
	caps := capabilities.Capabilities{ThinkingFormat: capabilities.ThinkingGeminiBudget, MaxOutput: 65536}
	ApplyGeminiBudgetThinking(body, "gemini-2.5-pro", caps)
	tc := body["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if tc["thinkingBudget"] != int(-1) {
		t.Errorf("thinkingBudget = %v, want -1 (dynamic)", tc["thinkingBudget"])
	}
	if tc["includeThoughts"] != true {
		t.Errorf("includeThoughts = %v, want true", tc["includeThoughts"])
	}
	if mt, _ := body["generationConfig"].(map[string]any)["maxOutputTokens"].(float64); mt != 32768 {
		t.Errorf("maxOutputTokens = %v, want 32768 (GeminiBudgetFloor(-1))", mt)
	}
}

// TestApplyGeminiBudgetThinkingNoneCanDisable verifies reasoning_effort none on
// a model that can disable thinking → thinkingBudget:0, includeThoughts:false.
func TestApplyGeminiBudgetThinkingNoneCanDisable(t *testing.T) {
	body := map[string]any{"reasoning_effort": "none"}
	caps := capabilities.Capabilities{ThinkingFormat: capabilities.ThinkingGeminiBudget, ThinkingCanDisable: true, MaxOutput: 65536}
	ApplyGeminiBudgetThinking(body, "gemini-2.5-pro", caps)
	tc := body["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if tc["thinkingBudget"] != int(0) {
		t.Errorf("thinkingBudget = %v, want 0 (disabled)", tc["thinkingBudget"])
	}
	if tc["includeThoughts"] != false {
		t.Errorf("includeThoughts = %v, want false (disabled)", tc["includeThoughts"])
	}
}

// TestApplyGeminiBudgetThinkingFloorCappedByMaxOutput verifies the floor is
// capped by the model's advertised maxOutput when it is lower than the floor.
func TestApplyGeminiBudgetThinkingFloorCappedByMaxOutput(t *testing.T) {
	body := map[string]any{"reasoning_effort": "high"} // budget 24576 → floor 32768
	caps := capabilities.Capabilities{
		ThinkingFormat: capabilities.ThinkingGeminiBudget,
		ThinkingRange:  &capabilities.ThinkingRange{Min: 0, Max: 24576},
		MaxOutput:      10000, // below floor 32768 → capped
	}
	ApplyGeminiBudgetThinking(body, "gemini-2.5-pro", caps)
	if mt, _ := body["generationConfig"].(map[string]any)["maxOutputTokens"].(float64); mt != 10000 {
		t.Errorf("maxOutputTokens = %v, want 10000 (floor capped by maxOutput)", mt)
	}
}

// TestApplyGeminiBudgetThinkingNoOpWithoutReasoningEffort verifies the spot-fix
// does not invent a thinkingConfig when the body carries no reasoning_effort.
func TestApplyGeminiBudgetThinkingNoOpWithoutReasoningEffort(t *testing.T) {
	body := map[string]any{}
	caps := capabilities.Capabilities{ThinkingFormat: capabilities.ThinkingGeminiBudget}
	ApplyGeminiBudgetThinking(body, "gemini-2.5-pro", caps)
	if _, has := body["generationConfig"]; has {
		t.Error("no-op body should not get generationConfig")
	}
}

// TestApplyGeminiBudgetThinkingEnvelope verifies the gemini-cli/antigravity
// request.generationConfig envelope is targeted when present.
func TestApplyGeminiBudgetThinkingEnvelope(t *testing.T) {
	body := map[string]any{
		"reasoning_effort": "medium",
		"request":          map[string]any{},
	}
	caps := capabilities.Capabilities{ThinkingFormat: capabilities.ThinkingGeminiBudget, MaxOutput: 65536}
	ApplyGeminiBudgetThinking(body, "gemini-2.5-pro", caps)
	req := body["request"].(map[string]any)
	tc := req["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if tc["thinkingBudget"] != int(8192) {
		t.Errorf("envelope thinkingBudget = %v, want 8192 (medium)", tc["thinkingBudget"])
	}
	if _, has := body["generationConfig"]; has {
		t.Error("top-level generationConfig should not be created with envelope present")
	}
}
