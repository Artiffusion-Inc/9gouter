// Package openai implements the OpenAI base format helpers and the OpenAI→Claude
// request translator. It registers itself on the translator registry at init
// time, mirroring open-sse/translator/request/openai-to-claude.js.
package openai

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/shared"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

// Default token limits matching open-sse/config/runtimeConfig.js.
const (
	defaultMaxTokens = 64000
	defaultMinTokens = 32000
)

// Block and role constants matching open-sse/translator/schema.
const (
	openaiBlockText                 = "text"
	openaiBlockImageURL             = "image_url"
	openaiBlockImage                = "image"
	openaiBlockFunction             = "function"
	claudeBlockText                 = "text"
	claudeBlockImage                = "image"
	claudeBlockDocument             = "document"
	claudeBlockToolUse              = "tool_use"
	claudeBlockToolResult           = "tool_result"
	claudeBlockThinking             = "thinking"
	roleUser                        = "user"
	roleAssistant                   = "assistant"
	roleTool                        = "tool"
	roleSystem                      = "system"
	roleDeveloper                   = "developer"
	responsesItemMessage            = "message"
	responsesItemInputText          = "input_text"
	responsesItemOutputText         = "output_text"
	responsesItemInputImage         = "input_image"
	responsesItemFunctionCall       = "function_call"
	responsesItemFunctionCallOutput = "function_call_output"
	responsesItemReasoning          = "reasoning"
	responsesItemSummaryText        = "summary_text"
)

var claudeSystemPrompt = "You are Claude Code, Anthropic's official CLI for Claude."

func init() {
	translator.Register(format.Openai, format.Claude, openaiToClaudeTranslator{})
	translator.Register(format.Openai, format.OpenaiResponses, openaiToOpenaiResponsesTranslator{})
	translator.Register(format.OpenaiResponses, format.Openai, openaiResponsesToOpenaiTranslator{})
	translator.RegisterResponse(format.OpenaiResponses, format.Openai, openaiResponsesToOpenaiResponseTranslator{})
}

type openaiToClaudeTranslator struct{}

func (openaiToClaudeTranslator) TranslateRequest(model string, body json.RawMessage, stream bool, providerID string) (json.RawMessage, error) {
	return openaiToClaudeRequest(model, body, stream)
}

type openaiToOpenaiResponsesTranslator struct{}

func (openaiToOpenaiResponsesTranslator) TranslateRequest(model string, body json.RawMessage, stream bool, providerID string) (json.RawMessage, error) {
	return openaiToOpenaiResponsesRequest(model, body, stream)
}

type openaiResponsesToOpenaiTranslator struct{}

func (openaiResponsesToOpenaiTranslator) TranslateRequest(model string, body json.RawMessage, stream bool, providerID string) (json.RawMessage, error) {
	return openaiResponsesToOpenaiRequest(model, body, stream)
}

type openaiResponsesToOpenaiResponseTranslator struct{}

