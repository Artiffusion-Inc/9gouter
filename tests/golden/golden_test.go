// Package golden ports the Vitest snapshot-based golden contract harness from
// tests/translator/ to Go. It loads .snap fixtures via go:embed, parses them,
// and asserts that the Go translator reproduces each snapshot byte-for-byte
// after the same clean() / stripVolatile() normalization the JS tests apply.
package golden

import (
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/translator"
	_ "github.com/Artiffusion-Inc/9router/internal/adapter/translator/claude"
	_ "github.com/Artiffusion-Inc/9router/internal/adapter/translator/commandcode"
	_ "github.com/Artiffusion-Inc/9router/internal/adapter/translator/gemini"
	_ "github.com/Artiffusion-Inc/9router/internal/adapter/translator/kiro"
	_ "github.com/Artiffusion-Inc/9router/internal/adapter/translator/ollama"
	_ "github.com/Artiffusion-Inc/9router/internal/adapter/translator/openai"
	"github.com/Artiffusion-Inc/9router/internal/adapter/translator/shared"
	"github.com/Artiffusion-Inc/9router/internal/domain/format"
)

//go:embed fixtures/*.snap
var fixtures embed.FS

func TestParseAllSnapshots(t *testing.T) {
	for _, name := range []string{
		"fixtures/golden-request.test.js.snap",
		"fixtures/golden-url-header.test.js.snap",
		"fixtures/golden-response-stream.test.js.snap",
		"fixtures/golden-translator-concerns.test.js.snap",
	} {
		snaps, err := parseSnapFile(fixtures, name)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if len(snaps) == 0 {
			t.Fatalf("no snapshots found in %s", name)
		}
		t.Logf("%s: parsed %d snapshots", name, len(snaps))
	}
}

func TestGoldenRequestOpenAIToClaudeFullBody(t *testing.T) {
	runRequestTest(t, "fixtures/golden-request.test.js.snap",
		"GOLDEN request: OpenAI → Claude > full body (system/image/tool/tool_result)",
		format.Openai, format.Claude, "claude-opus-4-6", baseBody())
}

func TestGoldenRequestOpenAIToClaudeReasoningEffort(t *testing.T) {
	body := map[string]any{
		"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning_effort": "high",
	}
	runRequestTest(t, "fixtures/golden-request.test.js.snap",
		"GOLDEN request: OpenAI → Claude > reasoning_effort → adaptive output_config (claude 4.6+)",
		format.Openai, format.Claude, "claude-opus-4-6", body)
}

func TestGoldenRequestOpenAIToGeminiFullBody(t *testing.T) {
	runRequestTest(t, "fixtures/golden-request.test.js.snap",
		"GOLDEN request: OpenAI → Gemini > full body (system/image/tool/tool_result)",
		format.Openai, format.Gemini, "gemini-3-pro", baseBody())
}

func TestGoldenRequestOpenAIToKiroFullBody(t *testing.T) {
	runRequestTest(t, "fixtures/golden-request.test.js.snap",
		"GOLDEN request: OpenAI → Kiro > full body (image base64 + tool_result)",
		format.Openai, format.Kiro, "claude-sonnet-4.5", baseBody())
}

