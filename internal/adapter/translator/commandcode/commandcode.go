// Package commandcode implements the CommandCode-to-OpenAI response translator.
// It registers itself on the translator registry at init time, mirroring
// open-sse/translator/response/commandcode-to-openai.js.
package commandcode

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/shared"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

func init() {
	translator.RegisterResponse(format.Commandcode, format.Openai, commandCodeToOpenaiTranslator{})
}

type commandCodeToOpenaiTranslator struct{}

func (commandCodeToOpenaiTranslator) TranslateResponse(chunk json.RawMessage, state map[string]any) ([]json.RawMessage, error) {
	// If the chunk is already an OpenAI chunk, pass it through.
	var preCheck map[string]any
	if err := json.Unmarshal(chunk, &preCheck); err == nil {
		if preCheck["object"] == "chat.completion.chunk" && preCheck["choices"] != nil {
			return []json.RawMessage{chunk}, nil
		}
	}

	var body map[string]any
	if err := json.Unmarshal(chunk, &body); err != nil {
		// Try parsing as a raw JSON line (without "data:" framing).
		var s string
		if err2 := json.Unmarshal(chunk, &s); err2 == nil {
			line := strings.TrimSpace(s)
			if strings.HasPrefix(line, "data:") {
				line = strings.TrimSpace(line[5:])
			}
			if line != "" && line != "[DONE]" {
				if err3 := json.Unmarshal([]byte(line), &body); err3 != nil {
					return nil, nil
				}
			} else {
				return nil, nil
			}
		} else {
			return nil, nil
		}
	}

	out := commandCodeToOpenAIResponse(body, state)
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

func ensureCommandCodeState(state map[string]any, model string) {
	if state["responseId"] == nil {
		state["responseId"] = fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
		state["created"] = 0
		state["model"] = "commandcode"
		if model != "" {
			state["model"] = model
		}
		state["chunkIndex"] = 0
		state["toolIndex"] = 0
		state["toolIndexById"] = map[string]any{}
		state["openTools"] = map[string]any{}
		state["finishReason"] = nil
		state["usage"] = nil
	}
}

func commandCodeMakeChunk(state map[string]any, delta map[string]any, finishReason any) map[string]any {
	id := ""
	if v, ok := state["responseId"].(string); ok {
		id = v
	}
	created := 0
	if v, ok := state["created"].(int); ok {
		created = v
	}
	model := "commandcode"
	if v, ok := state["model"].(string); ok && v != "" {
		model = v
	}
	return shared.BuildChunk(id, created, model, delta, finishReason)
}

func commandCodeToOpenAIResponse(chunk map[string]any, state map[string]any) []map[string]any {
	if chunk == nil {
		return nil
	}
	eventType, _ := chunk["type"].(string)
	if eventType == "" {
		return nil
	}

	model := ""
	if m, ok := chunk["model"].(string); ok {
		model = m
	}
	ensureCommandCodeState(state, model)

	results := []map[string]any{}
	idx := 0
	if v, ok := state["chunkIndex"].(int); ok {
		idx = v
	}
	firstChunk := idx == 0

	switch eventType {
	case "text-delta":
		text := ""
		if t, ok := chunk["text"].(string); ok {
			text = t
		} else if d, ok := chunk["delta"].(string); ok {
			text = d
		}
		if text == "" {
			break
		}
		delta := map[string]any{"content": text}
		if firstChunk {
			delta["role"] = "assistant"
		}
		state["chunkIndex"] = idx + 1
		results = append(results, commandCodeMakeChunk(state, delta, nil))

	case "reasoning-delta":
		text := ""
		if t, ok := chunk["text"].(string); ok {
			text = t
		}
		if text == "" {
			break
		}
		state["chunkIndex"] = idx + 1
		results = append(results, commandCodeMakeChunk(state, shared.ReasoningDelta(text), nil))

	case "tool-input-start":
		rawID, _ := chunk["id"].(string)
		if rawID == "" {
			if tcid, ok := chunk["toolCallId"].(string); ok {
				rawID = tcid
			}
		}
		toolIndexByID, _ := state["toolIndexById"].(map[string]any)
		if toolIndexByID == nil {
			toolIndexByID = map[string]any{}
		}
		toolIdx := 0
		if v, ok := toolIndexByID[rawID].(int); ok {
			toolIdx = v
		} else {
			toolIdx = shared.Number(state["toolIndex"])
			state["toolIndex"] = toolIdx + 1
			toolIndexByID[rawID] = toolIdx
			state["toolIndexById"] = toolIndexByID
		}
		openTools, _ := state["openTools"].(map[string]any)
		if openTools == nil {
			openTools = map[string]any{}
		}
		openTools[rawID] = true
		state["openTools"] = openTools

		toolName := ""
		if n, ok := chunk["toolName"].(string); ok {
			toolName = n
		}
		delta := map[string]any{
			"tool_calls": []any{
				map[string]any{
					"index": toolIdx,
					"id":    rawID,
					"type":  "function",
					"function": map[string]any{
						"name":      toolName,
						"arguments": "",
					},
				},
			},
		}
		if firstChunk {
			delta["role"] = "assistant"
		}
		state["chunkIndex"] = idx + 1
		results = append(results, commandCodeMakeChunk(state, delta, nil))

	case "tool-input-delta":
		rawID, _ := chunk["id"].(string)
		if rawID == "" {
			if tcid, ok := chunk["toolCallId"].(string); ok {
				rawID = tcid
			}
		}
		toolIndexByID, _ := state["toolIndexById"].(map[string]any)
		if toolIndexByID == nil {
			break
		}
		toolIdx, ok := toolIndexByID[rawID].(int)
		if !ok {
			break
		}
		argsDelta := ""
		if d, ok := chunk["delta"].(string); ok {
			argsDelta = d
		} else if d, ok := chunk["inputTextDelta"].(string); ok {
			argsDelta = d
		}
		results = append(results, commandCodeMakeChunk(state, map[string]any{
			"tool_calls": []any{
				map[string]any{
					"index": toolIdx,
					"function": map[string]any{
						"arguments": argsDelta,
					},
				},
			},
		}, nil))

	case "finish-step":
		if reason, ok := chunk["finishReason"].(string); ok {
			state["finishReason"] = shared.ToOpenAIFinish(reason, "commandcode")
		}
		if usage, ok := chunk["usage"].(map[string]any); ok {
			state["usage"] = usage
		}

	case "finish":
		finishReason := "stop"
		if fr, ok := state["finishReason"].(string); ok && fr != "" {
			finishReason = fr
		} else if reason, ok := chunk["finishReason"].(string); ok {
			finishReason = shared.ToOpenAIFinish(reason, "commandcode")
		}
		final := commandCodeMakeChunk(state, map[string]any{}, finishReason)
		totalUsage := state["usage"]
		if u, ok := chunk["totalUsage"].(map[string]any); ok {
			totalUsage = u
		}
		if usage := shared.ToOpenAIUsage(toStringMap(totalUsage), "commandcode"); usage != nil {
			final["usage"] = usage
		}
		results = append(results, final)

	case "error":
		state["finishReason"] = "stop"
		errVal := chunk["error"]
		if errVal == nil {
			errVal = chunk["message"]
		}
		if errVal == nil {
			errVal = "unknown"
		}
		errStr := fmt.Sprintf("%v", errVal)
		results = append(results, commandCodeMakeChunk(state, map[string]any{"content": fmt.Sprintf("\n\n[CommandCode error: %s]", errStr)}, nil))
		results = append(results, commandCodeMakeChunk(state, map[string]any{}, "stop"))
	}

	if len(results) == 0 {
		return nil
	}
	return results
}

func toStringMap(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m
}
