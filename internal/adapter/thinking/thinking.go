// Package thinking ports open-sse/translator/concerns/thinking.js (maps) and
// the gemini-level branch of thinkingUnified.js's applyFormat (c4f80d30).
//
// Go has no central applyThinking — passthrough bodies reach the upstream
// byte-for-byte — so the Gemini 3 thinking-level clamp (max/xhigh → high,
// auto → high) is applied as a spot-fix next to the OpenAI reasoning_effort
// clamp. Gemini 3 cannot fully disable thinking (none/off → minimal) and its
// thinkingLevel enum is minimal|low|medium|high, so OpenAI's max/xhigh levels
// must be clamped or the request is rejected.
package thinking

import (
	"regexp"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/capabilities"
)

// thinkingSuffixRe mirrors stripThinkingSuffix in thinkingUnified.js (b10b8070):
// a trailing "(value)" suffix the UI appends to a copied model name to force a
// thinking level across all formats — e.g. "claude-opus-4-8(high)". The captured
// value is ignored here; the strip keeps the upstream from rejecting the request
// for an unknown model id. Greedy group, anchored at end; the inner [^()]+ keeps
// it from matching across nested parens.
var thinkingSuffixRe = regexp.MustCompile(`^(.*)\([^()]+\)\s*$`)

// StripThinkingSuffix removes a trailing "(value)" thinking-level suffix from a
// model name, returning the clean upstream model id. A no-op when the suffix is
// absent. Non-string input (already-typed callers pass strings) is returned
// unchanged. Ports stripThinkingSuffix from thinkingUnified.js (b10b8070): the
// chat path forces body.model = StripThinkingSuffix(upstreamModel) so providers
// no longer reject requests carrying the UI's forced-level suffix.
func StripThinkingSuffix(model string) string {
	if model == "" {
		return model
	}
	m := thinkingSuffixRe.FindStringSubmatch(model)
	if m == nil {
		return model
	}
	return strings.TrimSpace(m[1])
}

// GeminiLevelOutputFloor mirrors GEMINI_LEVEL_OUTPUT_FLOOR in
// thinkingUnified.js: the minimum maxOutputTokens Gemini 3 needs for a given
// thinkingLevel, before being capped by the model's advertised maxOutput.
var geminiLevelOutputFloor = map[string]int{
	"minimal": 4096,
	"low":     8192,
	"medium":  16384,
	"high":    65535,
}

// EffortToThinkingLevel mirrors effortToThinkingLevel in thinking.js: maps an
// OpenAI reasoning_effort string to the Gemini 3 thinkingLevel enum
// (minimal|low|medium|high). Gemini 3 cannot fully disable thinking, so
// none/off → minimal; xhigh/max → high (the enum has no xhigh/max).
func EffortToThinkingLevel(effort string) string {
	e := strings.ToLower(strings.TrimSpace(effort))
	switch e {
	case "none", "off":
		return "minimal"
	case "xhigh", "max":
		return "high"
	default:
		return e
	}
}

// toLevel mirrors toLevel in thinkingUnified.js: resolves a thinking config
// (mode+level or mode+budget) to a discrete level. The Go spot-fix only needs
// the "auto" → "auto" and bare-level paths (reasoning_effort is the input), so
// budget→level is handled via budgetToLevel. Returns "" when unrecognized.
func toLevel(level string) string {
	if level == "" {
		return ""
	}
	if level == "auto" {
		return "auto"
	}
	return strings.ToLower(strings.TrimSpace(level))
}

// ToGeminiThinkingLevel mirrors toGeminiThinkingLevel in thinkingUnified.js
// (c4f80d30): auto → "high"; an unrecognized/empty level falls back to "high";
// then EffortToThinkingLevel clamps none/off→minimal and xhigh/max→high.
func ToGeminiThinkingLevel(level string) string {
	raw := toLevel(level)
	if raw == "" || raw == "auto" {
		raw = "high"
	}
	return EffortToThinkingLevel(raw)
}

