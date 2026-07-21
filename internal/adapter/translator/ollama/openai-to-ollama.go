// Package ollama — openai→ollama request translator. Ports
// open-sse/translator/request/openai-to-ollama.js. Ollama's /api/chat expects
// messages[].content as a STRING (not the OpenAI content-array) and moves
// multimodal image blocks into message.images[] as raw base64. Without this
// translator the OpenAI request body is forwarded verbatim and ollama.com
// rejects content-arrays with HTTP 400:
//   "json: cannot unmarshal array into Go struct field ...content of type string"
package ollama

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

const (
	roleAssistant = "assistant"
	roleTool      = "tool"
	blockText     = "text"
	blockImageURL = "image_url"
)

func init() {
	translator.RegisterRequest(format.Openai, format.Ollama, openaiToOllamaTranslator{})
}

type openaiToOllamaTranslator struct{}

func (openaiToOllamaTranslator) TranslateRequest(model string, body json.RawMessage, stream bool, providerID string) (json.RawMessage, error) {
	return openaiToOllamaRequest(model, body, stream)
}

// openaiToOllamaRequest converts an OpenAI chat.completions request body into
// the Ollama /api/chat shape.
func openaiToOllamaRequest(model string, raw json.RawMessage, stream bool) (json.RawMessage, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("unmarshal openai body: %w", err)
	}

	result := map[string]any{
		"model":    model,
		"messages": normalizeOllamaMessages(body["messages"]),
		"stream":   stream,
	}

	options := map[string]any{}
	if v, ok := body["temperature"]; ok && v != nil {
		options["temperature"] = v
	}
	if v, ok := body["max_tokens"]; ok && v != nil {
		options["num_predict"] = v
	}
	if v, ok := body["top_p"]; ok && v != nil {
		options["top_p"] = v
	}
	if len(options) > 0 {
		result["options"] = options
	}

	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		result["tools"] = tools
	}
	if tc, ok := body["tool_choice"]; ok && tc != nil {
		result["tool_choice"] = tc
	}

	return json.Marshal(result)
}

// normalizeOllamaMessages converts the OpenAI messages array into Ollama's
// expected shape: content must be a string, tool messages map tool_call_id to
// tool_name, assistant tool_calls adopt the Ollama function shape, and
// multimodal image_url blocks move to message.images[] (raw base64).
func normalizeOllamaMessages(messages any) []map[string]any {
	rawMsgs, ok := messages.([]any)
	if !ok {
		return nil
	}

	// First pass: build tool_call_id -> tool_name map from assistant messages.
	toolCallMap := map[string]string{}
	for _, m := range rawMsgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] != roleAssistant {
			continue
		}
		toolCalls, ok := msg["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, tc := range toolCalls {
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			id, _ := tcm["id"].(string)
			fn, _ := tcm["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if id != "" && name != "" {
				toolCallMap[id] = name
			}
		}
	}

	out := make([]map[string]any, 0, len(rawMsgs))
	for _, m := range rawMsgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)

		// Tool result messages: OpenAI {role:"tool", content, tool_call_id}
		// -> Ollama {role:"tool", tool_name, content}.
		if role == roleTool {
			content := normalizeOllamaContent(msg["content"])
			if content == "" {
				continue
			}
			toolName := ""
			if id, _ := msg["tool_call_id"].(string); id != "" {
				toolName = toolCallMap[id]
			}
			if toolName == "" {
				toolName, _ = msg["name"].(string)
			}
			if toolName == "" {
				toolName = "unknown_tool"
			}
			out = append(out, map[string]any{
				"role":      roleTool,
				"tool_name": toolName,
				"content":   content,
			})
			continue
		}

		// Assistant messages with tool_calls.
		if role == roleAssistant {
			if toolCalls, ok := msg["tool_calls"].([]any); ok && len(toolCalls) > 0 {
				content := normalizeOllamaContent(msg["content"])
				ollamaToolCalls := make([]map[string]any, 0, len(toolCalls))
				for _, tc := range toolCalls {
					tcm, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := tcm["function"].(map[string]any)
					if fn == nil {
						fn = map[string]any{}
					}
					name, _ := fn["name"].(string)
					args := fn["arguments"]
					var argsVal any
					switch a := args.(type) {
					case string:
						argsVal = safeParseJSONObject(a)
					default:
						argsVal = a
					}
					idx := 0
					if v, ok := tcm["index"].(float64); ok {
						idx = int(v)
					}
					ollamaToolCalls = append(ollamaToolCalls, map[string]any{
						"type": "function",
						"function": map[string]any{
							"index":     idx,
							"name":      name,
							"arguments": argsVal,
						},
					})
				}
				out = append(out, map[string]any{
					"role":       roleAssistant,
					"content":    content,
					"tool_calls": ollamaToolCalls,
				})
				continue
			}
		}

		// Normal messages.
		content := normalizeOllamaContent(msg["content"])
		images := extractOllamaImages(msg["content"])
		if content == "" && role != roleAssistant {
			continue
		}
		entry := map[string]any{
			"role":    role,
			"content": content,
		}
		if len(images) > 0 {
			entry["images"] = images
		}
		out = append(out, entry)
	}
	return out
}

// normalizeOllamaContent collapses an OpenAI content value (string OR array of
// content blocks) into the single string Ollama requires. Only text blocks
// contribute; image blocks are handled separately via extractOllamaImages.
func normalizeOllamaContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, b := range v {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] != blockText {
				continue
			}
			if t, ok := block["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	case nil:
		return ""
	}
	return ""
}

// extractOllamaImages pulls base64 image data out of OpenAI image_url content
// blocks. Ollama wants message.images[] as raw base64 (no data: prefix).
func extractOllamaImages(content any) []string {
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	var images []string
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] != blockImageURL {
			continue
		}
		var url string
		switch u := block["image_url"].(type) {
		case string:
			url = u
		case map[string]any:
			url, _ = u["url"].(string)
		}
		if url == "" {
			continue
		}
		if b64 := dataURIToBase64(url); b64 != "" {
			images = append(images, b64)
		}
	}
	return images
}

// dataURIToBase64 strips a "data:<mime>;base64," prefix and returns the raw
// base64 payload. Returns "" if the input is not a data: URI.
func dataURIToBase64(url string) string {
	if !strings.HasPrefix(url, "data:") {
		return ""
	}
	// data:image/png;base64,XXXX
	if idx := strings.Index(url, ";base64,"); idx >= 0 {
		return url[idx+len(";base64,"):]
	}
	if idx := strings.Index(url, ","); idx >= 0 {
		return url[idx+1:]
	}
	return ""
}

// safeParseJSONObject parses a JSON object string, returning the parsed map
// or an empty map on failure (Ollama prefers an object over a string for tool
// arguments).
func safeParseJSONObject(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return v
	}
	return map[string]any{}
}