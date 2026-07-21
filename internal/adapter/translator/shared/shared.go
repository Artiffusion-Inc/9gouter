// Package shared contains helpers used by multiple translator packages.
package shared

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// BuildChunk builds an OpenAI chat.completion.chunk.
func BuildChunk(id string, created int, model string, delta map[string]any, finishReason any) map[string]any {
	fr := finishReason
	if fr == nil {
		fr = nil
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":        0,
				"delta":        delta,
				"finish_reason": fr,
			},
		},
	}
}

// ReasoningDelta builds a reasoning_content delta.
func ReasoningDelta(text string) map[string]any {
	return map[string]any{"reasoning_content": text}
}

// ExtractReasoningText extracts reasoning text from a delta.
func ExtractReasoningText(delta map[string]any) string {
	if delta == nil {
		return ""
	}
	if s, ok := delta["reasoning_content"].(string); ok && s != "" {
		return s
	}
	if s, ok := delta["reasoning"].(string); ok && s != "" {
		return s
	}
	if details, ok := delta["reasoning_details"].([]any); ok {
		var parts []string
		for _, d := range details {
			switch x := d.(type) {
			case string:
				parts = append(parts, x)
			case map[string]any:
				if t, ok := x["text"].(string); ok {
					parts = append(parts, t)
				} else if t, ok := x["content"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// ToOpenAIFinish maps upstream finish/stop reasons to OpenAI finish_reason.
func ToOpenAIFinish(reason, format string) string {
	if reason == "" {
		return "stop"
	}
	format = strings.ToLower(format)
	switch format {
	case "claude":
		switch reason {
		case "end_turn", "stop_sequence":
			return "stop"
		case "max_tokens":
			return "length"
		case "tool_use":
			return "tool_calls"
		default:
			return "stop"
		}
	case "gemini":
		switch strings.ToUpper(reason) {
		case "STOP":
			return "stop"
		case "MAX_TOKENS":
			return "length"
		case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
			return "content_filter"
		default:
			return "stop"
		}
	case "commandcode":
		switch reason {
		case "stop":
			return "stop"
		case "length":
			return "length"
		case "tool-calls", "tool_use":
			return "tool_calls"
		case "content-filter":
			return "content_filter"
		case "error":
			return "stop"
		default:
			return reason
		}
	case "kiro", "ollama":
		switch reason {
		case "tool_calls", "tool_use":
			return "tool_calls"
		case "length", "max_tokens":
			return "length"
		default:
			return "stop"
		}
	default:
		return reason
	}
}

// FromOpenAIFinish maps OpenAI finish_reason to upstream stop reasons.
func FromOpenAIFinish(reason, format string) string {
	if strings.EqualFold(format, "claude") {
		switch reason {
		case "stop":
			return "end_turn"
		case "length":
			return "max_tokens"
		case "tool_calls":
			return "tool_use"
		default:
			return "end_turn"
		}
	}
	return reason
}

// BuildUsage constructs an OpenAI usage object.
func BuildUsage(promptTokens, completionTokens, totalTokens int, cachedTokens, cacheCreationTokens, reasoningTokens int) map[string]any {
	usage := map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      totalTokens,
	}
	if cachedTokens > 0 || cacheCreationTokens > 0 {
		details := map[string]any{}
		if cachedTokens > 0 {
			details["cached_tokens"] = cachedTokens
		}
		if cacheCreationTokens > 0 {
			details["cache_creation_tokens"] = cacheCreationTokens
		}
		usage["prompt_tokens_details"] = details
	}
	if reasoningTokens > 0 {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoningTokens}
	}
	return usage
}

// ToOpenAIUsage converts provider-native usage to OpenAI usage.
func ToOpenAIUsage(raw map[string]any, kind string) map[string]any {
	if raw == nil {
		return nil
	}
	n := func(v any) int {
		switch x := v.(type) {
		case float64:
			return int(x)
		case int:
			return x
		case int64:
			return int(x)
		case json.Number:
			i, _ := x.Int64()
			return int(i)
		}
		return 0
	}

	switch kind {
	case "claude":
		input := n(raw["input_tokens"])
		output := n(raw["output_tokens"])
		cacheRead := n(raw["cache_read_input_tokens"])
		cacheCreate := n(raw["cache_creation_input_tokens"])
		prompt := input + cacheRead + cacheCreate
		return BuildUsage(prompt, output, prompt+output, cacheRead, cacheCreate, 0)
	case "gemini":
		cached := n(raw["cachedContentTokenCount"])
		prompt := n(raw["promptTokenCount"])
		thoughts := n(raw["thoughtsTokenCount"])
		total := n(raw["totalTokenCount"])
		candidates := n(raw["candidatesTokenCount"])
		if candidates == 0 && total > 0 {
			candidates = total - prompt - thoughts
			if candidates < 0 {
				candidates = 0
			}
		}
		return BuildUsage(prompt, candidates+thoughts, total, cached, 0, thoughts)
	case "kiro":
		input := n(raw["inputTokens"])
		output := n(raw["outputTokens"])
		cached := n(raw["cache_read_input_tokens"]) + n(raw["cachedTokens"]) + n(raw["cached_tokens"])
		cacheCreation := n(raw["cache_creation_input_tokens"])
		u := BuildUsage(input, output, input+output, cached, cacheCreation, 0)
		if cached == 0 && cacheCreation == 0 {
			delete(u, "prompt_tokens_details")
		}
		return u
	case "ollama":
		input := n(raw["prompt_eval_count"])
		output := n(raw["eval_count"])
		return BuildUsage(input, output, input+output, 0, 0, 0)
	case "commandcode":
		input := n(raw["inputTokens"])
		output := n(raw["outputTokens"])
		total := n(raw["totalTokens"])
		if total == 0 {
			total = input + output
		}
		return BuildUsage(input, output, total, 0, 0, 0)
	default:
		return nil
	}
}

// Number coerces a value to int.
func Number(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		i, _ := x.Int64()
		return int(i)
	case string:
		var i int
		_, _ = fmt.Sscanf(x, "%d", &i)
		return i
	}
	return 0
}

// FallbackChatID returns a fallback chatcmpl id.
func FallbackChatID() string {
	return fmt.Sprintf("chatcmpl-%d", timeNowMs())
}

// GenerateMessageID returns a unique message id.
func GenerateMessageID() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	return fmt.Sprintf("msg_%d%06d", timeNowMs(), n.Int64()%1000000)
}

func timeNowMs() int64 {
	return time.Now().UnixMilli()
}
