// claude_to_openai_request.go ports open-sse/translator/request/claude-to-openai.js:
// the Claude→OpenAI Chat request translator, the inverse of openai.go's
// openaiToClaudeRequest. It registers itself on the translator registry at init
// time (format.Claude → format.Openai) so a Claude-shaped client body routed
// to an OpenAI upstream is pivoted through OpenAI by the registry.
//
// Ports upstream:
//   - 749c2e3f: a mid-conversation role:system message is mapped to role:user
//     wrapped in <instructions>…</instructions> (Anthropic prefill 400 avoidance).
//   - 3a866fe1: reasoning_effort / reasoning are carried into the OpenAI result.
package claude

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

const (
	roleUser   = "user"
	roleSystem = "system"
	roleTool   = "tool"

	openaiBlockText     = "text"
	openaiBlockImageURL = "image_url"
	openaiBlockFunction = "function"

	claudeBlockImage = "image"
)

func init() {
	translator.RegisterRequest(format.Claude, format.Openai, claudeToOpenaiRequestTranslator{})
}

type claudeToOpenaiRequestTranslator struct{}

func (claudeToOpenaiRequestTranslator) TranslateRequest(model string, body json.RawMessage, stream bool, providerID string) (json.RawMessage, error) {
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("unmarshal claude body: %w", err)
	}
	out := claudeToOpenAIRequest(model, in, stream)
	return json.Marshal(out)
}

// claudeToOpenAIRequest converts a Claude-format request body to the OpenAI Chat
// shape, mirroring open-sse/translator/request/claude-to-openai.js.
func claudeToOpenAIRequest(model string, body map[string]any, stream bool) map[string]any {
	result := map[string]any{
		"model":    model,
		"messages": []any{},
		"stream":   stream,
	}

	// Max tokens (carry through; no clamp here — the OpenAI leg owns that).
	if mt, ok := body["max_tokens"]; ok {
		result["max_tokens"] = mt
	}

	// Temperature.
	if t, ok := body["temperature"]; ok {
		result["temperature"] = t
	}

	// System message: Claude system is either a string or an array of text
	// blocks. Strip the x-anthropic-billing-header marker if present.
	if sys, ok := body["system"]; ok {
		systemContent := systemToText(sys)
		if systemContent != "" {
			result["messages"] = append(result["messages"].([]any), map[string]any{
				"role":    roleSystem,
				"content": systemContent,
			})
		}
	}

	// Convert messages.
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			converted := convertClaudeMessage(msg)
			if converted == nil {
				continue
			}
			if many, ok := converted.([]any); ok {
				result["messages"] = append(result["messages"].([]any), many...)
			} else if one, ok := converted.(map[string]any); ok {
				result["messages"] = append(result["messages"].([]any), one)
			}
		}
	}

	// Fix missing tool responses — OpenAI requires every tool_call to have a
	// matching tool reply. Local variant of the global concerns/toolCall check,
	// run on the openai leg. Returns a new slice (may grow) so the caller can
	// reassign — a slice header is passed by value, in-place append cannot
	// propagate growth back to result["messages"].
	result["messages"] = fixMissingToolResponsesOpenAI(result["messages"].([]any))

	// Tools.
	if rawTools, ok := body["tools"].([]any); ok {
		tools := []any{}
		for _, t := range rawTools {
			tool, ok := t.(map[string]any)
			if !ok {
				continue
			}
			name, _ := tool["name"].(string)
			desc, _ := tool["description"].(string)
			params := tool["input_schema"]
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{
				"type": openaiBlockFunction,
				"function": map[string]any{
					"name":        name,
					"description": desc,
					"parameters":  params,
				},
			})
		}
		if len(tools) > 0 {
			result["tools"] = tools
		}
	}

	// Tool choice.
	if tc, ok := body["tool_choice"]; ok {
		result["tool_choice"] = convertToolChoice(tc)
	}

	// Carry reasoning_effort / reasoning through (upstream 3a866fe1).
	if re, ok := body["reasoning_effort"]; ok {
		result["reasoning_effort"] = re
	} else if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"]; ok {
			result["reasoning_effort"] = effort
		}
	}
	if reasoning, ok := body["reasoning"]; ok {
		result["reasoning"] = reasoning
	}

	return result
}

