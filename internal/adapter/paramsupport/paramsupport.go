// Package paramsupport ports open-sse/translator/concerns/paramSupport.js into
// Go: a config-driven table of per-provider/model request-param rules applied
// before dispatch so unsupported params do not reach the upstream and trigger
// HTTP 400. Instead of scattering `delete body.x` across executors, every rule
// lives in STRIP_RULES and StripUnsupportedParams walks them in place.
//
// Rule kinds (mirroring the JS table):
//   - drop: remove listed keys when present (!= nil).
//   - flattenContent: collapse OpenAI content-part arrays to a plain string
//     (Cloudflare Workers AI rejects the array shape, #1926).
//   - clampToModelMaxOutput / maxOutputCap: clamp max_tokens /
//     max_completion_tokens / max_output_tokens to min(model maxOutput, cap)
//     (volcengine-ark GLM-5 / Kimi, bbae990b / cfbdf060).
package paramsupport

import (
	"regexp"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/capabilities"
)

// stripRule is one STRIP_RULES entry. provider empty = matches any provider.
// match is a regexp tested against the model id (case-insensitive via the
// pattern itself); a nil match matches any model. The predicate-only JS rule
// (github Claude-not-4.6) is expressed as a separate matcher field so Go keeps
// the same one-pass walk without a JS function type.
type stripRule struct {
	provider              string
	match                 *regexp.Regexp
	predicate             func(model string) bool
	drop                  []string
	flattenContent        bool
	clampToModelMaxOutput bool
	maxOutputCap          int // 0 = no explicit cap
}

// STRIP_RULES mirrors open-sse/translator/concerns/paramSupport.js. Order
// matters only for readability; rules are independent and applied in sequence.
var stripRules = []stripRule{
	// All Claude models: temperature deprecated/rejected upstream (Anthropic 400). #1748
	{match: regexp.MustCompile(`(?i)claude`), drop: []string{"temperature"}},
	// GitHub Copilot gpt-5.4: temperature unsupported.
	{provider: "github", match: regexp.MustCompile(`(?i)gpt-5\.4`), drop: []string{"temperature"}},
	// GitHub Copilot Claude (except opus/sonnet 4.6): thinking + reasoning_effort rejected. #713
	{
		provider: "github",
		predicate: func(m string) bool {
			matched, _ := regexp.MatchString(`(?i)claude`, m)
			if !matched {
				return false
			}
			is46, _ := regexp.MatchString(`(?i)claude.*(opus|sonnet).*4\.6`, m)
			return !is46
		},
		drop: []string{"thinking", "reasoning_effort"},
	},
	// Cloudflare Workers AI: content must be plain string, rejects the
	// OpenAI content-part array (#1926).
	{provider: "cloudflare-ai", flattenContent: true},
	// volcengine-ark GLM-5: clamp max_tokens to the model's advertised maxOutput
	// (bbae990b).
	{provider: "volcengine-ark", match: regexp.MustCompile(`(?i)glm-5`), clampToModelMaxOutput: true},
	// volcengine-ark Kimi: the endpoint caps max_tokens at 32768 even though the
	// model advertises a far higher ceiling (Kimi-K2.7-Code maxOutput 262144),
	// so clampToModelMaxOutput alone leaves it uncapped and the request 400s.
	// Pin an explicit endpoint cap; min() with the model ceiling still applies
	// if a variant's own limit is lower (cfbdf060).
	{provider: "volcengine-ark", match: regexp.MustCompile(`(?i)kimi`), maxOutputCap: 32768, clampToModelMaxOutput: true},
}

// matches reports whether a rule applies to the model id. A nil/empty match and
// a nil predicate both mean "any model". A predicate, when set, takes precedence
// over the regexp (the github Claude-not-4.6 rule is predicate-only).
func (r stripRule) matches(model string) bool {
	if r.predicate != nil {
		return r.predicate(model)
	}
	if r.match == nil {
		return true
	}
	return r.match.MatchString(model)
}

// StripUnsupportedParams removes/clamps unsupported params from body in place,
// mirroring stripUnsupportedParams(provider, model, body). It is a no-op when
// model or body is empty. Returns body for convenience.
func StripUnsupportedParams(provider, model string, body map[string]any) map[string]any {
	if model == "" || body == nil {
		return body
	}
	for _, rule := range stripRules {
		if rule.provider != "" && rule.provider != provider {
			continue
		}
		if !rule.matches(model) {
			continue
		}
		for _, key := range rule.drop {
			if _, ok := body[key]; ok {
				delete(body, key)
			}
		}
		if rule.flattenContent {
			flattenMessageContent(body)
		}
		if rule.clampToModelMaxOutput || rule.maxOutputCap > 0 {
			applyMaxOutputClamp(provider, model, body, rule)
		}
	}
	return body
}

// flattenMessageContent collapses each message's content-part array to a plain
// string (Cloudflare Workers AI, #1926). Non-array content is left untouched.
func flattenMessageContent(body map[string]any) {
	messages, ok := body["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		var joined string
		for _, p := range parts {
			block, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t == "text" {
				if text, ok := block["text"].(string); ok {
					joined += text
				}
			}
		}
		msg["content"] = joined
	}
}

// applyMaxOutputClamp clamps max_tokens / max_completion_tokens / max_output_tokens
// to min(modelCeiling, maxOutputCap), mirroring the JS candidates + Math.min.
// modelCeiling comes from the capabilities table; maxOutputCap is the rule's
// explicit endpoint cap (e.g. Kimi 32768).
func applyMaxOutputClamp(provider, model string, body map[string]any, rule stripRule) {
	var candidates []int
	if rule.clampToModelMaxOutput {
		if ceiling := capabilities.GetCapabilitiesForModel(provider, model).MaxOutput; ceiling > 0 {
			candidates = append(candidates, ceiling)
		}
	}
	if rule.maxOutputCap > 0 {
		candidates = append(candidates, rule.maxOutputCap)
	}
	if len(candidates) == 0 {
		return
	}
	ceiling := candidates[0]
	for _, c := range candidates[1:] {
		if c < ceiling {
			ceiling = c
		}
	}
	clampNumber(body, "max_tokens", ceiling)
	clampNumber(body, "max_completion_tokens", ceiling)
	clampNumber(body, "max_output_tokens", ceiling)
}

// clampNumber lowers body[key] to ceiling when it is a finite number above it.
// Mirrors the JS clampNumber (only clamps down, never raises).
func clampNumber(body map[string]any, key string, ceiling int) {
	switch v := body[key].(type) {
	case float64:
		if v > float64(ceiling) {
			body[key] = float64(ceiling)
		}
	case int:
		if v > ceiling {
			body[key] = ceiling
		}
	case int64:
		if v > int64(ceiling) {
			body[key] = ceiling
		}
	}
}