func (openaiResponsesToOpenaiResponseTranslator) TranslateResponse(chunk json.RawMessage, state map[string]any) ([]json.RawMessage, error) {
	var body map[string]any
	if err := json.Unmarshal(chunk, &body); err != nil {
		return nil, fmt.Errorf("unmarshal openai-responses chunk: %w", err)
	}
	out := openaiResponsesToOpenAIResponse(body, state)
	if out == nil {
		return nil, nil
	}
	results := make([]json.RawMessage, 0, len(out))
	for _, c := range out {
		b, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		results = append(results, b)
	}
	return results, nil
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

	// Port reasoning_effort handling for Claude adaptive-thinking models (4.6+).
	if supportsClaudeAdaptiveThinking(model) {
		if re, ok := body["reasoning_effort"].(string); ok && re != "" {
			level := normalizeClaudeEffort(re)
			result["thinking"] = map[string]any{"type": "adaptive"}
			result["output_config"] = map[string]any{"effort": level}
		}
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
	systemBlocks := []map[string]any{claudeCodePrompt}
	if len(systemParts) > 0 {
		systemText := strings.Join(systemParts, "\n")
		systemBlocks = append(systemBlocks, map[string]any{"type": claudeBlockText, "text": systemText})
	}
	// Add cache_control to the last system block to match JS formats/claude.js post-processing.
	last := systemBlocks[len(systemBlocks)-1]
	last["cache_control"] = map[string]any{"type": "ephemeral", "ttl": "1h"}
	result["system"] = systemBlocks

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

func normalizeClaudeEffort(re string) string {
	switch strings.ToLower(re) {
	case "xhigh", "max":
		return "high"
	case "low", "medium", "high":
		return strings.ToLower(re)
	default:
		return "medium"
	}
}

// supportsClaudeAdaptiveThinking mirrors the JS allowlist for models that accept
// thinking.type "adaptive" + output_config.effort.
func supportsClaudeAdaptiveThinking(model string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(model, "-", "."))
	if !strings.Contains(normalized, "claude") {
		return false
	}
	re := regexp.MustCompile(`(?:^|[/.])claude(?:[/.][a-z]+)*[/.](\d+)(?:[/.](\d+))?(?:[/.]|$)`)
	m := re.FindStringSubmatch(normalized)
	if m == nil {
		return false
	}
	major := 0
	fmt.Sscanf(m[1], "%d", &major)
	minor := -1
	if m[2] != "" {
		fmt.Sscanf(m[2], "%d", &minor)
	}
	if major < 4 {
		return false
	}
	if major == 4 && (minor == -1 || minor <= 5) {
		return false
	}
	return true
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
const maxCallIDLen = 64

func clampCallID(id any) any {
	s, ok := id.(string)
	if !ok || len(s) <= maxCallIDLen {
		return id
	}
	return s[:maxCallIDLen]
}

func openaiResponsesToOpenaiRequest(model string, raw json.RawMessage, stream bool) (json.RawMessage, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("unmarshal body: %w", err)
	}
	if _, ok := body["input"]; !ok {
		return raw, nil
	}

	result := shallowCopyMap(body)
	result["messages"] = []any{}

	if instructions, ok := body["instructions"].(string); ok && instructions != "" {
		result["messages"] = append(result["messages"].([]any), map[string]any{"role": roleSystem, "content": instructions})
	}

	inputItems := normalizeResponsesInput(body["input"])
	if inputItems == nil {
		return raw, nil
	}

	var currentAssistantMsg map[string]any
	var pendingReasoning string
	var pendingReasoningEncrypted string

	attachPendingReasoning := func(msg map[string]any) {
		if pendingReasoning != "" {
			msg["reasoning_content"] = pendingReasoning
		}
		if pendingReasoningEncrypted != "" {
			msg["encrypted_content"] = pendingReasoningEncrypted
		}
		pendingReasoning = ""
		pendingReasoningEncrypted = ""
	}

	flushCurrentAssistant := func() {
		if currentAssistantMsg != nil {
			result["messages"] = append(result["messages"].([]any), currentAssistantMsg)
			currentAssistantMsg = nil
		}
	}

	for _, itemAny := range inputItems {
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := item["type"].(string)
		if itemType == "" && item["role"] != nil {
			itemType = responsesItemMessage
		}
		switch itemType {
		case responsesItemMessage:
			flushCurrentAssistant()
			role, _ := item["role"].(string)
			content := normalizeResponsesContent(item["content"])
			msg := map[string]any{"role": role, "content": content}
			if role == roleAssistant {
				attachPendingReasoning(msg)
			} else {
				pendingReasoning = ""
				pendingReasoningEncrypted = ""
			}
			result["messages"] = append(result["messages"].([]any), msg)
		case responsesItemFunctionCall:
			if currentAssistantMsg == nil {
				currentAssistantMsg = map[string]any{
					"role":       roleAssistant,
					"content":    nil,
					"tool_calls": []any{},
				}
				attachPendingReasoning(currentAssistantMsg)
			}
			name, _ := item["name"].(string)
			if name = strings.TrimSpace(name); name == "" {
				continue
			}
			currentAssistantMsg["tool_calls"] = append(currentAssistantMsg["tool_calls"].([]any), map[string]any{
				"id":   clampCallID(item["call_id"]),
				"type": openaiBlockFunction,
				"function": map[string]any{
					"name":      name,
					"arguments": item["arguments"],
				},
			})
		case responsesItemFunctionCallOutput:
			flushCurrentAssistant()
			output := item["output"]
			if _, ok := output.(string); !ok {
				output = marshalJSONString(output)
			}
			result["messages"] = append(result["messages"].([]any), map[string]any{
				"role":         roleTool,
				"tool_call_id": clampCallID(item["call_id"]),
				"content":      output,
			})
		case responsesItemReasoning:
			txt := extractReasoningItemText(item)
			if txt != "" {
				if pendingReasoning != "" {
					pendingReasoning += "\n" + txt
				} else {
					pendingReasoning = txt
				}
			}
			if enc, ok := item["encrypted_content"].(string); ok && enc != "" {
				pendingReasoningEncrypted = enc
			}
		}
	}
	flushCurrentAssistant()

	if rawTools, ok := body["tools"].([]any); ok {
		var tools []any
		for _, t := range rawTools {
			tool, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if _, ok := tool["function"]; ok {
				tools = append(tools, tool)
				continue
			}
			name, _ := tool["name"].(string)
			if name = strings.TrimSpace(name); name == "" {
				continue
			}
			tools = append(tools, map[string]any{
				"type": openaiBlockFunction,
				"function": map[string]any{
					"name":        name,
					"description": toString(tool["description"]),
					"parameters":  normalizeToolParameters(tool["parameters"]),
					"strict":      tool["strict"],
				},
			})
		}
		if len(tools) > 0 {
			result["tools"] = tools
		}
	}

	if mot, ok := result["max_output_tokens"]; ok {
		if _, ok := result["max_tokens"]; !ok {
			result["max_tokens"] = mot
		}
		delete(result, "max_output_tokens")
	}
	for _, k := range []string{"input", "instructions", "include", "prompt_cache_key", "store", "reasoning", "client_metadata"} {
		delete(result, k)
	}
	return json.Marshal(result)
}

func openaiToOpenaiResponsesRequest(model string, raw json.RawMessage, stream bool) (json.RawMessage, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("unmarshal body: %w", err)
	}
	if _, ok := body["input"]; ok {
		out := shallowCopyMap(body)
		out["model"] = model
		out["stream"] = true
		return json.Marshal(out)
	}

	result := map[string]any{
		"model":  model,
		"input":  []any{},
		"stream": true,
		"store":  false,
	}

	hasSystem := false
	var messages []map[string]any
	if rawMsgs, ok := body["messages"].([]any); ok {
		for _, m := range rawMsgs {
			if msg, ok := m.(map[string]any); ok {
				messages = append(messages, msg)
			}
		}
	}

	for _, msg := range messages {
		role, _ := msg["role"].(string)
		if role == roleSystem || role == roleDeveloper {
			if !hasSystem {
				content := ""
				if c, ok := msg["content"].(string); ok {
					content = c
				}
				result["instructions"] = content
				hasSystem = true
			}
			continue
		}
		if role == roleTool {
			output := ""
			switch c := msg["content"].(type) {
			case string:
				output = c
			case []any:
				var parts []string
				for _, part := range c {
					if m, ok := part.(map[string]any); ok {
						if t, ok := m["text"].(string); ok {
							parts = append(parts, t)
						} else {
							parts = append(parts, marshalJSONString(m))
						}
					} else {
						parts = append(parts, marshalJSONString(part))
					}
				}
				output = strings.Join(parts, "")
			default:
				output = marshalJSONString(c)
			}
			result["input"] = append(result["input"].([]any), map[string]any{
				"type":      responsesItemFunctionCallOutput,
				"call_id":   clampCallID(msg["tool_call_id"]),
				"output":    output,
			})
			continue
		}
		if role != roleUser && role != roleAssistant {
			continue
		}

		if role == roleAssistant {
			if ri := buildReasoningInputItem(msg); ri != nil {
				result["input"] = append(result["input"].([]any), ri)
			}
		}

		contentType := responsesItemInputText
		if role == roleAssistant {
			contentType = responsesItemOutputText
		}
		content := convertChatMessageContentToResponses(msg["content"], contentType)
		if len(content) > 0 {
			result["input"] = append(result["input"].([]any), map[string]any{
				"type":    responsesItemMessage,
				"role":    role,
				"content": content,
			})
		}

		if role == roleAssistant {
			if rawTCs, ok := msg["tool_calls"].([]any); ok {
				for _, rawTC := range rawTCs {
					tc, ok := rawTC.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := tc["function"].(map[string]any)
					fnName, _ := fn["name"].(string)
					if fnName == "" {
						fnName = "_unknown"
					}
					args, _ := fn["arguments"].(string)
					if args == "" {
						args = "{}"
					}
					result["input"] = append(result["input"].([]any), map[string]any{
						"type":      responsesItemFunctionCall,
						"call_id":   clampCallID(tc["id"]),
						"name":      fnName,
						"arguments": args,
					})
				}
			}
		}
	}

	if !hasSystem {
		result["instructions"] = ""
	}

	if rawTools, ok := body["tools"].([]any); ok {
		var tools []any
		for _, t := range rawTools {
			tool, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := tool["type"].(string); typ == openaiBlockFunction {
				fn, _ := tool["function"].(map[string]any)
				name, _ := fn["name"].(string)
				tools = append(tools, map[string]any{
					"type":        openaiBlockFunction,
					"name":        name,
					"description": toString(fn["description"]),
					"parameters":  normalizeToolParameters(fn["parameters"]),
					"strict":      fn["strict"],
				})
			} else {
				tools = append(tools, tool)
			}
		}
		if len(tools) > 0 {
			result["tools"] = tools
		}
	}

	for _, k := range []string{"temperature", "max_tokens", "top_p", "reasoning"} {
		if v, ok := body[k]; ok {
			result[k] = v
		}
	}
	if v, ok := body["reasoning_effort"]; ok {
		result["reasoning"] = map[string]any{"effort": v, "summary": "auto"}
	}

	return json.Marshal(result)
}

