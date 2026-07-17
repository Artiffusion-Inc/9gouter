// Package openai implements the OpenAI base format helpers and the OpenAI→Claude
// request translator. It registers itself on the translator registry at init
// time, mirroring open-sse/translator/request/openai-to-claude.js.
package openai

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Artiffusion-Inc/9router/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9router/internal/domain/format"
)

// Default token limits matching open-sse/config/runtimeConfig.js.
const (
	defaultMaxTokens = 64000
	defaultMinTokens = 32000
)

// Block and role constants matching open-sse/translator/schema.
const (
	openaiBlockText      = "text"
	openaiBlockImageURL  = "image_url"
	openaiBlockImage     = "image"
	openaiBlockFunction  = "function"
	claudeBlockText      = "text"
	claudeBlockImage     = "image"
	claudeBlockDocument  = "document"
	claudeBlockToolUse   = "tool_use"
	claudeBlockToolResult = "tool_result"
	claudeBlockThinking  = "thinking"
	roleUser             = "user"
	roleAssistant        = "assistant"
	roleTool             = "tool"
	roleSystem           = "system"
)

var claudeSystemPrompt = "You are Claude Code, Anthropic's official CLI for Claude."

func init() {
	translator.Register(format.Openai, format.Claude, openaiToClaudeTranslator{})
}

type openaiToClaudeTranslator struct{}

func (openaiToClaudeTranslator) TranslateRequest(model string, body json.RawMessage, stream bool, providerID string) (json.RawMessage, error) {
	return openaiToClaudeRequest(model, body, stream)
}

