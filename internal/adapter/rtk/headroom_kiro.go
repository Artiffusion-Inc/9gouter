package rtk

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// headroom_kiro.go ports the Kiro Headroom compression branch of
// open-sse/rtk/headroom.js (#2488 / 65c65a0f). Kiro carries its conversation in
// body.conversationState (history[] + currentMessage), not in body.messages.
// The /v1/compress proxy only understands OpenAI chat messages, so the Kiro
// conversationState is projected to OpenAI messages, compressed, and the
// compressed text is written back into the original Kiro fields — preserving
// the provider payload shape for Kiro's executor. Fail-open when the proxy
// returns malformed or reordered messages.

// kiroHeadroomTarget is a pointer into the original Kiro body where a projected
// message's text lives, so the compressed text can be written back in place.
type kiroHeadroomTarget struct {
	object map[string]any
	key    string
}

// kiroHeadroomProjection is the set of OpenAI messages projected from a Kiro
// conversationState plus the in-order targets to write compressed text back to.
type kiroHeadroomProjection struct {
	messages []map[string]any
	targets  []kiroHeadroomTarget
}

// collectKiroHeadroomMessages mirrors collectKiroHeadroomMessages in headroom.js:
// walks conversationState.history[] + currentMessage, projecting each
// userInputMessage / assistantResponseMessage to OpenAI messages (system/user/
// tool/assistant, with tool_calls and tool_call_id where present) and recording
// the target field each text came from. Returns nil when there is nothing to
// compress (no conversationState or no text-bearing messages).
func collectKiroHeadroomMessages(body map[string]any) *kiroHeadroomProjection {
	state, ok := body["conversationState"].(map[string]any)
	if !ok {
		return nil
	}

	projection := &kiroHeadroomProjection{}

	addTextTarget := func(role, text string, target kiroHeadroomTarget, extra map[string]any) {
		if text == "" {
			return
		}
		msg := map[string]any{"role": role, "content": text}
		for k, v := range extra {
			msg[k] = v
		}
		projection.messages = append(projection.messages, msg)
		projection.targets = append(projection.targets, target)
	}

	toToolCalls := func(toolUses any) []any {
		uses, ok := toolUses.([]any)
		if !ok || len(uses) == 0 {
			return nil
		}
		var calls []any
		for _, uAny := range uses {
			u, ok := uAny.(map[string]any)
			if !ok {
				continue
			}
			name, _ := u["name"].(string)
			id, _ := u["toolUseId"].(string)
			input := u["input"]
			if input == nil {
				input = map[string]any{}
			}
			argsBytes, _ := json.Marshal(input)
			if id == "" && name == "" {
				continue
			}
			calls = append(calls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": string(argsBytes),
				},
			})
		}
		if len(calls) == 0 {
			return nil
		}
		return calls
	}

	visit := func(item any) {
		it, ok := item.(map[string]any)
		if !ok {
			return
		}
		if user, ok := it["userInputMessage"].(map[string]any); ok {
			if sys, ok := user["systemInstruction"].(string); ok {
				addTextTarget("system", sys, kiroHeadroomTarget{object: user, key: "systemInstruction"}, nil)
			}
			if content, ok := user["content"].(string); ok {
				addTextTarget("user", content, kiroHeadroomTarget{object: user, key: "content"}, nil)
			}
			if ctx, ok := user["userInputMessageContext"].(map[string]any); ok {
				if toolResults, ok := ctx["toolResults"].([]any); ok {
					for _, trAny := range toolResults {
						tr, ok := trAny.(map[string]any)
						if !ok {
							continue
						}
						toolUseID, _ := tr["toolUseId"].(string)
						content, ok := tr["content"].([]any)
						if !ok {
							continue
						}
						for _, partAny := range content {
							part, ok := partAny.(map[string]any)
							if !ok {
								continue
							}
							if text, ok := part["text"].(string); ok {
								extra := map[string]any{}
								if toolUseID != "" {
									extra["tool_call_id"] = toolUseID
								}
								addTextTarget("tool", text, kiroHeadroomTarget{object: part, key: "text"}, extra)
							}
						}
					}
				}
			}
			return
		}
		if assistant, ok := it["assistantResponseMessage"].(map[string]any); ok {
			var extra map[string]any
			if toolCalls := toToolCalls(assistant["toolUses"]); toolCalls != nil {
				extra = map[string]any{"tool_calls": toolCalls}
			}
			if content, ok := assistant["content"].(string); ok {
				addTextTarget("assistant", content, kiroHeadroomTarget{object: assistant, key: "content"}, extra)
			}
		}
	}

	if history, ok := state["history"].([]any); ok {
		for _, item := range history {
			visit(item)
		}
	}
	if current, ok := state["currentMessage"]; ok && current != nil {
		visit(current)
	}

	if len(projection.messages) == 0 {
		return nil
	}
	return projection
}

