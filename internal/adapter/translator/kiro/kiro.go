// Package kiro implements the Kiro-to-OpenAI response translator and the
// OpenAI-to-Kiro request translator. It registers itself on the translator
// registry at init time.
package kiro

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

func init() {
	translator.RegisterRequest(format.Openai, format.Kiro, openaiToKiroTranslator{})
	translator.RegisterResponse(format.Kiro, format.Openai, kiroToOpenaiTranslator{})
}

type openaiToKiroTranslator struct{}

func (openaiToKiroTranslator) TranslateRequest(model string, body json.RawMessage, stream bool, providerID string) (json.RawMessage, error) {
	return openaiToKiroRequest(model, body, stream)
}

type kiroToOpenaiTranslator struct{}

func (kiroToOpenaiTranslator) TranslateResponse(chunk json.RawMessage, state map[string]any) ([]json.RawMessage, error) {
	var body map[string]any
	if err := json.Unmarshal(chunk, &body); err != nil {
		return nil, fmt.Errorf("unmarshal kiro chunk: %w", err)
	}
	out := kiroToOpenAIResponse(body, state)
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

// -----------------------------------------------------------------------------
// Response translator
// -----------------------------------------------------------------------------

func kiroToOpenAIResponse(chunk map[string]any, state map[string]any) []map[string]any {
	if chunk == nil {
		return nil
	}

	// Already OpenAI chunk (executor may pre-transform).
	if chunk["object"] == "chat.completion.chunk" {
		if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
			return []map[string]any{chunk}
		}
	}

	if state["responseId"] == nil {
		state["responseId"] = fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
		state["created"] = 0
		state["chunkIndex"] = 0
		state["model"] = "kiro"
	}

	id := ""
	if v, ok := state["responseId"].(string); ok {
		id = v
	}
	created := 0
	if v, ok := state["created"].(int); ok {
		created = v
	}
	model := "kiro"
	if v, ok := state["model"].(string); ok && v != "" {
		model = v
	}
	chunkIndex := 0
	if v, ok := state["chunkIndex"].(int); ok {
		chunkIndex = v
	}
	firstChunk := chunkIndex == 0

	results := []map[string]any{}

	eventType := ""
	if t, ok := chunk["_eventType"].(string); ok {
		eventType = t
	}
	if eventType == "" && chunk["event"] != nil {
		eventType = fmt.Sprintf("%v", chunk["event"])
	}

	data := chunk
	// If _eventType names a nested event field (e.g. assistantResponseEvent),
	// prefer the nested payload even when both fields are present. This matches
	// the JS behavior where the wrapped payload carries the actual content.
	if eventType != "" {
		switch eventType {
		case "assistantResponseEvent":
			if v, ok := chunk["assistantResponseEvent"].(map[string]any); ok {
				data = v
			}
		case "reasoningContentEvent":
			if v, ok := chunk["reasoningContentEvent"].(map[string]any); ok {
				data = v
			}
		case "toolUseEvent":
			if v, ok := chunk["toolUseEvent"].(map[string]any); ok {
				data = v
			}
		case "usageEvent":
			if v, ok := chunk["usageEvent"].(map[string]any); ok {
				data = v
			}
		}
	} else {
		// Direct field dispatch (e.g. {assistantResponseEvent: {...}}).
		if _, ok := chunk["assistantResponseEvent"]; ok {
			eventType = "assistantResponseEvent"
			data, _ = chunk["assistantResponseEvent"].(map[string]any)
			if data == nil {
				data = chunk
			}
		} else if _, ok := chunk["reasoningContentEvent"]; ok {
			eventType = "reasoningContentEvent"
			data, _ = chunk["reasoningContentEvent"].(map[string]any)
			if data == nil {
				data = chunk
			}
		} else if _, ok := chunk["toolUseEvent"]; ok {
			eventType = "toolUseEvent"
			data, _ = chunk["toolUseEvent"].(map[string]any)
			if data == nil {
				data = chunk
			}
		} else if _, ok := chunk["usageEvent"]; ok {
			eventType = "usageEvent"
			data, _ = chunk["usageEvent"].(map[string]any)
			if data == nil {
				data = chunk
			}
		} else if _, ok := chunk["messageStopEvent"]; ok {
			eventType = "messageStopEvent"
		}
	}

	switch eventType {
	case "assistantResponseEvent":
		content := ""
		if d := data; d != nil {
			if c, ok := d["content"].(string); ok {
				content = c
			}
		} else if c, ok := chunk["content"].(string); ok {
			content = c
		}
		if content == "" {
			return nil
		}
		delta := map[string]any{"content": content}
		if firstChunk {
			delta["role"] = "assistant"
		}
		state["chunkIndex"] = chunkIndex + 1
		results = append(results, shared.BuildChunk(id, created, model, delta, nil))

	case "reasoningContentEvent":
		text := ""
		if d := data; d != nil {
			if t, ok := d["text"].(string); ok {
				text = t
			} else if t, ok := d["content"].(string); ok {
				text = t
			}
		} else if c, ok := chunk["content"].(string); ok {
			text = c
		}
		if text == "" {
			return nil
		}
		delta := shared.ReasoningDelta(text)
		if firstChunk {
			delta["role"] = "assistant"
		}
		state["chunkIndex"] = chunkIndex + 1
		results = append(results, shared.BuildChunk(id, created, model, delta, nil))

	case "toolUseEvent":
		state["hadToolUse"] = true
		toolUse := data
		if toolUse == nil {
			toolUse = map[string]any{}
		}
		toolUseID := ""
		if v, ok := toolUse["toolUseId"].(string); ok {
			toolUseID = v
		}
		if toolUseID == "" {
			toolUseID = fmt.Sprintf("call_%d_%d", 0, time.Now().UnixMilli())
		}
		toolName := ""
		if v, ok := toolUse["name"].(string); ok {
			toolName = v
		}
		toolInput := map[string]any{}
		if v, ok := toolUse["input"].(map[string]any); ok {
			toolInput = v
		}
		args, _ := json.Marshal(toolInput)
		argsStr := string(args)
		if argsStr == "" {
			argsStr = "{}"
		}

		delta := map[string]any{
			"tool_calls": []any{
				map[string]any{
					"index": 0,
					"id":    toolUseID,
					"type":  "function",
					"function": map[string]any{
						"name":      toolName,
						"arguments": argsStr,
					},
				},
			},
		}
		if firstChunk {
			delta["role"] = "assistant"
		}
		state["chunkIndex"] = chunkIndex + 1
		results = append(results, shared.BuildChunk(id, created, model, delta, nil))

	case "usageEvent":
		usageData := data
		if usageData == nil {
			usageData = chunk
		}
		if u := usageData; u != nil {
			if usage := shared.ToOpenAIUsage(u, "kiro"); usage != nil {
				state["usage"] = usage
			}
		}
		return nil

	case "messageStopEvent", "done":
		finishReason := "stop"
		if state["hadToolUse"] == true {
			finishReason = "tool_calls"
		}
		state["finishReason"] = finishReason
		final := shared.BuildChunk(id, created, model, map[string]any{}, finishReason)
		if u, ok := state["usage"].(map[string]any); ok {
			final["usage"] = u
		}
		results = append(results, final)
	}

	if len(results) == 0 {
		return nil
	}
	return results
}

