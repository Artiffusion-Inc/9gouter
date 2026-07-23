package paramsupport

import (
	"testing"
)

// TestStripClaudeTemperature ports STRIP_RULES #1748: every Claude model drops
// temperature; a non-Claude model keeps it.
func TestStripClaudeTemperature(t *testing.T) {
	body := map[string]any{"model": "claude-opus-4.8", "temperature": 0.7, "max_tokens": float64(1024)}
	StripUnsupportedParams("", "claude-opus-4.8", body)
	if _, ok := body["temperature"]; ok {
		t.Error("claude temperature should be dropped")
	}
	// Non-Claude keeps temperature.
	body2 := map[string]any{"model": "gpt-5", "temperature": 0.7}
	StripUnsupportedParams("", "gpt-5", body2)
	if v, ok := body2["temperature"].(float64); !ok || v != 0.7 {
		t.Error("non-claude temperature should be kept")
	}
}

// TestStripGitHubClaudeThinking ports #713: GitHub Copilot Claude (except
// opus/sonnet 4.6) drops thinking + reasoning_effort. A 4.6 model keeps them.
func TestStripGitHubClaudeThinking(t *testing.T) {
	body := map[string]any{"thinking": map[string]any{"type": "enabled"}, "reasoning_effort": "high"}
	StripUnsupportedParams("github", "claude-opus-4.8", body)
	if _, ok := body["thinking"]; ok {
		t.Error("github claude-opus-4.8 thinking should be dropped")
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Error("github claude-opus-4.8 reasoning_effort should be dropped")
	}
	// 4.6 variant is exempt: keeps both.
	body46 := map[string]any{"thinking": map[string]any{"type": "enabled"}, "reasoning_effort": "high"}
	StripUnsupportedParams("github", "claude-opus-4.6", body46)
	if _, ok := body46["thinking"]; !ok {
		t.Error("github claude-opus-4.6 thinking should be kept (exempt)")
	}
	if _, ok := body46["reasoning_effort"]; !ok {
		t.Error("github claude-opus-4.6 reasoning_effort should be kept (exempt)")
	}
	// Non-github provider does not trigger the rule.
	bodyNg := map[string]any{"thinking": map[string]any{"type": "enabled"}}
	StripUnsupportedParams("openai", "claude-opus-4.8", bodyNg)
	if _, ok := bodyNg["thinking"]; !ok {
		t.Error("non-github provider should not drop thinking via github rule")
	}
}

// TestStripGitHubGPT54Temperature ports the GitHub gpt-5.4 temperature drop.
func TestStripGitHubGPT54Temperature(t *testing.T) {
	body := map[string]any{"temperature": 0.5}
	StripUnsupportedParams("github", "gpt-5.4", body)
	if _, ok := body["temperature"]; ok {
		t.Error("github gpt-5.4 temperature should be dropped")
	}
}

// TestFlattenContentCloudflare ports #1926: Cloudflare Workers AI rejects the
// OpenAI content-part array; it is flattened to a plain string.
func TestFlattenContentCloudflare(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "hello "},
					map[string]any{"type": "text", "text": "world"},
				},
			},
		},
	}
	StripUnsupportedParams("cloudflare-ai", "some-model", body)
	msg := body["messages"].([]any)[0].(map[string]any)
	if c, ok := msg["content"].(string); !ok || c != "hello world" {
		t.Errorf("content = %#v, want \"hello world\"", msg["content"])
	}
}

// TestVolcengineArkGLM5Clamp ports bbae990b: volcengine-ark GLM-5 clamps
// max_tokens to the model's advertised maxOutput. Bare "glm-5" matches the
// *glm-5* pattern → MaxOutput 128000 (the clamp never raises, only lowers).
func TestVolcengineArkGLM5Clamp(t *testing.T) {
	body := map[string]any{"max_tokens": float64(200000), "max_completion_tokens": float64(999999)}
	StripUnsupportedParams("volcengine-ark", "glm-5", body)
	if mt, _ := body["max_tokens"].(float64); mt != 128000 {
		t.Errorf("glm-5 max_tokens = %v, want 128000 (clamped to *glm-5* pattern ceiling)", mt)
	}
	if mc, _ := body["max_completion_tokens"].(float64); mc != 128000 {
		t.Errorf("glm-5 max_completion_tokens = %v, want 128000", mc)
	}
}

// TestVolcengineArkKimiCap ports cfbdf060: volcengine-ark Kimi caps max_tokens
// at min(modelCeiling, 32768). Kimi-K2.7-Code advertises maxOutput 65536, so the
// 32768 endpoint cap wins.
func TestVolcengineArkKimiCap(t *testing.T) {
	body := map[string]any{"max_tokens": float64(200000), "max_output_tokens": float64(50000)}
	// kimi-k2.7-code exact id → maxOutput 65536; endpoint cap 32768 → min = 32768.
	StripUnsupportedParams("volcengine-ark", "kimi-k2.7-code", body)
	if mt, _ := body["max_tokens"].(float64); mt != 32768 {
		t.Errorf("kimi-k2.7-code max_tokens = %v, want 32768 (endpoint cap)", mt)
	}
	if mo, _ := body["max_output_tokens"].(float64); mo != 32768 {
		t.Errorf("kimi-k2.7-code max_output_tokens = %v, want 32768", mo)
	}
}

// TestVolcengineArkKimiModelCeilingWinsWhenLower verifies min() picks the lower
// of modelCeiling and endpoint cap. A Kimi variant whose own maxOutput is below
// 32768 clamps to its own (lower) ceiling.
func TestVolcengineArkKimiModelCeilingWinsWhenLower(t *testing.T) {
	// kimi-for-coding exact id → maxOutput 65536; still above 32768 → 32768 wins.
	body := map[string]any{"max_tokens": float64(200000)}
	StripUnsupportedParams("volcengine-ark", "kimi-for-coding", body)
	if mt, _ := body["max_tokens"].(float64); mt != 32768 {
		t.Errorf("kimi-for-coding max_tokens = %v, want 32768", mt)
	}
}

// TestClampNeverRaises verifies clampNumber only lowers, never raises a value
// already below the ceiling.
func TestClampNeverRaises(t *testing.T) {
	body := map[string]any{"max_tokens": float64(1000)}
	StripUnsupportedParams("volcengine-ark", "kimi-k2.7-code", body)
	if mt, _ := body["max_tokens"].(float64); mt != 1000 {
		t.Errorf("clamp raised max_tokens to %v, want 1000 (never raises)", mt)
	}
}

// TestStripUnsupportedParamsNoOpOnEmpty verifies the guard clauses.
func TestStripUnsupportedParamsNoOpOnEmpty(t *testing.T) {
	if got := StripUnsupportedParams("", "", map[string]any{"temperature": 0.7}); got["temperature"] == nil {
		t.Error("empty model should be a no-op")
	}
	if got := StripUnsupportedParams("claude-opus-4.8", "claude-opus-4.8", nil); got != nil {
		t.Error("nil body should be a no-op")
	}
}

// TestStripNonMatchingProvider verifies a rule with a provider gate is skipped
// for other providers (e.g. github rules do not fire on openai).
func TestStripNonMatchingProvider(t *testing.T) {
	body := map[string]any{"temperature": 0.5}
	StripUnsupportedParams("openai", "gpt-5.4", body)
	if _, ok := body["temperature"]; !ok {
		t.Error("openai provider should not trigger github gpt-5.4 rule")
	}
}