// systemToText collapses a Claude `system` field (string or text-block array)
// to a single string, stripping the x-anthropic-billing-header marker.
func systemToText(sys any) string {
	switch s := sys.(type) {
	case string:
		return stripAnthropicBillingHeader(s)
	case []any:
		var parts []string
		for _, b := range s {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t == claudeBlockText {
				if text, ok := block["text"].(string); ok && text != "" {
					parts = append(parts, stripAnthropicBillingHeader(text))
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// stripAnthropicBillingHeader removes the leading x-anthropic-billing-header
// marker line that some Claude clients prepend to system text.
func stripAnthropicBillingHeader(text string) string {
	for {
		trimmed := strings.TrimLeft(text, " \t")
		if !strings.HasPrefix(trimmed, "x-anthropic-billing-header:") {
			return trimmed
		}
		nl := strings.IndexAny(trimmed, "\r\n")
		if nl < 0 {
			return ""
		}
		// Skip past the newline.
		rest := trimmed[nl:]
		if strings.HasPrefix(rest, "\r\n") {
			text = rest[2:]
		} else {
			text = rest[1:]
		}
	}
}

// convertClaudeMessage converts one Claude message to an OpenAI message (or an
// array of messages when it carries multiple tool_results). Returns nil to drop.
func convertClaudeMessage(msg map[string]any) any {
	// Mid-conversation system message -> user wrapped in <instructions>
	// (upstream 749c2e3f, avoids Anthropic prefill 400).
	if role, _ := msg["role"].(string); role == roleSystem {
		text := systemReminderText(msg["content"])
		if text == "" {
			return nil
		}
		return map[string]any{"role": roleUser, "content": text}
	}

	role := roleAssistant
	if r, _ := msg["role"].(string); r == roleUser || r == roleTool {
		role = roleUser
	}

	// Simple string content.
	if c, ok := msg["content"].(string); ok {
		return map[string]any{"role": role, "content": c}
	}

	// Array content.
	blocks, ok := msg["content"].([]any)
	if !ok {
		return nil
	}
	var parts []any
	var toolCalls []any
	var toolResults []any
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := block["type"].(string)
		switch typ {
		case claudeBlockText:
			parts = append(parts, map[string]any{"type": openaiBlockText, "text": block["text"]})
		case claudeBlockImage:
			if source, ok := block["source"].(map[string]any); ok {
				if st, _ := source["type"].(string); st == "base64" {
					media, _ := source["media_type"].(string)
					data, _ := source["data"].(string)
					parts = append(parts, map[string]any{
						"type": openaiBlockImageURL,
						"image_url": map[string]any{
							"url": encodeDataUri(media, data),
						},
					})
				}
			}
		case claudeBlockToolUse:
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			args := block["input"]
			if args == nil {
				args = map[string]any{}
			}
			argsJSON, _ := json.Marshal(args)
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": openaiBlockFunction,
				"function": map[string]any{
					"name":      name,
					"arguments": string(argsJSON),
				},
			})
		case claudeBlockToolResult:
			toolResults = append(toolResults, map[string]any{
				"role":         roleTool,
				"tool_call_id": block["tool_use_id"],
				"content":      toolResultContent(block["content"]),
			})
		}
	}

	// Tool results win: return array of tool messages (+ a trailing user turn
	// if there were text parts alongside).
	if len(toolResults) > 0 {
		out := toolResults
		if len(parts) > 0 {
			out = append(out, map[string]any{"role": roleUser, "content": collapseTextParts(parts)})
		}
		return out
	}
	// Tool calls: assistant message with tool_calls.
	if len(toolCalls) > 0 {
		m := map[string]any{"role": roleAssistant}
		if len(parts) > 0 {
			m["content"] = collapseTextParts(parts)
		}
		m["tool_calls"] = toolCalls
		return m
	}
	// Plain content.
	if len(parts) > 0 {
		return map[string]any{"role": role, "content": collapseTextParts(parts)}
	}
	// Empty content array.
	if len(blocks) == 0 {
		return map[string]any{"role": role, "content": ""}
	}
	return nil
}

// systemReminderText wraps mid-conversation system text in <instructions> tags
// so it ends as a user turn (upstream 749c2e3f).
func systemReminderText(content any) string {
	var parts []string
	switch c := content.(type) {
	case string:
		parts = []string{c}
	case []any:
		for _, b := range c {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t == claudeBlockText {
				if text, ok := block["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
	}
	text := strings.Join(filterNonEmpty(parts), "\n")
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return "<instructions>\n" + text + "\n</instructions>"
}

// toolResultContent extracts the text from a Claude tool_result content field
// (string, text-block array, or arbitrary JSON fallback).
func toolResultContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, b := range c {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t == claudeBlockText {
				if text, ok := block["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		joined := strings.Join(parts, "\n")
		if joined != "" {
			return joined
		}
		if len(c) > 0 {
			raw, _ := json.Marshal(content)
			return string(raw)
		}
	case nil:
		return ""
	default:
		raw, _ := json.Marshal(content)
		return string(raw)
	}
	return ""
}

// collapseTextParts joins OpenAI text content parts into a plain string when the
// array holds only text blocks, matching concerns/message.js collapseTextParts.
func collapseTextParts(parts []any) any {
	if len(parts) == 0 {
		return ""
	}
	allText := true
	var sb strings.Builder
	for _, p := range parts {
		block, ok := p.(map[string]any)
		if !ok {
			allText = false
			break
		}
		if t, _ := block["type"].(string); t != openaiBlockText {
			allText = false
			break
		}
		if text, ok := block["text"].(string); ok {
			sb.WriteString(text)
		}
	}
	if allText {
		return sb.String()
	}
	return parts
}

// convertToolChoice maps a Claude tool_choice to the OpenAI shape.
func convertToolChoice(choice any) any {
	if choice == nil {
		return "auto"
	}
	if s, ok := choice.(string); ok {
		return s
	}
	c, ok := choice.(map[string]any)
	if !ok {
		return "auto"
	}
	switch t, _ := c["type"].(string); t {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		name, _ := c["name"].(string)
		return map[string]any{"type": openaiBlockFunction, "function": map[string]any{"name": name}}
	default:
		return "auto"
	}
}

// fixMissingToolResponsesOpenAI inserts "[No response received]" tool replies
// for any assistant tool_call lacking an immediately-following tool response.
// It returns a new slice (which may be longer than the input) so the caller can
// reassign; a Go slice header is passed by value, so an in-place append cannot
// propagate growth back to the caller's variable.
func fixMissingToolResponsesOpenAI(messages []any) []any {
	out := append([]any{}, messages...)
	for i := 0; i < len(out); i++ {
		msg, ok := out[i].(map[string]any)
		if !ok {
			continue
		}
		if r, _ := msg["role"].(string); r != roleAssistant {
			continue
		}
		calls, ok := msg["tool_calls"].([]any)
		if !ok || len(calls) == 0 {
			continue
		}
		callIDs := make([]string, 0, len(calls))
		for _, c := range calls {
			call, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if id, ok := call["id"].(string); ok {
				callIDs = append(callIDs, id)
			}
		}
		responded := map[string]bool{}
		insertAt := i + 1
		for j := i + 1; j < len(out); j++ {
			next, ok := out[j].(map[string]any)
			if !ok {
				break
			}
			if r, _ := next["role"].(string); r != roleTool {
				break
			}
			if id, ok := next["tool_call_id"].(string); ok {
				responded[id] = true
			}
			insertAt = j + 1
		}
		var missing []string
		for _, id := range callIDs {
			if !responded[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 {
			continue
		}
		insert := make([]any, 0, len(missing))
		for _, id := range missing {
			insert = append(insert, map[string]any{
				"role":         roleTool,
				"tool_call_id": id,
				"content":      "[No response received]",
			})
		}
		// Splice into a freshly grown slice.
		grown := make([]any, 0, len(out)+len(insert))
		grown = append(grown, out[:insertAt]...)
		grown = append(grown, insert...)
		grown = append(grown, out[insertAt:]...)
		out = grown
		// Skip past the inserted replies so we don't reprocess them.
		i = insertAt + len(insert) - 1
	}
	return out
}

// encodeDataUri builds a data: URI from a media type and base64 data, matching
// concerns/image.js encodeDataUri.
func encodeDataUri(mediaType, data string) string {
	if mediaType == "" {
		mediaType = "image/png"
	}
	return "data:" + mediaType + ";base64," + data
}

// filterNonEmpty returns the non-empty subset of a string slice.
func filterNonEmpty(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
