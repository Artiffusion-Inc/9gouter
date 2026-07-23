// normalize_passthrough.go ports open-sse/translator/formats/claude.js's
// normalizeClaudePassthrough: normalizes a native Claude passthrough body to
// match what Anthropic OAuth endpoints accept. Newer Cowork/Claude Code clients
// emit beta-only shapes that OAuth rejects:
//  1. thinking.type "adaptive" → unsupported on Haiku.
//  2. output_config.effort → unsupported on Haiku.
//  3. role "system" messages (mid-conversation-system beta) → only top-level
//     system is allowed.
//  4. (cd557a25) foreign thinking signatures leak into history when a combo
//     mixes models; Anthropic rejects them. Validate signatures, drop invalid
//     thinking blocks, and re-insert a placeholder when a tool_use requires one.
//
// normalizeClaudePassthrough mutates body in place and returns it.
package claude

import "strings"

// claudeBlockRedactedThinking is the redacted-thinking block type, which carries
// a signature like a normal thinking block and is validated the same way.
const claudeBlockRedactedThinking = "redacted_thinking"

// adaptiveThinkingUnsupported matches models that reject thinking.type
// "adaptive" + output_config.effort (Haiku; Opus 4.5+/Sonnet 4.6+ support it).
// Case-insensitive substring match, mirroring the JS /haiku/i regex.
var adaptiveThinkingUnsupported = "haiku"

// matchesAdaptiveUnsupported reports whether model contains the Haiku marker
// (case-insensitive). Empty model never matches, matching the JS behavior where
// the regex test against "" is false.
func matchesAdaptiveUnsupported(model string) bool {
	return model != "" && strings.Contains(strings.ToLower(model), adaptiveThinkingUnsupported)
}

// buildThinkingPlaceholder returns a minimal valid thinking block used to
// re-insert thinking ahead of a tool_use after foreign-signed thinking was
// dropped. Mirrors buildThinkingPlaceholder(provider) in JS; the "claude" branch
// always sets the signature (DeepSeek's branch is not used here).
func buildThinkingPlaceholder() map[string]any {
	return map[string]any{
		"type":      claudeBlockThinking,
		"thinking":  ".",
		"signature": defaultThinkingClaudeSignature,
	}
}

// NormalizeClaudePassthrough normalizes a native Claude passthrough body in place
// and returns it. It mirrors open-sse/translator/formats/claude.js step-for-step.
// A non-map body is returned unchanged.
func NormalizeClaudePassthrough(body map[string]any, model string) map[string]any {
	if body == nil {
		return body
	}

	// 1. Downgrade adaptive thinking for models that don't support it (Haiku).
	if thinking, ok := body["thinking"].(map[string]any); ok {
		if t, _ := thinking["type"].(string); t == "adaptive" && matchesAdaptiveUnsupported(model) {
			body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": float64(10000)}
		}
	}

	// 2a. Strip output_config.effort for models that don't support it (Haiku),
	// keeping other output_config fields. Drop output_config if it becomes empty.
	if matchesAdaptiveUnsupported(model) {
		if oc, ok := body["output_config"].(map[string]any); ok {
			if _, hasEffort := oc["effort"]; hasEffort {
				delete(oc, "effort")
				if len(oc) == 0 {
					delete(body, "output_config")
				} else {
					body["output_config"] = oc
				}
			}
		}
	}

	// 2b. Hoist mid-conversation role:system messages into the top-level system
	// field. OAuth endpoints only accept system at the top level.
	if msgs, ok := body["messages"].([]any); ok {
		var systemBlocks []any
		var kept []any
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				kept = append(kept, m)
				continue
			}
			if r, _ := msg["role"].(string); r == roleSystem {
				text := systemMessageText(msg["content"])
				if strings.TrimSpace(text) != "" {
					systemBlocks = append(systemBlocks, map[string]any{"type": claudeBlockText, "text": text})
				}
				continue
			}
			kept = append(kept, m)
		}
		if len(systemBlocks) > 0 {
			body["system"] = append(existingSystemBlocks(body["system"]), systemBlocks...)
			body["messages"] = kept
		}
	}

	// 3. Drop thinking blocks whose signature is not Claude's, and re-insert a
	// placeholder when a tool_use is left without a preceding thinking block.
	// thinkingEnabled is measured AFTER step 1, so a Haiku that came in as
	// "adaptive" now reads "enabled".
	thinkingEnabled := false
	if thinking, ok := body["thinking"].(map[string]any); ok {
		if t, _ := thinking["type"].(string); t == "enabled" {
			thinkingEnabled = true
		}
	}
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if r, _ := msg["role"].(string); r != roleAssistant {
				continue
			}
			blocks, ok := msg["content"].([]any)
			if !ok {
				continue
			}
			hasToolUse := false
			hasKeptThinking := false
			kept := make([]any, 0, len(blocks))
			for _, b := range blocks {
				block, ok := b.(map[string]any)
				if !ok {
					kept = append(kept, b)
					continue
				}
				typ, _ := block["type"].(string)
				if typ == claudeBlockThinking || typ == claudeBlockRedactedThinking {
					sig, _ := block["signature"].(string)
					if isValidClaudeSignature(sig) {
						hasKeptThinking = true
						kept = append(kept, block)
					}
					// Invalid/foreign signature: drop the block.
					continue
				}
				if typ == claudeBlockToolUse {
					hasToolUse = true
				}
				kept = append(kept, block)
			}
			msg["content"] = kept
			if thinkingEnabled && !hasKeptThinking && hasToolUse {
				// unshift: placeholder first, then the kept blocks.
				msg["content"] = append([]any{buildThinkingPlaceholder()}, kept...)
			}
		}
	}

	return body
}

// systemMessageText collapses a role:system message's content (string or block
// array) to a single text string, mirroring the JS inline collapse.
func systemMessageText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, b := range c {
			if s, ok := b.(string); ok {
				parts = append(parts, s)
				continue
			}
			if block, ok := b.(map[string]any); ok {
				if text, ok := block["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// existingSystemBlocks normalizes body.system into an array of text blocks:
// an array is kept as-is; a non-empty string is wrapped; anything else is empty.
func existingSystemBlocks(sys any) []any {
	switch s := sys.(type) {
	case []any:
		return s
	case string:
		if strings.TrimSpace(s) != "" {
			return []any{map[string]any{"type": claudeBlockText, "text": s}}
		}
	}
	return nil
}