func TestGoldenResponseStreamClaudeToOpenAI(t *testing.T) {
	events := []json.RawMessage{
		eventJSON(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "model": "claude-opus-4-6"}}),
		eventJSON(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking"}}),
		eventJSON(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "let me think"}}),
		eventJSON(map[string]any{"type": "content_block_stop", "index": 0}),
		eventJSON(map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text"}}),
		eventJSON(map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": "Hello"}}),
		eventJSON(map[string]any{"type": "content_block_stop", "index": 1}),
		eventJSON(map[string]any{"type": "content_block_start", "index": 2, "content_block": map[string]any{"type": "tool_use", "id": "tu_1", "name": "get_weather"}}),
		eventJSON(map[string]any{"type": "content_block_delta", "index": 2, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"city":"NYC"}`}}),
		eventJSON(map[string]any{"type": "content_block_stop", "index": 2}),
		eventJSON(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 3}}),
		eventJSON(map[string]any{"type": "message_stop"}),
	}
	runResponseStreamTest(t, "fixtures/golden-response-stream.test.js.snap",
		"GOLDEN response stream: Claude → OpenAI > text + thinking + tool_use + usage + finish",
		format.Claude, format.Openai, events)
}

func TestGoldenResponseStreamGeminiTextThoughtFunctionCall(t *testing.T) {
	events := []json.RawMessage{
		eventJSON(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{
					"parts": []any{map[string]any{"text": "thinking part", "thought": true}},
				},
			}},
			"responseId":   "resp_1",
			"modelVersion": "gemini-3-pro",
		}),
		eventJSON(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{
					"parts": []any{map[string]any{"text": "Answer"}},
				},
			}},
		}),
		eventJSON(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{
					"parts": []any{map[string]any{
						"functionCall": map[string]any{"name": "search", "args": map[string]any{"q": "x"}},
					}},
				},
			}},
		}),
		eventJSON(map[string]any{
			"candidates": []any{map[string]any{"finishReason": "STOP"}},
			"usageMetadata": map[string]any{
				"promptTokenCount":        8,
				"candidatesTokenCount":    4,
				"thoughtsTokenCount":      2,
				"totalTokenCount":         14,
				"cachedContentTokenCount": 1,
			},
		}),
	}
	runResponseStreamTest(t, "fixtures/golden-response-stream.test.js.snap",
		"GOLDEN response stream: Gemini → OpenAI > text + thought(no-sig) + functionCall + usage + finish",
		format.Gemini, format.Openai, events)
}

func TestGoldenResponseStreamGeminiImageOutput(t *testing.T) {
	events := []json.RawMessage{
		eventJSON(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{
					"parts": []any{map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "BASE64DATA"}}},
				},
			}},
			"responseId":   "resp_2",
			"modelVersion": "gemini-3-flash-image",
		}),
		eventJSON(map[string]any{
			"candidates": []any{map[string]any{"finishReason": "STOP"}},
		}),
	}
	runResponseStreamTest(t, "fixtures/golden-response-stream.test.js.snap",
		"GOLDEN response stream: Gemini → OpenAI > image output (inlineData → delta.images)",
		format.Gemini, format.Openai, events)
}

func TestGoldenResponseStreamKiroToOpenAI(t *testing.T) {
	events := []json.RawMessage{
		eventJSON(map[string]any{"assistantResponseEvent": map[string]any{"content": "Hello"}, "_eventType": "assistantResponseEvent"}),
		eventJSON(map[string]any{"reasoningContentEvent": map[string]any{"text": "thinking"}, "_eventType": "reasoningContentEvent"}),
		eventJSON(map[string]any{"toolUseEvent": map[string]any{"toolUseId": "tu_1", "name": "get_weather", "input": map[string]any{"city": "NYC"}}, "_eventType": "toolUseEvent"}),
		eventJSON(map[string]any{"usageEvent": map[string]any{"inputTokens": 10, "outputTokens": 5}, "_eventType": "usageEvent"}),
		eventJSON(map[string]any{"_eventType": "messageStopEvent"}),
	}
	runResponseStreamTest(t, "fixtures/golden-response-stream.test.js.snap",
		"GOLDEN response stream: Kiro → OpenAI > text + reasoning + toolUse + usage + stop",
		format.Kiro, format.Openai, events)
}

func TestGoldenResponseStreamKiroToOpenAIFinishAfterTool(t *testing.T) {
	events := []json.RawMessage{
		eventJSON(map[string]any{"assistantResponseEvent": map[string]any{"content": "Hi"}, "_eventType": "assistantResponseEvent"}),
		eventJSON(map[string]any{"toolUseEvent": map[string]any{"toolUseId": "tu_1", "name": "search", "input": map[string]any{"q": "x"}}, "_eventType": "toolUseEvent"}),
		eventJSON(map[string]any{"usageEvent": map[string]any{"inputTokens": 7, "outputTokens": 3}, "_eventType": "usageEvent"}),
		eventJSON(map[string]any{"_eventType": "messageStopEvent"}),
	}
	runResponseStreamTest(t, "fixtures/golden-translator-concerns.test.js.snap",
		"GOLDEN response stream: Kiro → OpenAI (finish after tool) > toolUse then stop — lock current finish_reason behavior",
		format.Kiro, format.Openai, events)
}

func TestGoldenResponseStreamOllamaToOpenAI(t *testing.T) {
	events := []json.RawMessage{
		eventJSON(map[string]any{"model": "qwen3", "message": map[string]any{"role": "assistant", "content": "Hi", "thinking": "reason"}}),
		eventJSON(map[string]any{
			"model": "qwen3",
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{"function": map[string]any{"name": "search", "arguments": map[string]any{"q": "x"}}}},
			},
		}),
		eventJSON(map[string]any{"model": "qwen3", "done": true, "done_reason": "stop", "prompt_eval_count": 8, "eval_count": 4}),
	}
	runResponseStreamTest(t, "fixtures/golden-response-stream.test.js.snap",
		"GOLDEN response stream: Ollama → OpenAI > content + thinking + tool_calls + done usage",
		format.Ollama, format.Openai, events)
}

func TestGoldenResponseStreamOllamaToOpenAIFinishAfterTool(t *testing.T) {
	events := []json.RawMessage{
		eventJSON(map[string]any{
			"model": "qwen3",
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{"function": map[string]any{"name": "search", "arguments": map[string]any{"q": "x"}}}},
			},
		}),
		eventJSON(map[string]any{"model": "qwen3", "done": true, "done_reason": "stop", "prompt_eval_count": 5, "eval_count": 2}),
	}
	runResponseStreamTest(t, "fixtures/golden-translator-concerns.test.js.snap",
		"GOLDEN response stream: Ollama → OpenAI (finish after tool) > tool_calls then done_reason=stop — lock current finish_reason",
		format.Ollama, format.Openai, events)
}

func TestGoldenResponseStreamCommandCodeToOpenAI(t *testing.T) {
	events := []json.RawMessage{
		eventJSON(map[string]any{"type": "text-delta", "text": "Hello"}),
		eventJSON(map[string]any{"type": "reasoning-delta", "text": "thinking"}),
		eventJSON(map[string]any{"type": "tool-input-start", "id": "t1", "toolName": "get_weather"}),
		eventJSON(map[string]any{"type": "tool-input-delta", "id": "t1", "delta": `{"city":"NYC"}`}),
		eventJSON(map[string]any{"type": "finish-step", "finishReason": "tool-calls", "usage": map[string]any{"inputTokens": 10, "outputTokens": 5, "totalTokens": 15}}),
		eventJSON(map[string]any{"type": "finish"}),
	}
	runResponseStreamTest(t, "fixtures/golden-translator-concerns.test.js.snap",
		"GOLDEN response stream: CommandCode → OpenAI > text + reasoning + tool + finish-step usage",
		format.Commandcode, format.Openai, events)
}

func TestGoldenResponseStreamOpenAIResponsesCodexToOpenAI(t *testing.T) {
	events := []json.RawMessage{
		eventJSON(map[string]any{"type": "response.output_text.delta", "data": map[string]any{"delta": "Hello"}}),
		eventJSON(map[string]any{"type": "response.reasoning_summary_text.delta", "data": map[string]any{"delta": "thinking"}}),
		eventJSON(map[string]any{"type": "response.output_item.added", "data": map[string]any{"item": map[string]any{"type": "function_call", "call_id": "call_1", "name": "get_weather"}}}),
		eventJSON(map[string]any{"type": "response.function_call_arguments.delta", "data": map[string]any{"delta": `{"city":"NYC"}`}}),
		eventJSON(map[string]any{"type": "response.output_item.done", "data": map[string]any{"item": map[string]any{"type": "function_call", "call_id": "call_1"}}}),
		eventJSON(map[string]any{
			"type": "response.completed",
			"data": map[string]any{
				"response": map[string]any{
					"usage": map[string]any{
						"input_tokens":         10,
						"output_tokens":        5,
						"input_tokens_details": map[string]any{"cached_tokens": 3},
					},
				},
			},
		}),
	}
	runResponseStreamTest(t, "fixtures/golden-response-stream.test.js.snap",
		"GOLDEN response stream: OpenAI-Responses (codex) → OpenAI > text + reasoning + tool_call + completed usage",
		format.OpenaiResponses, format.Openai, events)
}

func TestGoldenResponseStreamOpenAIResponsesCodexError(t *testing.T) {
	events := []json.RawMessage{
		eventJSON(map[string]any{"type": "error", "data": map[string]any{"error": map[string]any{"message": "model_not_found"}}}),
	}
	runResponseStreamTest(t, "fixtures/golden-response-stream.test.js.snap",
		"GOLDEN response stream: OpenAI-Responses (codex) → OpenAI > error event → error chunk (fallback id/created)",
		format.OpenaiResponses, format.Openai, events)
}

func TestGoldenUsageMathClaudePromptCache(t *testing.T) {
	events := []json.RawMessage{
		eventJSON(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "model": "claude-opus-4-6"}}),
		eventJSON(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text"}}),
		eventJSON(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "ok"}}),
		eventJSON(map[string]any{"type": "content_block_stop", "index": 0}),
		eventJSON(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 2}}),
		eventJSON(map[string]any{"type": "message_stop"}),
	}
	got := runResponseStream(format.Claude, format.Openai, events)
	final, _ := got[len(got)-1].(map[string]any)
	usage, _ := final["usage"].(map[string]any)
	if shared.Number(usage["prompt_tokens"]) != 15 {
		t.Errorf("prompt_tokens mismatch: got %v, want 15", usage["prompt_tokens"])
	}
	if shared.Number(usage["completion_tokens"]) != 5 {
		t.Errorf("completion_tokens mismatch: got %v, want 5", usage["completion_tokens"])
	}

	key := "GOLDEN usage math: Claude prompt = input + cache (lock) > prompt_tokens sums input + cache_read + cache_creation"
	want := loadSnapshot(t, "fixtures/golden-translator-concerns.test.js.snap", key)
	compareSnapshot(t, want, snapshotString(usage))
}

// baseBody returns the OpenAI request body used by the JS golden-request.test.js.
func baseBody() map[string]any {
	return map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful."},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "What's in this image?"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,IMGDATA", "detail": "high"}},
			}},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"NYC"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "get_weather", "description": "Get weather", "parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []any{"city"}}}},
		},
		"temperature": 0.7,
	}
}

func runRequestTest(t *testing.T, file, key string, sourceFormat, targetFormat format.Format, model string, body map[string]any) {
	t.Helper()
	want := loadSnapshot(t, file, key)

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	gotBytes, err := translator.TranslateRequest(sourceFormat, targetFormat, model, raw, true, "")
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	got, err := clean(gotBytes)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	compareSnapshot(t, want, got)
}

func runResponseStreamTest(t *testing.T, file, key string, targetFormat, sourceFormat format.Format, events []json.RawMessage) {
	t.Helper()
	want := loadSnapshot(t, file, key)
	got := runResponseStream(targetFormat, sourceFormat, events)
	compareSnapshot(t, want, snapshotString(stripVolatile(got)))
}

func runResponseStream(targetFormat, sourceFormat format.Format, events []json.RawMessage) []any {
	state := translator.InitState(sourceFormat)
	all := []any{}
	for _, ev := range events {
		out, err := translator.TranslateResponse(targetFormat, sourceFormat, ev, state)
		if err != nil {
			// Panic with details so the test fails clearly.
			panic(fmt.Sprintf("translate response %s->%s: %v", targetFormat, sourceFormat, err))
		}
		if out == nil {
			continue
		}
		for _, b := range out {
			var c map[string]any
			if err := json.Unmarshal(b, &c); err == nil {
				all = append(all, c)
			} else {
				all = append(all, string(b))
			}
		}
	}
	return all
}

func eventJSON(v map[string]any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func loadSnapshot(t *testing.T, file, key string) string {
	t.Helper()
	snaps, err := parseSnapFile(fixtures, file)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	want, ok := snaps[key+" 1"]
	if !ok {
		t.Fatalf("snapshot %q not found in %s; have keys: %v", key, file, keys(snaps))
	}
	return want
}

func compareSnapshot(t *testing.T, want, got string) {
	t.Helper()
	if got != want {
		t.Errorf("snapshot mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// clean normalizes a request body exactly like golden-request.test.js clean():
//   - drop _toolNameMap, conversationId, agentContinuationId keys at any level
//   - replace "Current time is <...>" with "Current time is <TS>"
//
// It returns a snapshot-string matching the Vitest .snap serialization.
func clean(raw []byte) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	v = stripKeys(v, map[string]struct{}{
		"_toolNameMap":        {},
		"conversationId":      {},
		"agentContinuationId": {},
	})
	s := snapshotString(v)
	re := regexp.MustCompile(`Current time is [^"\\\n]+`)
	s = string(re.ReplaceAll([]byte(s), []byte("Current time is <TS>")))
	return s, nil
}

func stripKeys(v any, drop map[string]struct{}) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v2 := range x {
			if _, ok := drop[k]; ok {
				continue
			}
			out[k] = stripKeys(v2, drop)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v2 := range x {
			out[i] = stripKeys(v2, drop)
		}
		return out
	default:
		return x
	}
}

// stripVolatile normalizes dynamic fields (created timestamps and ids) so that
// response-stream snapshots are stable across runs, matching the JS helper.
func stripVolatile(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v2 := range x {
			if k == "created" {
				out[k] = 0
				continue
			}
			if k == "id" {
				if s, ok := v2.(string); ok {
					out[k] = normalizeVolatileID(s)
					continue
				}
			}
			out[k] = stripVolatile(v2)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v2 := range x {
			out[i] = stripVolatile(v2)
		}
		return out
	default:
		return x
	}
}

var (
	geminiToolIDRe   = regexp.MustCompile(`^([^-]+)-\d{10,}-(\d+)$`)
	chatcmplIDRe     = regexp.MustCompile(`^chatcmpl-\d{10,}$`)
	ollamaToolIDRe1  = regexp.MustCompile(`^call_(\d+)_\d{10,}$`)
	ollamaToolIDRe2  = regexp.MustCompile(`^call_\d{10,}_(\d+)$`)
)

func normalizeVolatileID(s string) string {
	if m := geminiToolIDRe.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("%s-<TS>-%s", m[1], m[2])
	}
	if chatcmplIDRe.MatchString(s) {
		return "chatcmpl-<TS>"
	}
	if m := ollamaToolIDRe1.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("call_%s_<TS>", m[1])
	}
	if m := ollamaToolIDRe2.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("call_<TS>_%s", m[1])
	}
	return s
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
