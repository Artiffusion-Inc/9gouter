// Package claude implements the Claude-to-OpenAI response translator and the
// OpenAI-to-Claude response translator. It registers itself on the translator
// registry at init time, mirroring open-sse/translator/response/claude-to-openai.js
// and open-sse/translator/response/openai-to-claude.js.
package claude

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/shared"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

const (
	claudeBlockText       = "text"
	claudeBlockThinking   = "thinking"
	claudeBlockToolUse    = "tool_use"
	claudeBlockToolResult = "tool_result"
	roleAssistant         = "assistant"
)

func init() {
	translator.RegisterResponse(format.Claude, format.Openai, claudeToOpenaiTranslator{})
	translator.RegisterResponse(format.Openai, format.Claude, openaiToClaudeTranslator{})
}

type claudeToOpenaiTranslator struct{}

func (claudeToOpenaiTranslator) TranslateResponse(chunk json.RawMessage, state map[string]any) ([]json.RawMessage, error) {
	var body map[string]any
	if err := json.Unmarshal(chunk, &body); err != nil {
		return nil, fmt.Errorf("unmarshal claude chunk: %w", err)
	}
	out := claudeToOpenAIResponse(body, state)
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

type openaiToClaudeTranslator struct{}

func (openaiToClaudeTranslator) TranslateResponse(chunk json.RawMessage, state map[string]any) ([]json.RawMessage, error) {
	var body map[string]any
	if err := json.Unmarshal(chunk, &body); err != nil {
		return nil, fmt.Errorf("unmarshal openai chunk: %w", err)
	}
	out := openaiToClaudeResponse(body, state)
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

func createChunk(state map[string]any, delta map[string]any, finishReason any) map[string]any {
	id := ""
	if v, ok := state["messageId"].(string); ok {
		id = fmt.Sprintf("chatcmpl-%s", v)
	}
	if id == "" || id == "chatcmpl-" {
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

func claudeToOpenAIResponse(chunk map[string]any, state map[string]any) []map[string]any {
	if chunk == nil {
		return nil
	}

	results := []map[string]any{}
	event, _ := chunk["type"].(string)

	switch event {
	case "message_start":
		msg, _ := chunk["message"].(map[string]any)
		state["messageId"] = ""
		if msg != nil {
			if id, ok := msg["id"].(string); ok && id != "" {
				state["messageId"] = id
			}
			if m, ok := msg["model"].(string); ok {
				state["model"] = m
			}
		}
		if state["messageId"] == "" {
			state["messageId"] = shared.GenerateMessageID()
		}
		state["toolCallIndex"] = 0

		if startUsage, ok := msg["usage"].(map[string]any); ok && startUsage != nil {
			inputTokens := shared.Number(startUsage["input_tokens"])
			cacheRead := shared.Number(startUsage["cache_read_input_tokens"])
			cacheCreate := shared.Number(startUsage["cache_creation_input_tokens"])
			promptTokens := inputTokens + cacheRead + cacheCreate
			u := map[string]any{
				"prompt_tokens":     promptTokens,
				"completion_tokens": 0,
				"total_tokens":      promptTokens,
				"input_tokens":      inputTokens,
				"output_tokens":     0,
			}
			if cacheRead > 0 {
				u["cache_read_input_tokens"] = cacheRead
			}
			if cacheCreate > 0 {
				u["cache_creation_input_tokens"] = cacheCreate
			}
			state["usage"] = u
		}

		results = append(results, createChunk(state, map[string]any{"role": roleAssistant}, nil))

	case "content_block_start":
		block, _ := chunk["content_block"].(map[string]any)
		if block == nil {
			break
		}
		if block["type"] == "server_tool_use" {
			state["serverToolBlockIndex"] = chunk["index"]
			break
		}
		switch block["type"] {
		case claudeBlockText:
			state["textBlockStarted"] = true
		case claudeBlockThinking:
			state["inThinkingBlock"] = true
			state["currentBlockIndex"] = chunk["index"]
			results = append(results, createChunk(state, map[string]any{"content": "<think>"}, nil))
		case claudeBlockToolUse:
			toolCallIndex := 0
			if v, ok := state["toolCallIndex"].(int); ok {
				toolCallIndex = v
			}
			state["toolCallIndex"] = toolCallIndex + 1
			name, _ := block["name"].(string)
			toolName := name
			if m, ok := state["toolNameMap"].(map[string]string); ok {
				if orig, ok := m[name]; ok {
					toolName = orig
				}
			}
			toolCall := map[string]any{
				"index": toolCallIndex,
				"id":    block["id"],
				"type":  "function",
				"function": map[string]any{
					"name":      toolName,
					"arguments": "",
				},
				"_started": false,
			}
			idx := fmt.Sprintf("%v", chunk["index"])
			if tc, ok := state["toolCalls"].(map[string]any); ok {
				tc[idx] = toolCall
			} else {
				state["toolCalls"] = map[string]any{idx: toolCall}
			}
			// Do not emit a start chunk here; the first input_json_delta will emit
			// the tool call with full accumulated arguments, matching JS snapshot.
		}

	case "content_block_delta":
		if chunk["index"] == state["serverToolBlockIndex"] {
			break
		}
		delta, _ := chunk["delta"].(map[string]any)
		if delta == nil {
			break
		}
		deltaType, _ := delta["type"].(string)
		switch deltaType {
		case "text_delta":
			if text, ok := delta["text"].(string); ok && text != "" {
				results = append(results, createChunk(state, map[string]any{"content": text}, nil))
			}
		case "thinking_delta":
			if thinking, ok := delta["thinking"].(string); ok && thinking != "" {
				results = append(results, createChunk(state, shared.ReasoningDelta(thinking), nil))
			}
		case "input_json_delta":
			idx := fmt.Sprintf("%v", chunk["index"])
			tc, _ := state["toolCalls"].(map[string]any)
			if tc == nil {
				break
			}
			toolCall, _ := tc[idx].(map[string]any)
			if toolCall == nil {
				break
			}
			partial, _ := delta["partial_json"].(string)
			fn, _ := toolCall["function"].(map[string]any)
			if fn == nil {
				fn = map[string]any{}
				toolCall["function"] = fn
			}
			fn["arguments"] = fmt.Sprintf("%v", fn["arguments"]) + partial
			// Suppress no-op empty deltas after the tool call has already started.
			if partial == "" {
				if started, _ := toolCall["_started"].(bool); started {
					break
				}
			}
			emitChunk := map[string]any{
				"index": toolCall["index"],
				"id":    toolCall["id"],
				"function": map[string]any{
					"name":      fn["name"],
					"arguments": fn["arguments"],
				},
			}
			started, _ := toolCall["_started"].(bool)
			if !started {
				emitChunk["type"] = "function"
				toolCall["_started"] = true
			}
			results = append(results, createChunk(state, map[string]any{
				"tool_calls": []any{emitChunk},
			}, nil))
		}

	case "content_block_stop":
		if chunk["index"] == state["serverToolBlockIndex"] {
			state["serverToolBlockIndex"] = -1
			break
		}
		if state["inThinkingBlock"] == true && chunk["index"] == state["currentBlockIndex"] {
			results = append(results, createChunk(state, map[string]any{"content": "</think>"}, nil))
			state["inThinkingBlock"] = false
		}
		state["textBlockStarted"] = false
		state["thinkingBlockStarted"] = false

		// Emit the trailing tool_calls delta (no type) when a tool_use block ends.
		idx := fmt.Sprintf("%v", chunk["index"])
		if tc, ok := state["toolCalls"].(map[string]any); ok {
			if toolCall, ok := tc[idx].(map[string]any); ok {
				if started, _ := toolCall["_started"].(bool); started {
					fn, _ := toolCall["function"].(map[string]any)
					if fn != nil {
						results = append(results, createChunk(state, map[string]any{
							"tool_calls": []any{
								map[string]any{
									"index": toolCall["index"],
									"id":    toolCall["id"],
									"function": map[string]any{
										"arguments": fn["arguments"],
									},
								},
							},
						}, nil))
					}
				}
			}
		}

	case "message_delta":
		if usage, ok := chunk["usage"].(map[string]any); ok && usage != nil {
			prev := map[string]any{}
			if u, ok := state["usage"].(map[string]any); ok {
				prev = u
			}
			inputTokens := shared.Number(usage["input_tokens"])
			if inputTokens == 0 {
				inputTokens = shared.Number(prev["input_tokens"])
			}
			outputTokens := shared.Number(usage["output_tokens"])
			cacheRead := shared.Number(usage["cache_read_input_tokens"])
			if cacheRead == 0 {
				cacheRead = shared.Number(prev["cache_read_input_tokens"])
			}
			cacheCreate := shared.Number(usage["cache_creation_input_tokens"])
			if cacheCreate == 0 {
				cacheCreate = shared.Number(prev["cache_creation_input_tokens"])
			}
			promptTokens := inputTokens + cacheRead + cacheCreate
			u := map[string]any{
				"prompt_tokens":     promptTokens,
				"completion_tokens": outputTokens,
				"total_tokens":      promptTokens + outputTokens,
				"input_tokens":      inputTokens,
				"output_tokens":     outputTokens,
			}
			if cacheRead > 0 {
				u["cache_read_input_tokens"] = cacheRead
			}
			if cacheCreate > 0 {
				u["cache_creation_input_tokens"] = cacheCreate
			}
			state["usage"] = u
		}

		if delta, ok := chunk["delta"].(map[string]any); ok {
			if reason, ok := delta["stop_reason"].(string); ok && reason != "" {
				state["finishReason"] = convertStopReason(reason)
				finalChunk := createChunk(state, map[string]any{}, state["finishReason"])
				if u, ok := state["usage"].(map[string]any); ok {
					finalChunk["usage"] = shared.ToOpenAIUsage(u, "claude")
				}
				results = append(results, finalChunk)
				state["finishReasonSent"] = true
			}
		}

	case "message_stop":
		if state["finishReasonSent"] != true {
			finishReason := "stop"
			if fr, ok := state["finishReason"].(string); ok && fr != "" {
				finishReason = fr
			} else if tc, ok := state["toolCalls"].(map[string]any); ok && len(tc) > 0 {
				finishReason = "tool_calls"
			}
			finalChunk := createChunk(state, map[string]any{}, finishReason)
			if u, ok := state["usage"].(map[string]any); ok {
				finalChunk["usage"] = map[string]any{
					"prompt_tokens":     u["input_tokens"],
					"completion_tokens": u["output_tokens"],
					"total_tokens":      shared.Number(u["input_tokens"]) + shared.Number(u["output_tokens"]),
				}
			}
			results = append(results, finalChunk)
			state["finishReasonSent"] = true
		}
	}

	if len(results) == 0 {
		return nil
	}
	return results
}

func convertStopReason(reason string) string {
	return shared.ToOpenAIFinish(reason, "claude")
}

func openaiToClaudeResponse(chunk map[string]any, state map[string]any) []map[string]any {
	choice, ok := chunk["choices"].([]any)
	if !ok || len(choice) == 0 {
		return nil
	}
	first, ok := choice[0].(map[string]any)
	if !ok {
		return nil
	}

	results := []map[string]any{}
	delta, _ := first["delta"].(map[string]any)

	if chunk["usage"] != nil {
		if u, ok := chunk["usage"].(map[string]any); ok {
			promptTokens := shared.Number(u["prompt_tokens"])
			outputTokens := shared.Number(u["completion_tokens"])
			// prompt_tokens_details is optional; OpenAI providers add it
			// (cached_tokens / cache_creation_tokens), but other upstreams
			// (ollama, llama.cpp) emit a flat usage with no details. A bare
			// type assertion on a missing/nil field panics ("interface {} is
			// nil, not map[string]interface {}"), so navigate defensively and
			// treat absent details as zero cache.
			var cacheRead, cacheCreate int
			if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
				cacheRead = shared.Number(d["cached_tokens"])
				cacheCreate = shared.Number(d["cache_creation_tokens"])
			}
			inputTokens := promptTokens - cacheRead - cacheCreate
			state["usage"] = map[string]any{
				"input_tokens":                inputTokens,
				"output_tokens":               outputTokens,
				"cache_read_input_tokens":     cacheRead,
				"cache_creation_input_tokens": cacheCreate,
			}
		}
	}

	if state["messageStartSent"] != true {
		state["messageStartSent"] = true
		id := ""
		if rawID, ok := chunk["id"].(string); ok {
			id = strings.Replace(rawID, "chatcmpl-", "", 1)
		}
		if id == "" || id == "chat" || len(id) < 8 {
			id = shared.GenerateMessageID()
		}
		state["messageId"] = id
		if m, ok := chunk["model"].(string); ok {
			state["model"] = m
		} else {
			state["model"] = "unknown"
		}
		state["nextBlockIndex"] = 0
		results = append(results, map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":              id,
				"type":            "message",
				"role":            roleAssistant,
				"model":           state["model"],
				"content":         []any{},
				"stop_reason":     nil,
				"stop_sequence":   nil,
				"usage":           map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})
	}

	reasoningContent := shared.ExtractReasoningText(delta)
	if reasoningContent != "" {
		stopTextBlock(state, &results)
		if state["thinkingBlockStarted"] != true {
			state["thinkingBlockStarted"] = true
			idx := state["nextBlockIndex"].(int)
			state["thinkingBlockIndex"] = idx
			state["nextBlockIndex"] = idx + 1
			results = append(results, map[string]any{
				"type":          "content_block_start",
				"index":         idx,
				"content_block": map[string]any{"type": claudeBlockThinking, "thinking": ""},
			})
		}
		results = append(results, map[string]any{
			"type":  "content_block_delta",
			"index": state["thinkingBlockIndex"],
			"delta": map[string]any{"type": "thinking_delta", "thinking": reasoningContent},
		})
	}

	if content, ok := delta["content"].(string); ok && content != "" {
		stopThinkingBlock(state, &results)
		if state["textBlockStarted"] != true {
			state["textBlockStarted"] = true
			state["textBlockClosed"] = false
			idx := state["nextBlockIndex"].(int)
			state["textBlockIndex"] = idx
			state["nextBlockIndex"] = idx + 1
			results = append(results, map[string]any{
				"type":          "content_block_start",
				"index":         idx,
				"content_block": map[string]any{"type": claudeBlockText, "text": ""},
			})
		}
		results = append(results, map[string]any{
			"type":  "content_block_delta",
			"index": state["textBlockIndex"],
			"delta": map[string]any{"type": "text_delta", "text": content},
		})
	}

	if rawToolCalls, ok := delta["tool_calls"].([]any); ok {
		for _, rawTC := range rawToolCalls {
			tc, ok := rawTC.(map[string]any)
			if !ok {
				continue
			}
			idx := 0
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			}
			if tc["id"] != nil {
				stopThinkingBlock(state, &results)
				stopTextBlock(state, &results)
				toolBlockIndex := state["nextBlockIndex"].(int)
				state["nextBlockIndex"] = toolBlockIndex + 1
				if state["toolCalls"] == nil {
					state["toolCalls"] = map[string]any{}
				}
				toolName := ""
				if fn, ok := tc["function"].(map[string]any); ok {
					toolName, _ = fn["name"].(string)
				}
				state["toolCalls"].(map[string]any)[fmt.Sprintf("%d", idx)] = map[string]any{
					"id":         tc["id"],
					"name":       toolName,
					"blockIndex": toolBlockIndex,
				}
				results = append(results, map[string]any{
					"type":  "content_block_start",
					"index": toolBlockIndex,
					"content_block": map[string]any{
						"type":  claudeBlockToolUse,
						"id":    tc["id"],
						"name":  toolName,
						"input": map[string]any{},
					},
				})
			}
			if fn, ok := tc["function"].(map[string]any); ok && fn["arguments"] != nil {
				if state["toolArgBuffers"] == nil {
					state["toolArgBuffers"] = map[string]any{}
				}
				buf := ""
				if v, ok := state["toolArgBuffers"].(map[string]any)[fmt.Sprintf("%d", idx)].(string); ok {
					buf = v
				}
				state["toolArgBuffers"].(map[string]any)[fmt.Sprintf("%d", idx)] = buf + fmt.Sprintf("%v", fn["arguments"])
			}
		}
	}

	if first["finish_reason"] != nil {
		stopThinkingBlock(state, &results)
		stopTextBlock(state, &results)

		if tc, ok := state["toolCalls"].(map[string]any); ok {
			for _, raw := range tc {
				toolInfo, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				idx := toolInfo["blockIndex"]
				if bufs, ok := state["toolArgBuffers"].(map[string]any); ok {
					for k, v := range bufs {
						_ = k
						results = append(results, map[string]any{
							"type":  "content_block_delta",
							"index": idx,
							"delta": map[string]any{"type": "input_json_delta", "partial_json": v},
						})
					}
				}
				results = append(results, map[string]any{
					"type":  "content_block_stop",
					"index": idx,
				})
			}
		}

		state["finishReason"] = first["finish_reason"]
		finalUsage := map[string]any{"input_tokens": 0, "output_tokens": 0}
		if u, ok := state["usage"].(map[string]any); ok {
			finalUsage = u
		}
		results = append(results, map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason": shared.FromOpenAIFinish(fmt.Sprintf("%v", first["finish_reason"]), "claude"),
			},
			"usage": finalUsage,
		})
		results = append(results, map[string]any{"type": "message_stop"})
	}

	if len(results) == 0 {
		return nil
	}
	return results
}

func stopThinkingBlock(state map[string]any, results *[]map[string]any) {
	if state["thinkingBlockStarted"] == true {
		*results = append(*results, map[string]any{
			"type":  "content_block_stop",
			"index": state["thinkingBlockIndex"],
		})
		state["thinkingBlockStarted"] = false
	}
}

func stopTextBlock(state map[string]any, results *[]map[string]any) {
	if state["textBlockStarted"] == true && state["textBlockClosed"] != true {
		state["textBlockClosed"] = true
		*results = append(*results, map[string]any{
			"type":  "content_block_stop",
			"index": state["textBlockIndex"],
		})
		state["textBlockStarted"] = false
	}
}