func normalizeResponsesInput(input any) []any {
	switch v := input.(type) {
	case string:
		text := v
		if strings.TrimSpace(text) == "" {
			text = "..."
		}
		return []any{map[string]any{
			"type":    responsesItemMessage,
			"role":    roleUser,
			"content": []any{map[string]any{"type": responsesItemInputText, "text": text}},
		}}
	case []any:
		if len(v) == 0 {
			return []any{map[string]any{
				"type":    responsesItemMessage,
				"role":    roleUser,
				"content": []any{map[string]any{"type": responsesItemInputText, "text": "..."}},
			}}
		}
		return v
	}
	return nil
}

func normalizeResponsesContent(content any) any {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var out []any
		for _, itemAny := range c {
			item, ok := itemAny.(map[string]any)
			if !ok {
				out = append(out, itemAny)
				continue
			}
			typ, _ := item["type"].(string)
			switch typ {
			case responsesItemInputText, responsesItemOutputText:
				if text, ok := item["text"].(string); ok {
					out = append(out, map[string]any{"type": openaiBlockText, "text": text})
				}
			case responsesItemInputImage:
				url := ""
				if u, ok := item["image_url"].(string); ok {
					url = u
				} else if u, ok := item["file_id"].(string); ok {
					url = u
				}
				out = append(out, map[string]any{
					"type": openaiBlockImageURL,
					"image_url": map[string]any{
						"url":    url,
						"detail": item["detail"],
					},
				})
			default:
				out = append(out, item)
			}
		}
		return out
	}
	return content
}