// openaiToClaudeRequest ports openaiToClaudeRequest from
// open-sse/translator/request/openai-to-claude.js.
func openaiToClaudeRequest(model string, raw json.RawMessage, stream bool) (json.RawMessage, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("unmarshal body: %w", err)
	}

	result := map[string]any{
		"model":      model,
		"max_tokens": adjustMaxTokens(body),
		"stream":     stream,
	}

	if temp, ok := body["temperature"]; ok {
		result["temperature"] = temp
	}

	var messages []map[string]any
	if rawMsgs, ok := body["messages"].([]any); ok {
		for _, m := range rawMsgs {
			if msg, ok := m.(map[string]any); ok {
				messages = append(messages, msg)
			}
		}
	}

	systemParts := []string{}
	nonSystemMessages := make([]map[string]any, 0, len(messages))

	for _, msg := range messages {
		if role, _ := msg["role"].(string); role == roleSystem {
			systemParts = append(systemParts, extractTextContent(msg["content"]))
		} else {
			nonSystemMessages = append(nonSystemMessages, msg)
		}
	}

	resultMessages := []map[string]any{}
	var currentRole string
	currentParts := []map[string]any{}

	flushCurrentMessage := func() {
		if currentRole != "" && len(currentParts) > 0 {
			resultMessages = append(resultMessages, map[string]any{"role": currentRole, "content": currentParts})
			currentParts = nil
		}
	}

	for _, msg := range nonSystemMessages {
		role, _ := msg["role"].(string)
		newRole := role
		if role == roleUser || role == roleTool {
			newRole = roleUser
		} else {
			newRole = roleAssistant
		}

		blocks := getContentBlocksFromMessage(msg)
		hasToolUse := false
		hasToolResult := false
		for _, b := range blocks {
			typ, _ := b["type"].(string)
			if typ == claudeBlockToolUse {
				hasToolUse = true
			}
			if typ == claudeBlockToolResult {
				hasToolResult = true
			}
		}

		if hasToolResult {
			toolResultBlocks := []map[string]any{}
			otherBlocks := []map[string]any{}
			for _, b := range blocks {
				typ, _ := b["type"].(string)
				if typ == claudeBlockToolResult {
					toolResultBlocks = append(toolResultBlocks, b)
				} else {
					otherBlocks = append(otherBlocks, b)
				}
			}
			flushCurrentMessage()
			if len(toolResultBlocks) > 0 {
				resultMessages = append(resultMessages, map[string]any{"role": roleUser, "content": toolResultBlocks})
			}
			if len(otherBlocks) > 0 {
				currentRole = newRole
				currentParts = append(currentParts, otherBlocks...)
			}
			continue
		}

		if currentRole != newRole {
			flushCurrentMessage()
			currentRole = newRole
		}

		currentParts = append(currentParts, blocks...)
		if hasToolUse {
			flushCurrentMessage()
		}
	}
	flushCurrentMessage()

	// Add cache_control to last assistant message.
	for i := len(resultMessages) - 1; i >= 0; i-- {
		msg := resultMessages[i]
		if role, _ := msg["role"].(string); role == roleAssistant {
			if content, ok := msg["content"].([]map[string]any); ok && len(content) > 0 {
				validBlockTypes := map[string]struct{}{claudeBlockText: {}, claudeBlockToolUse: {}, claudeBlockToolResult: {}, claudeBlockImage: {}}
				for j := len(content) - 1; j >= 0; j-- {
					typ, _ := content[j]["type"].(string)
					if _, ok := validBlockTypes[typ]; ok {
						content[j]["cache_control"] = map[string]any{"type": "ephemeral"}
						break
					}
				}
			}
			break
		}
	}

	result["messages"] = resultMessages

	// Handle response_format for JSON mode.
	if rf, ok := body["response_format"].(map[string]any); ok {
		if typ, _ := rf["type"].(string); typ == "json_schema" {
			if js, ok := rf["json_schema"].(map[string]any); ok {
				if schema, ok := js["schema"].(map[string]any); ok {
					schemaJSON, _ := json.MarshalIndent(schema, "", "  ")
					systemParts = append(systemParts, fmt.Sprintf("You must respond with valid JSON that strictly follows this JSON schema:\n```json\n%s\n```\nRespond ONLY with the JSON object, no other text.", string(schemaJSON)))
				}
			}
		} else if typ == "json_object" {
			systemParts = append(systemParts, "You must respond with valid JSON. Respond ONLY with a JSON object, no other text.")
		}
	}

	claudeCodePrompt := map[string]any{"type": claudeBlockText, "text": claudeSystemPrompt}
	if len(systemParts) > 0 {
		systemText := strings.Join(systemParts, "\n")
		result["system"] = []map[string]any{
			claudeCodePrompt,
			{"type": claudeBlockText, "text": systemText, "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}},
		}
	} else {
		result["system"] = []map[string]any{claudeCodePrompt}
	}

	// Tools.
	if rawTools, ok := body["tools"].([]any); ok {
		tools := []map[string]any{}
		for _, t := range rawTools {
			tool, ok := t.(map[string]any)
			if !ok {
				continue
			}
			toolType, _ := tool["type"].(string)
			if toolType != "" && toolType != openaiBlockFunction {
				// pass-through built-in tools
				tools = append(tools, tool)
				continue
			}
			toolData := tool
			if fn, ok := tool["function"].(map[string]any); ok {
				toolData = fn
			}
			name, _ := toolData["name"].(string)
			desc, _ := toolData["description"].(string)
			params := toolData["parameters"]
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}
			}
			tools = append(tools, map[string]any{
				"name":        name,
				"description": desc,
				"input_schema": params,
			})
		}
		if len(tools) > 0 {
			tools[len(tools)-1]["cache_control"] = map[string]any{"type": "ephemeral", "ttl": "1h"}
			result["tools"] = tools
		}
	}

	// Tool choice.
	if tc, ok := body["tool_choice"]; ok {
		result["tool_choice"] = convertOpenAIToolChoice(tc)
	}

	return json.Marshal(result)
}

