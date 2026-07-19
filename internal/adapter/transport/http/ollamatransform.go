package http

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// ollamaNDJSONConverter ports open-sse/utils/ollamaTransform.js: it consumes an
// OpenAI SSE byte stream and emits Ollama NDJSON lines:
//
//	content chunk  -> {"model","message":{"role":"assistant","content":<delta>},"done":false}\n
//	tool_calls end -> {"model","message":{"role":"assistant","content":"","tool_calls":[...]},"done":true}\n
//	stop / [DONE] -> {"model","message":{"role":"assistant","content":""},"done":true}\n
//
// tool_calls are accumulated across chunks by index (name + arguments are
// concatenated as strings), then emitted once on finish_reason=="tool_calls".
// The [DONE] sentinel and EOF both emit the final done:true line — guarded by
// sentinelEmitted so we never emit it twice (this fixes a latent JS bug where
// flush() could double-emit after [DONE]).
type ollamaNDJSONConverter struct {
	model            string
	pendingToolCalls map[int]*pendingToolCall
	sentinelEmitted  bool
}

type pendingToolCall struct {
	id string
	fn struct {
		name      string
		arguments string
	}
}

func newOllamaNDJSONConverter(model string) *ollamaNDJSONConverter {
	if model == "" {
		model = "llama3.2"
	}
	return &ollamaNDJSONConverter{
		model:            model,
		pendingToolCalls: make(map[int]*pendingToolCall),
	}
}

// Convert reads an OpenAI SSE stream from r and writes Ollama NDJSON to w. It
// returns when the input is exhausted. Lines that are not "data:"-prefixed are
// skipped; malformed JSON in a data line is ignored (matching the JS silent
// catch), though the JS code swallows errors entirely — here we keep going.
func (c *ollamaNDJSONConverter) Convert(w io.Writer, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	// SSE chunks can be large (image/tool_call deltas); allow generous buffers.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			c.emitDone(w)
			return scanner.Err()
		}
		var parsed struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			// Silently ignore parse errors, matching the JS catch.
			continue
		}
		if len(parsed.Choices) == 0 {
			continue
		}
		ch := parsed.Choices[0]
		for _, tc := range ch.Delta.ToolCalls {
			p, ok := c.pendingToolCalls[tc.Index]
			if !ok {
				p = &pendingToolCall{id: tc.ID}
				c.pendingToolCalls[tc.Index] = p
			}
			if tc.Function.Name != "" {
				p.fn.name += tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				p.fn.arguments += tc.Function.Arguments
			}
		}
		if ch.Delta.Content != "" {
			c.emitContent(w, ch.Delta.Content)
		}
		switch ch.FinishReason {
		case "tool_calls":
			c.emitToolCallsDone(w)
		case "stop":
			c.emitDone(w)
		}
	}
	// EOF: emit the trailing done:true unless we already did (e.g. via [DONE]).
	c.emitDone(w)
	return scanner.Err()
}

func (c *ollamaNDJSONConverter) emitContent(w io.Writer, content string) {
	line, _ := json.Marshal(map[string]any{
		"model":   c.model,
		"message": map[string]any{"role": "assistant", "content": content},
		"done":    false,
	})
	_, _ = w.Write(line)
	_, _ = w.Write([]byte("\n"))
}

func (c *ollamaNDJSONConverter) emitDone(w io.Writer) {
	if c.sentinelEmitted {
		return
	}
	c.sentinelEmitted = true
	line, _ := json.Marshal(map[string]any{
		"model":   c.model,
		"message": map[string]any{"role": "assistant", "content": ""},
		"done":    true,
	})
	_, _ = w.Write(line)
	_, _ = w.Write([]byte("\n"))
}

func (c *ollamaNDJSONConverter) emitToolCallsDone(w io.Writer) {
	c.sentinelEmitted = true
	calls := make([]map[string]any, 0, len(c.pendingToolCalls))
	for _, p := range c.pendingToolCalls {
		var args any
		if err := json.Unmarshal([]byte(p.fn.arguments), &args); err != nil {
			args = map[string]any{}
		}
		calls = append(calls, map[string]any{
			"function": map[string]any{
				"name":      p.fn.name,
				"arguments": args,
			},
		})
	}
	c.pendingToolCalls = make(map[int]*pendingToolCall)
	line, _ := json.Marshal(map[string]any{
		"model":   c.model,
		"message": map[string]any{
			"role":       "assistant",
			"content":    "",
			"tool_calls": calls,
		},
		"done": true,
	})
	_, _ = w.Write(line)
	_, _ = w.Write([]byte("\n"))
}