func extractReasoningItemText(item map[string]any) string {
	if summary, ok := item["summary"].([]any); ok {
		var parts []string
		for _, sAny := range summary {
			s, ok := sAny.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := s["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	if content, ok := item["content"].([]any); ok {
		var parts []string
		for _, cAny := range content {
			c, ok := cAny.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := c["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

func buildReasoningInputItem(msg map[string]any) map[string]any {
	if msg == nil {
		return nil
	}
	var encrypted string
	if s, ok := msg["encrypted_content"].(string); ok && s != "" {
		encrypted = s
	} else if s, ok := msg["reasoning_encrypted_content"].(string); ok && s != "" {
		encrypted = s
	} else if r, ok := msg["reasoning"].(map[string]any); ok {
		if s, ok := r["encrypted_content"].(string); ok && s != "" {
			encrypted = s
		}
	}
	var summaryText string
	if s, ok := msg["reasoning_content"].(string); ok && strings.TrimSpace(s) != "" {
		summaryText = s
	} else if s, ok := msg["reasoning"].(string); ok && strings.TrimSpace(s) != "" {
		summaryText = s
	} else if details, ok := msg["reasoning_details"].([]any); ok {
		var parts []string
		for _, dAny := range details {
			d, ok := dAny.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := d["text"].(string); ok && t != "" {
				parts = append(parts, t)
			} else if t, ok := d["content"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
		summaryText = strings.Join(parts, "\n")
	}
	if encrypted == "" && summaryText == "" {
		return nil
	}
	item := map[string]any{"type": responsesItemReasoning}
	if summaryText != "" {
		item["summary"] = []any{map[string]any{"type": responsesItemSummaryText, "text": summaryText}}
	}
	if encrypted != "" {
		item["encrypted_content"] = encrypted
	}
	return item
}

func convertChatMessageContentToResponses(content any, contentType string) []any {
	switch c := content.(type) {
	case string:
		if c == "" {
			return nil
		}
		return []any{map[string]any{"type": contentType, "text": c}}
	case []any:
		var out []any
		for _, itemAny := range c {
			item, ok := itemAny.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := item["type"].(string)
			switch typ {
			case openaiBlockText:
				if t, ok := item["text"].(string); ok {
					out = append(out, map[string]any{"type": contentType, "text": t})
				}
			case openaiBlockImageURL:
				url := ""
				if u, ok := item["image_url"].(string); ok {
					url = u
				} else if iu, ok := item["image_url"].(map[string]any); ok {
					url, _ = iu["url"].(string)
				}
				out = append(out, map[string]any{
					"type":      responsesItemInputImage,
					"image_url": url,
					"detail":    item["detail"],
				})
			case responsesItemInputImage:
				out = append(out, item)
			default:
				text := ""
				if t, ok := item["text"].(string); ok && t != "" {
					text = t
				} else if t, ok := item["content"].(string); ok && t != "" {
					text = t
				} else {
					text = marshalJSONString(item)
				}
				out = append(out, map[string]any{"type": contentType, "text": text})
			}
		}
		return out
	}
	return nil
}

func normalizeToolParameters(params any) any {
	if params == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	m, ok := params.(map[string]any)
	if !ok {
		return params
	}
	if typ, _ := m["type"].(string); typ == "object" {
		if _, ok := m["properties"]; !ok {
			cp := shallowCopyMap(m)
			cp["properties"] = map[string]any{}
			return cp
		}
	}
	return params
}

func shallowCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return marshalJSONString(v)
}

func marshalJSONString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// openaiResponsesToOpenAIResponse ports the OpenAI Responses API → OpenAI Chat
// Completions response translator from open-sse/translator/response/openai-responses.js.
func openaiResponsesToOpenAIResponse(chunk map[string]any, state map[string]any) []map[string]any {
	// Flush on nil chunk.
	if chunk == nil {
		if state["finishReasonSent"] == true || state["started"] != true {
			return nil
		}
		finishReason := "stop"
		if state["toolCallIndex"] != nil {
			if n, ok := state["toolCallIndex"].(int); ok && n > 0 {
				finishReason = "tool_calls"
			}
		}
		if state["currentToolCallId"] != nil {
			if s, ok := state["currentToolCallId"].(string); ok && s != "" {
				finishReason = "tool_calls"
			}
		}
		state["finishReasonSent"] = true
		state["finishReason"] = finishReason
		final := buildResponsesOpenAIChunk(state, map[string]any{}, finishReason)
		if u, ok := state["usage"].(map[string]any); ok {
			final["usage"] = u
		}
		return []map[string]any{final}
	}

	eventType := ""
	if t, ok := chunk["type"].(string); ok {
		eventType = t
	}
	if eventType == "" {
		if e, ok := chunk["event"].(string); ok {
			eventType = e
		}
	}
	data := chunk
	if d, ok := chunk["data"].(map[string]any); ok {
		data = d
	}

	// Initialize state.
	if state["started"] != true {
		state["started"] = true
		state["chatId"] = fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
		state["created"] = 0
		state["toolCallIndex"] = 0
		state["currentToolCallId"] = nil
	}

	switch eventType {
	case "response.output_text.delta":
		deltaText := ""
		if d, ok := data["delta"].(string); ok {
			deltaText = d
		}
		if deltaText == "" {
			return nil
		}
		return []map[string]any{buildResponsesOpenAIChunk(state, map[string]any{"content": deltaText}, nil)}

	case "response.output_text.done":
		return nil

	case "response.output_item.added":
		item, _ := data["item"].(map[string]any)
		if item == nil {
			return nil
		}
		itemType, _ := item["type"].(string)
		if itemType == "function_call" || itemType == "custom_tool_call" {
			callID := ""
			if c, ok := item["call_id"].(string); ok {
				callID = c
			}
			if callID == "" {
				callID = fmt.Sprintf("call_%d_%d", shared.Number(state["toolCallIndex"]), time.Now().UnixMilli())
			}
			state["currentToolCallId"] = callID
			toolName := ""
			if n, ok := item["name"].(string); ok {
				toolName = n
			}
			idx := 0
			if v, ok := state["toolCallIndex"].(int); ok {
				idx = v
			}
			return []map[string]any{buildResponsesOpenAIChunk(state, map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index": idx,
						"id":    callID,
						"type":  "function",
						"function": map[string]any{
							"name":      toolName,
							"arguments": "",
						},
					},
				},
			}, nil)}
		}
		return nil

	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		deltaText := ""
		if d, ok := data["delta"].(string); ok {
			deltaText = d
		}
		if deltaText == "" {
			return nil
		}
		idx := 0
		if v, ok := state["toolCallIndex"].(int); ok {
			idx = v
		}
		return []map[string]any{buildResponsesOpenAIChunk(state, map[string]any{
			"tool_calls": []any{
				map[string]any{
					"index": idx,
					"function": map[string]any{
						"arguments": deltaText,
					},
				},
			},
		}, nil)}

	case "response.output_item.done":
		item, _ := data["item"].(map[string]any)
		if item == nil {
			return nil
		}
		itemType, _ := item["type"].(string)
		if itemType == "function_call" || itemType == "custom_tool_call" {
			idx := 0
			if v, ok := state["toolCallIndex"].(int); ok {
				idx = v
			}
			state["toolCallIndex"] = idx + 1
			state["currentToolCallId"] = nil
		}
		return nil

	case "response.completed", "response.done":
		response, _ := data["response"].(map[string]any)
		if response != nil {
			if usage, ok := response["usage"].(map[string]any); ok {
				inputTokens := shared.Number(usage["input_tokens"])
				outputTokens := shared.Number(usage["output_tokens"])
				if inputTokens == 0 {
					inputTokens = shared.Number(usage["prompt_tokens"])
				}
				if outputTokens == 0 {
					outputTokens = shared.Number(usage["completion_tokens"])
				}
				cachedTokens := 0
				if details, ok := usage["input_tokens_details"].(map[string]any); ok {
					cachedTokens = shared.Number(details["cached_tokens"])
				}
				if cachedTokens == 0 {
					cachedTokens = shared.Number(usage["cache_read_input_tokens"])
				}
				state["usage"] = shared.BuildUsage(inputTokens, outputTokens, inputTokens+outputTokens, cachedTokens, 0, 0)
			}
		}
		if state["finishReasonSent"] == true {
			return nil
		}
		finishReason := "stop"
		if state["toolCallIndex"] != nil {
			if n, ok := state["toolCallIndex"].(int); ok && n > 0 {
				finishReason = "tool_calls"
			}
		}
		if state["currentToolCallId"] != nil {
			if s, ok := state["currentToolCallId"].(string); ok && s != "" {
				finishReason = "tool_calls"
			}
		}
		state["finishReasonSent"] = true
		state["finishReason"] = finishReason
		final := buildResponsesOpenAIChunk(state, map[string]any{}, finishReason)
		if u, ok := state["usage"].(map[string]any); ok {
			final["usage"] = u
		}
		return []map[string]any{final}

	case "error", "response.failed":
		if state["finishReasonSent"] == true {
			return nil
		}
		errObj, _ := data["error"].(map[string]any)
		if errObj == nil {
			if response, ok := data["response"].(map[string]any); ok {
				errObj, _ = response["error"].(map[string]any)
			}
		}
		if errObj == nil {
			return nil
		}
		errMsg := ""
		if m, ok := errObj["message"].(string); ok {
			errMsg = m
		} else {
			errMsg = marshalJSONString(errObj)
		}
		state["finishReasonSent"] = true
		return []map[string]any{buildResponsesOpenAIChunk(state, map[string]any{"content": fmt.Sprintf("[Error] %s", errMsg)}, "stop")}

	case "response.reasoning_summary_text.delta":
		deltaText := ""
		if d, ok := data["delta"].(string); ok {
			deltaText = d
		}
		if deltaText == "" {
			return nil
		}
		return []map[string]any{buildResponsesOpenAIChunk(state, shared.ReasoningDelta(deltaText), nil)}
	}

	return nil
}

func buildResponsesOpenAIChunk(state map[string]any, delta map[string]any, finishReason any) map[string]any {
	id := ""
	if v, ok := state["chatId"].(string); ok {
		id = v
	}
	if id == "" {
		id = shared.FallbackChatID()
	}
	created := 0
	if v, ok := state["created"].(int); ok {
		created = v
	}
	model := "unknown"
	if v, ok := state["model"].(string); ok && v != "" {
		model = v
	}
	return shared.BuildChunk(id, created, model, delta, finishReason)
}
