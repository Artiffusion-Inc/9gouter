// Package ollama implements the Ollama-to-OpenAI response translator.
// It registers itself on the translator registry at init time, mirroring
// open-sse/translator/response/ollama-to-openai.js.
package ollama

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/shared"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

func init() {
	translator.RegisterResponse(format.Ollama, format.Openai, ollamaToOpenaiTranslator{})
}

type ollamaToOpenaiTranslator struct{}

func (ollamaToOpenaiTranslator) TranslateResponse(chunk json.RawMessage, state map[string]any) ([]json.RawMessage, error) {
	var body map[string]any
	if err := json.Unmarshal(chunk, &body); err != nil {
		return nil, fmt.Errorf("unmarshal ollama chunk: %w", err)
	}
	out := ollamaToOpenAIResponse(body, state)
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

func ollamaToOpenAIResponse(chunk map[string]any, state map[string]any) []map[string]any {
	if chunk == nil {
		return nil
	}

	// Initialize state on first chunk.
	if state["ollama"] == nil {
		now := int(time.Now().UnixMilli())
		model := "ollama"
		if m, ok := chunk["model"].(string); ok && m != "" {
			model = m
		} else if m, ok := state["model"].(string); ok && m != "" {
			model = m
		}
		state["ollama"] = map[string]any{
			"id":      fmt.Sprintf("chatcmpl-%d", now),
			"created": 0,
			"model":   model,
		}
		state["model"] = model
	}
	meta, _ := state["ollama"].(map[string]any)
	if meta == nil {
		meta = map[string]any{"id": shared.FallbackChatID(), "created": 0, "model": "ollama"}
	}
	id, _ := meta["id"].(string)
	created, _ := meta["created"].(int)
	model, _ := meta["model"].(string)

	// Parse the message fields before the done check: some upstreams deliver the
	// final content/thinking in the SAME chunk as "done": true (e.g.
	// {"message":{"content":"!"},"done":true}). Returning an empty delta on
	// done would drop that final token. This mirrors the upstream fix for
	// decolua/9gouter issue #2694 (final-chunk content cutoff).
	message, _ := chunk["message"].(map[string]any)
	content, _ := message["content"].(string)
	thinking, _ := message["thinking"].(string)
	rawToolCalls, _ := message["tool_calls"].([]any)

	// Final chunk.
	if done, _ := chunk["done"].(bool); done {
		finishReason := shared.ToOpenAIFinish(fmt.Sprintf("%v", chunk["done_reason"]), "ollama")
		if state["hadToolCalls"] == true || len(rawToolCalls) > 0 {
			finishReason = "tool_calls"
		}
		delta := map[string]any{}
		if content != "" {
			delta["content"] = content
		}
		if thinking != "" {
			delta["reasoning_content"] = thinking
		}
		if len(rawToolCalls) > 0 {
			delta["tool_calls"] = convertOllamaToolCalls(rawToolCalls)
		}
		final := shared.BuildChunk(id, created, model, delta, finishReason)
		final["usage"] = shared.ToOpenAIUsage(chunk, "ollama")
		return []map[string]any{final}
	}

	if message == nil {
		return nil
	}

	if content == "" && thinking == "" && len(rawToolCalls) == 0 {
		return nil
	}

	delta := map[string]any{}
	if content != "" {
		delta["content"] = content
	}
	if thinking != "" {
		delta["reasoning_content"] = thinking
	}
	if len(rawToolCalls) > 0 {
		state["hadToolCalls"] = true
		delta["tool_calls"] = convertOllamaToolCalls(rawToolCalls)
	}

	return []map[string]any{shared.BuildChunk(id, created, model, delta, nil)}
}

func convertOllamaToolCalls(raw []any) []any {
	out := make([]any, 0, len(raw))
	for i, rawTC := range raw {
		tc, ok := rawTC.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			fn = map[string]any{}
		}
		name, _ := fn["name"].(string)
		args := fn["arguments"]
		argsStr := ""
		switch a := args.(type) {
		case string:
			argsStr = a
		case map[string]any:
			b, _ := json.Marshal(a)
			argsStr = string(b)
		default:
			b, _ := json.Marshal(args)
			argsStr = string(b)
		}
		if argsStr == "" || argsStr == "null" {
			argsStr = "{}"
		}

		tcID, _ := tc["id"].(string)
		if tcID == "" {
			tcID = fallbackOllamaToolCallID(i)
		}
		idx := 0
		if v, ok := fn["index"].(float64); ok {
			idx = int(v)
		} else {
			idx = i
		}

		out = append(out, map[string]any{
			"index": idx,
			"id":    tcID,
			"type":  "function",
			"function": map[string]any{
				"name":      name,
				"arguments": argsStr,
			},
		})
	}
	return out
}

func fallbackOllamaToolCallID(index int) string {
	return fmt.Sprintf("call_%d_%d", index, time.Now().UnixMilli())
}