// -----------------------------------------------------------------------------
// Request translator (simplified port of openai-to-kiro.js for golden contract)
// -----------------------------------------------------------------------------

var dataURIRe = regexp.MustCompile(`^data:([^;]+);base64,(.+)$`)

func openaiToKiroRequest(model string, raw json.RawMessage, stream bool) (json.RawMessage, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("unmarshal body: %w", err)
	}

	messages := []map[string]any{}
	if rawMsgs, ok := body["messages"].([]any); ok {
		for _, m := range rawMsgs {
			if msg, ok := m.(map[string]any); ok {
				messages = append(messages, msg)
			}
		}
	}
	tools := []map[string]any{}
	if rawTools, ok := body["tools"].([]any); ok {
		for _, t := range rawTools {
			if tool, ok := t.(map[string]any); ok {
				tools = append(tools, tool)
			}
		}
	}

	temperature := 0.0
	if t, ok := body["temperature"].(float64); ok {
		temperature = t
	}

	// Simplified conversion matching the golden snapshot shape.
	history, currentMessage := convertOpenAIMessagesToKiro(messages, tools, model)

	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	currentTimeContext := fmt.Sprintf("[Context: Current time is %s]", timestamp)

	// Inject context prefix into every userInputMessage.
	for _, item := range history {
		if uim, ok := item["userInputMessage"].(map[string]any); ok {
			uim["content"] = prefixCurrentTime(fmt.Sprintf("%v", uim["content"]), currentTimeContext)
		}
	}
	if currentMessage != nil {
		if uim, ok := currentMessage["userInputMessage"].(map[string]any); ok {
			uim["content"] = prefixCurrentTime(fmt.Sprintf("%v", uim["content"]), currentTimeContext)
			uim["origin"] = "AI_EDITOR"
		}
	}

	payload := map[string]any{
		"agentMode": "vibe",
		"conversationState": map[string]any{
			"agentTaskType": "vibe",
			"chatTriggerType": "MANUAL",
			"currentMessage":  currentMessage,
			"history":         history,
		},
		"inferenceConfig": map[string]any{
			"maxTokens":   32000,
			"temperature": temperature,
		},
		"profileArn": "arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX",
	}

	return json.Marshal(payload)
}

