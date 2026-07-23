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
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/capabilities"
)

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