func extractTextContent(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				if typ, _ := m["type"].(string); typ == openaiBlockText {
					if t, ok := m["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

func getContentBlocksFromMessage(msg map[string]any) []map[string]any {
	blocks := []map[string]any{}
	role, _ := msg["role"].(string)

	switch role {
	case roleTool:
		blocks = append(blocks, map[string]any{
			"type":        claudeBlockToolResult,
			"tool_use_id": msg["tool_call_id"],
			"content":     msg["content"],
		})
	case roleUser:
		content := msg["content"]
		switch c := content.(type) {
		case string:
			if c != "" {
				blocks = append(blocks, map[string]any{"type": claudeBlockText, "text": c})
			}
		case []any:
			for _, item := range c {
				part, ok := item.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := part["type"].(string)
				switch typ {
				case openaiBlockText:
					if t, ok := part["text"].(string); ok && t != "" {
						blocks = append(blocks, map[string]any{"type": claudeBlockText, "text": t})
					}
				case "tool_result":
					tr := map[string]any{
						"type":        claudeBlockToolResult,
						"tool_use_id": part["tool_use_id"],
						"content":     part["content"],
					}
					if isErr, ok := part["is_error"].(bool); ok {
						tr["is_error"] = isErr
					}
					blocks = append(blocks, tr)
				case openaiBlockImageURL:
					if iu, ok := part["image_url"].(map[string]any); ok {
						url, _ := iu["url"].(string)
						if parsed := parseDataURI(url); parsed != nil {
							blocks = append(blocks, map[string]any{
								"type": claudeBlockImage,
								"source": map[string]any{
									"type":       "base64",
									"media_type": parsed.mimeType,
									"data":       parsed.base64,
								},
							})
						} else if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
							blocks = append(blocks, map[string]any{
								"type": claudeBlockImage,
								"source": map[string]any{
									"type": "url",
									"url":  url,
								},
							})
						}
					}
				case openaiBlockImage:
					if src, ok := part["source"].(map[string]any); ok {
						blocks = append(blocks, map[string]any{"type": claudeBlockImage, "source": src})
					}
				case "file":
					if f, ok := part["file"].(map[string]any); ok {
						if fd, ok := f["file_data"].(string); ok {
							if parsed := parseDataURI(fd); parsed != nil && parsed.mimeType == "application/pdf" {
								blocks = append(blocks, map[string]any{
									"type": claudeBlockDocument,
									"source": map[string]any{
										"type":       "base64",
										"media_type": parsed.mimeType,
										"data":       parsed.base64,
									},
								})
							}
						}
					}
				}
			}
		}
	case roleAssistant:
		content := msg["content"]
		switch c := content.(type) {
		case []any:
			for _, item := range c {
				part, ok := item.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := part["type"].(string)
				switch typ {
				case openaiBlockText:
					if t, ok := part["text"].(string); ok && t != "" {
						blocks = append(blocks, map[string]any{"type": claudeBlockText, "text": t})
					}
				case claudeBlockToolUse:
					blocks = append(blocks, map[string]any{
						"type":  claudeBlockToolUse,
						"id":    part["id"],
						"name":  part["name"],
						"input": part["input"],
					})
				case claudeBlockThinking:
					thinking := map[string]any{"type": claudeBlockThinking, "thinking": part["thinking"]}
					if sig, ok := part["signature"].(string); ok {
						thinking["signature"] = sig
					}
					blocks = append(blocks, thinking)
				}
			}
		case string:
			if c != "" {
				blocks = append(blocks, map[string]any{"type": claudeBlockText, "text": c})
			}
		default:
			if text := extractTextContent(c); text != "" {
				blocks = append(blocks, map[string]any{"type": claudeBlockText, "text": text})
			}
		}

		if rawTCs, ok := msg["tool_calls"].([]any); ok {
			for _, raw := range rawTCs {
				tc, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if typ, _ := tc["type"].(string); typ != openaiBlockFunction {
					continue
				}
				fn, ok := tc["function"].(map[string]any)
				if !ok {
					continue
				}
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				var input any
				if err := json.Unmarshal([]byte(args), &input); err != nil {
					input = args
				}
				blocks = append(blocks, map[string]any{
					"type":  claudeBlockToolUse,
					"id":    tc["id"],
					"name":  name,
					"input": input,
				})
			}
		}

		if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
			blocks = append([]map[string]any{{"type": claudeBlockThinking, "thinking": rc}}, blocks...)
		}
	}

	return blocks
}

func convertOpenAIToolChoice(choice any) map[string]any {
	switch c := choice.(type) {
	case string:
		if c == "required" {
			return map[string]any{"type": "any"}
		}
		return map[string]any{"type": "auto"}
	case map[string]any:
		if fn, ok := c["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
		if typ, _ := c["type"].(string); typ == "auto" || typ == "any" || typ == "tool" || typ == "none" {
			return c
		}
	}
	return map[string]any{"type": "auto"}
}

type dataURI struct {
	mimeType string
	base64   string
}

var dataURIRe = regexp.MustCompile(`^data:([^;]+);base64,(.+)$`)

func parseDataURI(url string) *dataURI {
	m := dataURIRe.FindStringSubmatch(url)
	if m == nil {
		return nil
	}
	return &dataURI{mimeType: m[1], base64: m[2]}
}

func adjustMaxTokens(body map[string]any) int {
	maxTokens := defaultMaxTokens
	if mt, ok := body["max_tokens"].(float64); ok {
		maxTokens = int(mt)
	} else if mt, ok := body["max_tokens"].(json.Number); ok {
		if n, err := mt.Int64(); err == nil {
			maxTokens = int(n)
		}
	}

	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		if maxTokens < defaultMinTokens {
			maxTokens = defaultMinTokens
		}
	}

	if thinking, ok := body["thinking"].(map[string]any); ok {
		if budget, ok := thinking["budget_tokens"].(float64); ok {
			if maxTokens <= int(budget) {
				maxTokens = int(budget) + 1024
			}
		}
	}

	if maxTokens > defaultMaxTokens {
		maxTokens = defaultMaxTokens
	}
	return maxTokens
}