// getGeminiGenerationConfig mirrors getGeminiGenerationConfig in
// thinkingUnified.js: Gemini nests thinkingConfig under generationConfig, and
// gemini-cli/antigravity wrap the whole request in a { request: { generationConfig } }
// envelope — target the envelope's generationConfig when present, else the
// top-level one. It allocates the generationConfig map if missing.
func getGeminiGenerationConfig(body map[string]any) map[string]any {
	if req, ok := body["request"].(map[string]any); ok {
		gc, ok := req["generationConfig"].(map[string]any)
		if !ok {
			gc = map[string]any{}
			req["generationConfig"] = gc
		}
		return gc
	}
	gc, ok := body["generationConfig"].(map[string]any)
	if !ok {
		gc = map[string]any{}
		body["generationConfig"] = gc
	}
	return gc
}

// SetGeminiThinking mirrors setGeminiThinking: writes the thinkingConfig
// object into the (envelope-aware) generationConfig.
func SetGeminiThinking(body map[string]any, tc map[string]any) {
	getGeminiGenerationConfig(body)["thinkingConfig"] = tc
}

// EnsureGeminiOutputFloor mirrors ensureGeminiOutputFloor: raises
// generationConfig.maxOutputTokens to min(floor, caps.maxOutput) when it is
// missing or below the target. capsMaxOutput <= 0 means no cap (use floor).
func EnsureGeminiOutputFloor(body map[string]any, floor int, capsMaxOutput int) {
	target := floor
	if capsMaxOutput > 0 && capsMaxOutput < target {
		target = capsMaxOutput
	}
	gc := getGeminiGenerationConfig(body)
	current := numberOrZero(gc["maxOutputTokens"])
	if current < float64(target) {
		gc["maxOutputTokens"] = float64(target)
	}
}

// GeminiLevelFloor returns GEMINI_LEVEL_OUTPUT_FLOOR[level] with a high fallback.
func GeminiLevelFloor(level string) int {
	if f, ok := geminiLevelOutputFloor[level]; ok {
		return f
	}
	return geminiLevelOutputFloor["high"]
}

// levelToBudget mirrors LEVEL_TO_BUDGET in thinking.js: maps a discrete thinking
// level to a token budget for budget-style formats. Zero/missing → 0 (none).
var levelToBudget = map[string]int{
	"none":    0,
	"minimal": 512,
	"low":     1024,
	"medium":  8192,
	"high":    24576,
	"xhigh":   32768,
	"max":     128000,
}

// EffortToBudget mirrors effortToBudget in thinking.js: maps a reasoning_effort
// string to a token budget via LEVEL_TO_BUDGET. Returns ok=false for an empty or
// unrecognized effort (the JS function returns undefined in that case).
func EffortToBudget(effort string) (int, bool) {
	if effort == "" {
		return 0, false
	}
	b, ok := levelToBudget[strings.ToLower(strings.TrimSpace(effort))]
	if !ok {
		return 0, false
	}
	return b, true
}

// clampBudget mirrors the range clamp in toBudget (thinkingUnified.js): when a
// ThinkingRange is present, the budget is clamped to [min,max]. A nil range or
// zero field means no bound on that side.
func clampBudget(budget int, r *capabilities.ThinkingRange) int {
	if r == nil {
		return budget
	}
	if r.Min != 0 && budget < r.Min {
		budget = r.Min
	}
	if r.Max != 0 && budget > r.Max {
		budget = r.Max
	}
	return budget
}

// GeminiBudgetFloor mirrors geminiBudgetOutputFloor in thinkingUnified.js
// (7610f28f): the minimum maxOutputTokens a gemini-budget model needs for a
// given thinking budget, before being capped by the model's advertised
// maxOutput. -1 (auto / no budget) and non-finite budgets → 32768.
func GeminiBudgetFloor(budget int) int {
	if budget == -1 {
		return 32768
	}
	if budget <= 1024 {
		return 8192
	}
	if budget <= 8192 {
		return 16384
	}
	if budget <= 24576 {
		return 32768
	}
	return 65535
}