// textFromHeadroomMessage mirrors textFromHeadroomMessage: a compressed message
// carries text either as a plain string or as an array of string/{text} parts.
// Returns "", false when no text could be extracted.
func textFromHeadroomMessage(msg map[string]any) (string, bool) {
	switch content := msg["content"].(type) {
	case string:
		return content, true
	case []any:
		var parts []string
		for _, partAny := range content {
			if s, ok := partAny.(string); ok {
				parts = append(parts, s)
				continue
			}
			if part, ok := partAny.(map[string]any); ok {
				if t, ok := part["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return strings.Join(parts, "\n"), true
	}
	return "", false
}

// applyKiroHeadroomMessages mirrors applyKiroHeadroomMessages: writes the
// compressed text back into the original Kiro fields. Fail-open (returns false)
// when the proxy response is malformed, has a mismatched message count, or does
// not preserve role order — the body is left untouched in those cases.
func applyKiroHeadroomMessages(projection *kiroHeadroomProjection, compressed []HeadroomMessage, diag *HeadroomDiagnostics) bool {
	if len(compressed) != len(projection.messages) {
		setDiagnostic(diag, "proxy response did not match Kiro message count")
		return false
	}
	type update struct {
		target kiroHeadroomTarget
		text   string
	}
	var updates []update
	for i, expected := range projection.messages {
		actual := compressed[i]
		if actual.Role != expected["role"] {
			setDiagnostic(diag, "proxy response did not preserve Kiro message order")
			return false
		}
		text, hasText := textFromHeadroomMessage(map[string]any{"content": actual.Content})
		if !hasText {
			setDiagnostic(diag, "proxy response missing Kiro text content")
			return false
		}
		updates = append(updates, update{target: projection.targets[i], text: text})
	}
	for _, u := range updates {
		u.target.object[u.target.key] = u.text
	}
	return true
}

// compressKiroViaHeadroomImpl ports the `format === "kiro"` branch of
// compressWithHeadroom: project conversationState → OpenAI messages →
// /v1/compress → write compressed text back into the original Kiro fields.
func compressKiroViaHeadroomImpl(body map[string]any, cfg HeadroomConfig, client *http.Client, timeoutMs int) (*HeadroomStats, error) {
	projection := collectKiroHeadroomMessages(body)
	if projection == nil {
		setDiagnostic(cfg.Diagnostics, "Kiro request did not project to messages[]")
		return nil, nil
	}
	messages := make([]any, len(projection.messages))
	for i, m := range projection.messages {
		messages[i] = m
	}
	stats, err := callCompress(cfg.URL, messages, cfg.Model, timeoutMs, cfg.CompressUserMessages, client, cfg.Diagnostics)
	if err != nil {
		return nil, err
	}
	if stats == nil || len(stats.Messages) == 0 {
		return nil, errHeadroomNoMessages
	}
	if !applyKiroHeadroomMessages(projection, stats.Messages, cfg.Diagnostics) {
		return nil, nil
	}
	return stats, nil
}

var errHeadroomNoMessages = fmt.Errorf("proxy returned no messages")