func prefixCurrentTime(content, context string) string {
	if content == "" {
		content = "continue"
	}
	return context + "\n\n" + content
}

func convertOpenAIMessagesToKiro(messages []map[string]any, tools []map[string]any, model string) ([]map[string]any, map[string]any) {
	history := []map[string]any{}

	clientProvidedTools := len(tools) > 0
	buildToolSpecs := func() []any {
		out := []any{}
		for _, t := range tools {
			name, desc, schema := extractToolInfo(t)
			if name == "" {
				continue
			}
			if strings.TrimSpace(desc) == "" {
				desc = fmt.Sprintf("Tool: %s", name)
			}
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}
			}
			out = append(out, map[string]any{
				"toolSpecification": map[string]any{
					"name":        name,
					"description": desc,
					"inputSchema": map[string]any{"json": schema},
				},
			})
		}
		return out
	}

	var pendingUserContent []string
	var pendingAssistantContent []string
	var pendingToolResults []any
	var pendingImages []any
	var currentRole string
	toolsInjected := false

	flush := func() {
		switch currentRole {
		case "user":
			content := strings.TrimSpace(strings.Join(pendingUserContent, "\n\n"))
			if content == "" {
				content = "continue"
			}
			uim := map[string]any{
				"content": content,
				"modelId": model,
			}
			if len(pendingImages) > 0 {
				uim["images"] = pendingImages
			}
			ctx := map[string]any{}
			if len(pendingToolResults) > 0 {
				ctx["toolResults"] = pendingToolResults
			}
			if clientProvidedTools && !toolsInjected {
				ctx["tools"] = buildToolSpecs()
				toolsInjected = true
			}
			if len(ctx) > 0 {
				uim["userInputMessageContext"] = ctx
			}
			history = append(history, map[string]any{"userInputMessage": uim})
			pendingUserContent = nil
			pendingToolResults = nil
			pendingImages = nil
		case "assistant":
			content := strings.TrimSpace(strings.Join(pendingAssistantContent, "\n\n"))
			if content == "" {
				content = "..."
			}
			history = append(history, map[string]any{
				"assistantResponseMessage": map[string]any{"content": content},
			})
			pendingAssistantContent = nil
		}
	}

	for _, msg := range messages {
		role, _ := msg["role"].(string)
		wasSystem := role == "system"
		if role == "system" || role == "tool" {
			role = "user"
		}

		if currentRole != "" && currentRole != role {
			flush()
		}
		currentRole = role

		if role == "user" {
			content := msg["content"]
			switch c := content.(type) {
			case string:
				if msg["role"] == "tool" {
					if tcid, ok := msg["tool_call_id"].(string); ok && tcid != "" {
						pendingToolResults = append(pendingToolResults, map[string]any{
							"toolUseId": tcid,
							"status":    "success",
							"content":   []any{map[string]any{"text": c}},
						})
					}
				} else if c != "" {
					pendingUserContent = append(pendingUserContent, wrapSystemText(c, wasSystem))
				}
			case []any:
				texts := []string{}
				for _, itemAny := range c {
					item, ok := itemAny.(map[string]any)
					if !ok {
						continue
					}
					typ, _ := item["type"].(string)
					switch typ {
					case "text":
						if t, ok := item["text"].(string); ok && t != "" {
							texts = append(texts, wrapSystemText(t, wasSystem))
						}
					case "image_url":
						if iu, ok := item["image_url"].(map[string]any); ok {
							url, _ := iu["url"].(string)
							if m := dataURIRe.FindStringSubmatch(url); m != nil {
								format := strings.TrimPrefix(m[1], "image/")
								pendingImages = append(pendingImages, map[string]any{
									"format": format,
									"source": map[string]any{"bytes": m[2]},
								})
							}
						}
					}
				}
				if len(texts) > 0 {
					pendingUserContent = append(pendingUserContent, strings.Join(texts, "\n"))
				}
			}
		} else if role == "assistant" {
			var textContent string
			var toolUses []any

			switch c := msg["content"].(type) {
			case string:
				textContent = strings.TrimSpace(c)
			case []any:
				for _, itemAny := range c {
					item, ok := itemAny.(map[string]any)
					if !ok {
						continue
					}
					typ, _ := item["type"].(string)
					switch typ {
					case "text":
						if t, ok := item["text"].(string); ok {
							textContent += t + "\n"
						}
					case "tool_use":
						toolUses = append(toolUses, item)
					}
				}
			}

			if rawTCs, ok := msg["tool_calls"].([]any); ok {
				for _, raw := range rawTCs {
					tc, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := tc["function"].(map[string]any)
					if fn == nil {
						continue
					}
					name, _ := fn["name"].(string)
					argsRaw, _ := fn["arguments"].(string)
					var input any
					if err := json.Unmarshal([]byte(argsRaw), &input); err != nil {
						input = map[string]any{}
					}
					id, _ := tc["id"].(string)
					toolUses = append(toolUses, map[string]any{
						"toolUseId": id,
						"name":      name,
						"input":     input,
					})
				}
			}

			if textContent != "" {
				pendingAssistantContent = append(pendingAssistantContent, textContent)
			}
			if len(toolUses) > 0 {
				flush()
				last := history[len(history)-1]
				if arm, ok := last["assistantResponseMessage"].(map[string]any); ok {
					converted := []any{}
					for _, tu := range toolUses {
						if m, ok := tu.(map[string]any); ok {
							converted = append(converted, m)
						}
					}
					arm["toolUses"] = converted
				}
				currentRole = ""
			}
		}
	}
	if currentRole != "" {
		flush()
	}

	// Pop the last userInputMessage as currentMessage, matching JS behavior.
	var currentMessage map[string]any
	for i := len(history) - 1; i >= 0; i-- {
		if uim, ok := history[i]["userInputMessage"].(map[string]any); ok {
			currentMessage = map[string]any{"userInputMessage": uim}
			history = append(history[:i], history[i+1:]...)
			break
		}
	}

	// Grab tools from first history item BEFORE cleanup removes them.
	var firstHistoryTools []any
	if len(history) > 0 {
		if uim, ok := history[0]["userInputMessage"].(map[string]any); ok {
			if ctx, ok := uim["userInputMessageContext"].(map[string]any); ok {
				firstHistoryTools, _ = ctx["tools"].([]any)
			}
		}
	}

	// Clean up history: remove tools, drop empty contexts, fill modelId.
	for _, item := range history {
		if uim, ok := item["userInputMessage"].(map[string]any); ok {
			if ctx, ok := uim["userInputMessageContext"].(map[string]any); ok {
				delete(ctx, "tools")
				if len(ctx) == 0 {
					delete(uim, "userInputMessageContext")
				}
			}
			if uim["modelId"] == "" {
				uim["modelId"] = model
			}
		}
	}

	// Merge consecutive userInputMessages in history so Kiro gets alternating
	// user/assistant turns. Combine content, images, and contexts.
	mergedHistory := []map[string]any{}
	for _, item := range history {
		if uim, ok := item["userInputMessage"].(map[string]any); ok && len(mergedHistory) > 0 {
			prev := mergedHistory[len(mergedHistory)-1]
			if prevUim, ok := prev["userInputMessage"].(map[string]any); ok {
				prevUim["content"] = fmt.Sprintf("%v\n\n%v", prevUim["content"], uim["content"])
				if imgs, ok := uim["images"].([]any); ok && len(imgs) > 0 {
					prevImgs, _ := prevUim["images"].([]any)
					prevUim["images"] = append(prevImgs, imgs...)
				}
				if curCtx, ok := uim["userInputMessageContext"].(map[string]any); ok {
					prevCtx, _ := prevUim["userInputMessageContext"].(map[string]any)
					if prevCtx == nil {
						prevCtx = map[string]any{}
						prevUim["userInputMessageContext"] = prevCtx
					}
					if trs, ok := curCtx["toolResults"].([]any); ok {
						prevTrs, _ := prevCtx["toolResults"].([]any)
						prevCtx["toolResults"] = append(prevTrs, trs...)
					}
					if tools, ok := curCtx["tools"].([]any); ok {
						prevTools, _ := prevCtx["tools"].([]any)
						prevCtx["tools"] = append(prevTools, tools...)
					}
				}
				continue
			}
		}
		mergedHistory = append(mergedHistory, item)
	}

	// Ensure a currentMessage exists even when no user turn is present.
	if currentMessage == nil {
		currentMessage = map[string]any{
			"userInputMessage": map[string]any{
				"content": "",
				"modelId": model,
			},
		}
	}

	// Inject tools from first history item into currentMessage.
	if len(firstHistoryTools) > 0 {
		uim, _ := currentMessage["userInputMessage"].(map[string]any)
		if uim != nil {
			ctx, _ := uim["userInputMessageContext"].(map[string]any)
			if ctx == nil {
				ctx = map[string]any{}
				uim["userInputMessageContext"] = ctx
			}
			if _, hasTools := ctx["tools"]; !hasTools {
				ctx["tools"] = firstHistoryTools
			}
		}
	}

	return mergedHistory, currentMessage
}

func wrapSystemText(text string, wasSystem bool) string {
	if wasSystem {
		return fmt.Sprintf("<instructions>\n%s\n</instructions>", text)
	}
	return text
}

func extractToolInfo(tool map[string]any) (string, string, map[string]any) {
	if fn, ok := tool["function"].(map[string]any); ok {
		name, _ := fn["name"].(string)
		desc, _ := fn["description"].(string)
		schema, _ := fn["parameters"].(map[string]any)
		return name, desc, schema
	}
	name, _ := tool["name"].(string)
	desc, _ := tool["description"].(string)
	schema, _ := tool["input_schema"].(map[string]any)
	return name, desc, schema
}