// ApplyGeminiLevelThinking ports the case "gemini-level" branch of
// thinkingUnified.applyFormat (c4f80d30). It resolves the thinking level from
// the OpenAI reasoning_effort carried on the body, clamps it to the Gemini 3
// enum, writes generationConfig.thinkingConfig = { thinkingLevel, includeThoughts },
// and raises maxOutputTokens to the level floor (capped by the model's
// advertised maxOutput).
//
// none=true (thinking disabled) maps to "minimal" and includeThoughts=false,
// mirroring the `none ? "minimal" : toGeminiThinkingLevel(eff)` branch.
//
// This is a no-op when reasoning_effort is absent AND thinking is not disabled —
// the body already carries whatever the client sent and we do not invent a
// thinkingConfig. (The JS pipeline runs this for every gemini-level model; the
// Go spot-fix only fires when there is a reasoning_effort to translate, to
// avoid mutating passthrough Gemini bodies that already set thinkingConfig.)
func ApplyGeminiLevelThinking(body map[string]any, model string, caps capabilities.Capabilities) {
	reasoningEffort, _ := body["reasoning_effort"].(string)
	// Detect "thinking disabled" via reasoning_effort none/off or an explicit
	// thinkingConfig.includeThoughts=false passthrough; Gemini 3 still maps to
	// minimal rather than disabling.
	none := strings.EqualFold(reasoningEffort, "none") || strings.EqualFold(reasoningEffort, "off")

	if reasoningEffort == "" && !none {
		return
	}

	level := "minimal"
	if !none {
		level = ToGeminiThinkingLevel(reasoningEffort)
	}
	SetGeminiThinking(body, map[string]any{
		"thinkingLevel":   level,
		"includeThoughts": level != "minimal",
	})
	EnsureGeminiOutputFloor(body, GeminiLevelFloor(level), caps.MaxOutput)
}

// ApplyGeminiBudgetThinking ports the case "gemini-budget" branch of
// thinkingUnified.applyFormat (7610f28f). Gemini-budget models (gemini-2.5)
// take a numeric thinkingBudget instead of the discrete thinkingLevel enum.
// It resolves the budget from the OpenAI reasoning_effort on the body (via
// EffortToBudget, clamped to the model's ThinkingRange), writes
// generationConfig.thinkingConfig = { thinkingBudget, includeThoughts }, and
// raises maxOutputTokens to GeminiBudgetFloor (capped by the model's
// advertised maxOutput).
//
// reasoning_effort none/off (thinking disabled) maps to thinkingBudget:0 and
// includeThoughts:false when the model can disable thinking; otherwise a
// disabled-but-cannot-disable request still gets a minimal budget so Gemini
// does not reject it. Auto / no budget → thinkingBudget:-1 (dynamic) with the
// 32768 floor.
//
// Like ApplyGeminiLevelThinking this is a no-op when reasoning_effort is absent
// AND thinking is not disabled, to avoid mutating passthrough Gemini bodies
// that already set thinkingConfig.
func ApplyGeminiBudgetThinking(body map[string]any, model string, caps capabilities.Capabilities) {
	reasoningEffort, _ := body["reasoning_effort"].(string)
	none := strings.EqualFold(reasoningEffort, "none") || strings.EqualFold(reasoningEffort, "off")

	if reasoningEffort == "" && !none {
		return
	}

	if none && caps.ThinkingCanDisable {
		SetGeminiThinking(body, map[string]any{
			"thinkingBudget":  0,
			"includeThoughts": false,
		})
		EnsureGeminiOutputFloor(body, GeminiBudgetFloor(0), caps.MaxOutput)
		return
	}

	// auto (or no resolvable budget) → -1 (dynamic). The JS toBudget returns -1
	// for mode "auto"; reasoning_effort "auto" is not in LEVEL_TO_BUDGET so
	// EffortToBudget reports not-ok and we treat it as the dynamic -1 path.
	budget := -1
	if !none {
		if b, ok := EffortToBudget(reasoningEffort); ok {
			budget = clampBudget(b, caps.ThinkingRange)
		}
	}

	SetGeminiThinking(body, map[string]any{
		"thinkingBudget":  budget,
		"includeThoughts": true,
	})
	EnsureGeminiOutputFloor(body, GeminiBudgetFloor(budget), caps.MaxOutput)
}

// numberOrZero reads a numeric body field as a float64, tolerating int/int64.
func numberOrZero(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}
