// Package jsontosse synthesizes an OpenAI chat.completion.chunk SSE stream
// from a single, complete OpenAI chat-completion JSON body. It mirrors the
// behavior of open-sse/utils/jsonToSse.js (#3089 / OmniRoute) and is only
// applied when the client requested an OpenAI-shaped stream.
package translator

import (
	"encoding/json"
	"strings"
)

// Synthesize converts a single OpenAI chat-completion JSON body into an
// equivalent OpenAI SSE (chat.completion.chunk) stream. It returns "" when the
// input is not a parseable chat-completion object with at least one valid
// choice; callers then fall back to the existing (error) handling.
func Synthesize(body []byte) (string, error) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", nil
	}
	if parsed == nil {
		return "", nil
	}

	choicesRaw, ok := parsed["choices"].([]any)
	if !ok || len(choicesRaw) == 0 {
		return "", nil
	}

	id, _ := parsed["id"].(string)
	if id == "" {
		id = "chatcmpl-9router-sse"
	}
	createdF, _ := parsed["created"].(float64)
	created := int64(createdF)
	model, _ := parsed["model"].(string)

	base := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
	}

	var out strings.Builder
	emittedAny := false

	for fallbackIndex, choiceRaw := range choicesRaw {
		choice, ok := choiceRaw.(map[string]any)
		if !ok {
			continue
		}

		indexF, _ := choice["index"].(float64)
		index := int(indexF)
		if _, hasIndex := choice["index"]; !hasIndex {
			index = fallbackIndex
		}

		messageRaw, _ := choice["message"].(map[string]any)
		if messageRaw == nil {
			messageRaw = map[string]any{}
		}

		role, _ := messageRaw["role"].(string)
		if role == "" {
			role = "assistant"
		}

		emitDelta := func(delta map[string]any) {
			chunk := map[string]any{
				"id":     base["id"],
				"object": base["object"],
				"created": base["created"],
				"model":  base["model"],
				"choices": []any{
					map[string]any{
						"index":        index,
						"delta":        delta,
						"finish_reason": nil,
					},
				},
			}
			writeEvent(&out, chunk)
		}

		emitDelta(map[string]any{"role": role})

		reasoningDelta := buildReasoningDelta(messageRaw)
		if reasoningDelta != nil {
			emitDelta(reasoningDelta)
		}

		if content, ok := messageRaw["content"].(string); ok && content != "" {
			emitDelta(map[string]any{"content": content})
		}

		if toolCallsRaw, ok := messageRaw["tool_calls"].([]any); ok && len(toolCallsRaw) > 0 {
			emitDelta(map[string]any{"tool_calls": toolCallsRaw})
		}

		finishReason := normalizeOpenAICompatibleFinishReasonString(choice["finish_reason"], "stop")
		finalChunk := map[string]any{
			"id":     base["id"],
			"object": base["object"],
			"created": base["created"],
			"model":  base["model"],
			"choices": []any{
				map[string]any{
					"index":         index,
					"delta":         map[string]any{},
					"finish_reason": finishReason,
				},
			},
		}
		if usage, ok := parsed["usage"].(map[string]any); ok && len(usage) > 0 {
			finalChunk["usage"] = usage
		}
		writeEvent(&out, finalChunk)
		emittedAny = true
	}

	if !emittedAny {
		return "", nil
	}
	out.WriteString("data: [DONE]\n\n")
	return out.String(), nil
}

func writeEvent(out *strings.Builder, payload map[string]any) {
	b, _ := json.Marshal(payload)
	out.WriteString("data: ")
	out.Write(b)
	out.WriteString("\n\n")
}

// buildReasoningDelta extracts readable or unsupported reasoning fields from a
// message object, matching jsonToSse.js helpers and reasoningFields.js.
func buildReasoningDelta(message map[string]any) map[string]any {
	delta := make(map[string]any)

	if reasoningDetails, ok := message["reasoning_details"].([]any); ok && len(reasoningDetails) > 0 {
		delta["reasoning_details"] = reasoningDetails
	}

	if addReadableReasoning(message, delta) {
		return delta
	}
	addUnsupportedReasoning(message, delta)
	if len(delta) == 0 {
		return nil
	}
	return delta
}

func addReadableReasoning(message, delta map[string]any) bool {
	if content, ok := message["reasoning_content"].(string); ok && content != "" {
		delta["reasoning_content"] = content
		return true
	}
	if reasoning, ok := message["reasoning"].(string); ok && reasoning != "" {
		delta["reasoning"] = reasoning
		return true
	}
	return false
}

func addUnsupportedReasoning(message, delta map[string]any) {
	if v := getUnsupportedReasoningValue(message); v != "" {
		delta["reasoning_content"] = v
	}
}

func getUnsupportedReasoningValue(value map[string]any) string {
	for _, key := range []string{"reasoning_text", "reasoning_content_polyfill", "thinking"} {
		if v, ok := value[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// normalizeOpenAICompatibleFinishReasonString maps vendor finish_reason strings
// to the OpenAI canonical set, ported from open-sse/utils/finishReason.js.
func normalizeOpenAICompatibleFinishReasonString(value any, fallback string) *string {
	if fallback == "" {
		fallback = "stop"
	}
	s, ok := value.(string)
	if !ok || s == "" {
		return &fallback
	}
	normalized := strings.ToLower(s)
	if isOpenAIFinishReason(normalized) {
		return &normalized
	}
	if normalized == "max_tokens" {
		stop := "length"
		return &stop
	}
	if isSafetyFinishReason(normalized) {
		cf := "content_filter"
		return &cf
	}
	return &normalized
}

func isOpenAIFinishReason(s string) bool {
	switch s {
	case "stop", "length", "tool_calls", "content_filter", "function_call":
		return true
	}
	return false
}

func isSafetyFinishReason(s string) bool {
	switch s {
	case "safety", "recitation", "blocklist", "prohibited_content", "content_filtered", "policy_violation", "malformed_response":
		return true
	}
	return false
}
