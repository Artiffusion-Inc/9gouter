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
	"sort"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/claude"
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/commandcode"
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/gemini"
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/kiro"
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/ollama"
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/openai"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/shared"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
	domain "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
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
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
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
				"role":       "assistant",
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
				"role":       "assistant",
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

func TestGoldenURLHeaderDefaultProviders(t *testing.T) {
	snaps, err := parseSnapFile(fixtures, "fixtures/golden-url-header.test.js.snap")
	if err != nil {
		t.Fatalf("parse url-header snapshot: %v", err)
	}

	urlRe := regexp.MustCompile(`GOLDEN buildUrl \(default executor providers\) > ([^ ]+) → url \(stream \+ non-stream\)`)
	hdrRe := regexp.MustCompile(`GOLDEN buildHeaders \(default executor providers\) > ([^ ]+) → headers \(apiKey / oauth\)`)

	urlPids := map[string]bool{}
	hdrPids := map[string]bool{}
	for key := range snaps {
		if m := urlRe.FindStringSubmatch(key); m != nil {
			urlPids[m[1]] = true
		}
		if m := hdrRe.FindStringSubmatch(key); m != nil {
			hdrPids[m[1]] = true
		}
	}

	for pid := range urlPids {
		hdrPids[pid] = true
	}
	pids := make([]string, 0, len(hdrPids))
	for pid := range hdrPids {
		pids = append(pids, pid)
	}
	sort.Strings(pids)

	for _, pid := range pids {
		t.Run(pid, func(t *testing.T) {
			p, err := provider.Lookup(pid)
			if err != nil {
				t.Fatalf("lookup %s: %v", pid, err)
			}
			exec := p.Executor()
			cred := specialCred
			noAuth := false
			if _, ok := noAuthProviders[pid]; ok {
				cred = domain.Credentials{}
				noAuth = true
			}

			// URL snapshot.
			urlKey := fmt.Sprintf("GOLDEN buildUrl (default executor providers) > %s → url (stream + non-stream)", pid)
			wantURL, ok := snaps[urlKey+" 1"]
			if !ok {
				t.Fatalf("url snapshot not found for %s", pid)
			}
			gotURL := map[string]any{
				"stream":    safeString(func() string { return exec.BuildURL("test-model", true, 0, cred) }),
				"nonStream": safeString(func() string { return exec.BuildURL("test-model", false, 0, cred) }),
			}
			compareSnapshot(t, wantURL, snapshotString(gotURL))

			// Header snapshot.
			hdrKey := fmt.Sprintf("GOLDEN buildHeaders (default executor providers) > %s → headers (apiKey / oauth)", pid)
			wantHdr, ok := snaps[hdrKey+" 1"]
			if !ok {
				t.Fatalf("header snapshot not found for %s", pid)
			}
			apiKeyC := cred
			oauthC := oauthCred
			if noAuth {
				apiKeyC = domain.Credentials{}
				oauthC = domain.Credentials{}
			}
			gotHdr := map[string]any{
				"apiKey":    sanitizeHeaders(exec.BuildHeaders(apiKeyC, true)),
				"oauth":     sanitizeHeaders(exec.BuildHeaders(oauthC, true)),
				"nonStream": sanitizeHeaders(exec.BuildHeaders(apiKeyC, false)),
			}
			compareSnapshot(t, wantHdr, snapshotString(gotHdr))
		})
	}
}

var apiKeyCred = domain.Credentials{
	APIKey:               "sk-test-APIKEY",
	ProviderSpecificData: map[string]any{},
}

var oauthCred = domain.Credentials{
	AccessToken:          "tok-test-ACCESS",
	ProviderSpecificData: map[string]any{},
}

var specialCred = domain.Credentials{
	APIKey:      "sk-test-APIKEY",
	AccessToken: "tok-test-ACCESS",
	ProviderSpecificData: map[string]any{
		"accountId": "ACC123",
		"region":    "sgp",
		"baseUrl":   "https://custom.example.com/v1",
		"orgId":     "ORG9",
	},
}

var noAuthProviders = map[string]struct{}{
	"mmf": {},
}

func safeString(fn func() string) string {
	defer func() {
		if r := recover(); r != nil {
			// keep THROW string below
		}
	}()
	return fn()
}

var (
	bearerRe = regexp.MustCompile(`Bearer .+`)
	kimiTsRe = regexp.MustCompile(`kimi-\d{10,}`)
)

func sanitizeHeaders(h map[string][]string) map[string]any {
	out := make(map[string]any, len(h))
	for k, vv := range h {
		if len(vv) == 0 {
			continue
		}
		v := vv[0]
		v = bearerRe.ReplaceAllString(v, "Bearer <TOK>")
		v = strings.ReplaceAll(v, "sk-test-APIKEY", "<CRED>")
		v = strings.ReplaceAll(v, "tok-test-ACCESS", "<CRED>")
		v = kimiTsRe.ReplaceAllString(v, "kimi-<TS>")
		if k == "X-Msh-Device-Model" {
			v = "<OS>"
		}
		if k == "X-Msh-Device-Name" {
			// hostname is machine-specific (upstream buildKimiHeaders reads
			// os.hostname()); collapse to a stable placeholder.
			v = "<HOST>"
		}
		out[k] = v
	}
	return out
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
	geminiToolIDRe  = regexp.MustCompile(`^([^-]+)-\d{10,}-(\d+)$`)
	chatcmplIDRe    = regexp.MustCompile(`^chatcmpl-\d{10,}$`)
	ollamaToolIDRe1 = regexp.MustCompile(`^call_(\d+)_\d{10,}$`)
	ollamaToolIDRe2 = regexp.MustCompile(`^call_\d{10,}_(\d+)$`)
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

// TestGoldenURLHeaderAll25Providers covers the providers not present in the
// Vitest url/header snapshot. It asserts exact URL and sanitized headers for
// each ported executor.
func TestGoldenURLHeaderAll25Providers(t *testing.T) {
	tests := []struct {
		pid           string
		creds         domain.Credentials
		wantURLStream string
		wantURLNon    string
		wantHeaders   map[string]any // apiKey path only; oauth path inferred by replacing key header if needed
		oauthHeaders  map[string]any // optional overrides for oauth path
	}{
		{
			pid:           "antigravity",
			creds:         oauthCred,
			wantURLStream: "https://cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse",
			wantURLNon:    "https://cloudcode-pa.googleapis.com/v1internal:generateContent",
			wantHeaders: map[string]any{
				"Accept":        "text/event-stream",
				"Authorization": "Bearer <TOK>",
				"Content-Type":  "application/json",
				"User-Agent":    "antigravity/ide/2.1.1 darwin/arm64",
			},
		},
		{
			pid:           "azure",
			creds:         specialCred,
			wantURLStream: "https://api.openai.com/openai/deployments/test-model/chat/completions?api-version=2024-10-01-preview",
			wantURLNon:    "https://api.openai.com/openai/deployments/test-model/chat/completions?api-version=2024-10-01-preview",
			wantHeaders: map[string]any{
				"Accept":       "text/event-stream",
				"api-key":      "<CRED>",
				"Content-Type": "application/json",
			},
		},
		{
			pid:           "codex",
			creds:         apiKeyCred,
			wantURLStream: "https://chatgpt.com/backend-api/codex/responses",
			wantURLNon:    "https://chatgpt.com/backend-api/codex/responses",
			wantHeaders: map[string]any{
				"Accept":        "text/event-stream",
				"Authorization": "Bearer <TOK>",
				"Content-Type":  "application/json",
				"originator":    "codex_cli_rs",
				"session_id":    "<SESSION>",
				"User-Agent":    "codex_cli_rs/0.136.0",
			},
		},
		{
			pid:           "commandcode",
			creds:         apiKeyCred,
			wantURLStream: "https://api.commandcode.ai/alpha/generate",
			wantURLNon:    "https://api.commandcode.ai/alpha/generate",
			wantHeaders: map[string]any{
				"Accept":                 "text/event-stream",
				"Authorization":          "Bearer <TOK>",
				"Content-Type":           "application/json",
				"x-cli-environment":      "cli",
				"x-command-code-version": "0.25.7",
				"x-session-id":           "<UUID>",
			},
		},
		{
			pid: "cursor",
			creds: domain.Credentials{
				APIKey:               "sk-test-APIKEY",
				AccessToken:          "tok-test-ACCESS",
				ProviderSpecificData: map[string]any{"machineId": "MACHINE123"},
			},
			wantURLStream: "https://api2.cursor.sh/aiserver.v1.ChatService/StreamUnifiedChatWithTools",
			wantURLNon:    "https://api2.cursor.sh/aiserver.v1.ChatService/StreamUnifiedChatWithTools",
			wantHeaders: map[string]any{
				"Accept":                   "text/event-stream",
				"Authorization":            "Bearer <TOK>",
				"connect-accept-encoding":  "gzip",
				"connect-protocol-version": "1",
				"Content-Type":             "application/connect+proto",
				"User-Agent":               "connect-es/1.6.1",
				"x-cursor-client-type":     "ide",
				"x-cursor-client-version":  "3.1.0",
				"x-machine-id":             "<MACHINE>",
			},
		},
		{
			pid:           "gemini-cli",
			creds:         oauthCred,
			wantURLStream: "https://cloudcode-pa.googleapis.com/v1internal/models/test-model:streamGenerateContent?alt=sse",
			wantURLNon:    "https://cloudcode-pa.googleapis.com/v1internal/models/test-model:generateContent",
			wantHeaders: map[string]any{
				"Accept":            "text/event-stream",
				"Authorization":     "Bearer <TOK>",
				"Content-Type":      "application/json",
				"X-Goog-Api-Client": "google-genai-sdk/1.41.0 gl-node/v22.19.0",
			},
		},
		{
			pid:           "github",
			creds:         oauthCred,
			wantURLStream: "https://api.githubcopilot.com/chat/completions",
			wantURLNon:    "https://api.githubcopilot.com/chat/completions",
			wantHeaders: map[string]any{
				"Accept":                              "text/event-stream",
				"anthropic-version":                   "2023-06-01",
				"Authorization":                       "Bearer <TOK>",
				"Content-Type":                        "application/json",
				"copilot-integration-id":              "vscode-chat",
				"editor-plugin-version":               "copilot-chat/0.38.0",
				"editor-version":                      "vscode/1.110.0",
				"openai-intent":                       "conversation-panel",
				"user-agent":                          "GitHubCopilotChat/0.38.0",
				"x-github-api-version":                "2025-04-01",
				"x-request-id":                        "<UUID>",
				"x-vscode-user-agent-library-version": "electron-fetch",
				"X-Initiator":                         "user",
			},
		},
		{
			pid:           "grok-cli",
			creds:         oauthCred,
			wantURLStream: "https://cli-chat-proxy.grok.com/v1/responses",
			wantURLNon:    "https://cli-chat-proxy.grok.com/v1/responses",
			wantHeaders: map[string]any{
				"Accept":                   "text/event-stream",
				"Authorization":            "Bearer <TOK>",
				"Content-Type":             "application/json",
				"User-Agent":               "grok-shell/0.2.99 (linux; x86_64)",
				"x-grok-client-identifier": "grok-shell",
				"x-grok-client-version":    "0.2.99",
			},
		},
		{
			pid:           "grok-web",
			creds:         domain.Credentials{},
			wantURLStream: "https://grok.com/rest/app-chat/conversations/new",
			wantURLNon:    "https://grok.com/rest/app-chat/conversations/new",
			wantHeaders: map[string]any{
				"Accept":             "*/*",
				"Accept-Encoding":    "gzip, deflate, br, zstd",
				"Accept-Language":    "en-US,en;q=0.9",
				"Cache-Control":      "no-cache",
				"Content-Type":       "application/json",
				"Origin":             "https://grok.com",
				"Pragma":             "no-cache",
				"Referer":            "https://grok.com/",
				"Sec-Ch-Ua":          `"Google Chrome";v="136", "Chromium";v="136", "Not(A:Brand";v="24"`,
				"Sec-Ch-Ua-Mobile":   "?0",
				"Sec-Ch-Ua-Platform": `"macOS"`,
				"Sec-Fetch-Dest":     "empty",
				"Sec-Fetch-Mode":     "cors",
				"Sec-Fetch-Site":     "same-origin",
				"traceparent":        "00-<TRACE>-00",
				"User-Agent":         "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
				"x-statsig-id":       "<SIG>",
				"x-xai-request-id":   "<REQ>",
			},
		},
		{
			pid:           "iflow",
			creds:         apiKeyCred,
			wantURLStream: "https://apis.iflow.cn/v1/chat/completions",
			wantURLNon:    "https://apis.iflow.cn/v1/chat/completions",
			wantHeaders: map[string]any{
				"Accept":            "text/event-stream",
				"Authorization":     "Bearer <TOK>",
				"Content-Type":      "application/json",
				"session-id":        "<SESSION>",
				"User-Agent":        "iFlow-Cli",
				"x-iflow-signature": "<SIG>",
				"x-iflow-timestamp": "<TS>",
			},
		},
		{
			pid:           "kiro",
			creds:         oauthCred,
			wantURLStream: "https://runtime.us-east-1.kiro.dev/generateAssistantResponse",
			wantURLNon:    "https://runtime.us-east-1.kiro.dev/generateAssistantResponse",
			wantHeaders: map[string]any{
				"Accept":                "application/vnd.amazon.eventstream",
				"Amz-Sdk-Invocation-Id": "<UUID>",
				"Amz-Sdk-Request":       "attempt=1; max=3",
				"Authorization":         "Bearer <TOK>",
				"Content-Type":          "application/json",
				"User-Agent":            "AWS-SDK-JS/3.0.0 kiro-ide/1.0.0",
				"X-Amz-Target":          "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
				"X-Amz-User-Agent":      "aws-sdk-js/3.0.0 kiro-ide/1.0.0",
			},
		},
		{
			pid:           "mimo-free",
			creds:         domain.Credentials{},
			wantURLStream: "https://api.xiaomimimo.com/api/free-ai/openai/chat",
			wantURLNon:    "https://api.xiaomimimo.com/api/free-ai/openai/chat",
			wantHeaders: map[string]any{
				"Accept":             "text/event-stream",
				"Content-Type":       "application/json",
				"User-Agent":         "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
				"X-Mimo-Source":      "mimocode-cli-free",
				"x-session-affinity": "<SESSION>",
			},
		},
		{
			pid:           "ollama-local",
			creds:         domain.Credentials{},
			wantURLStream: "http://localhost:11434/api/chat",
			wantURLNon:    "http://localhost:11434/api/chat",
			wantHeaders: map[string]any{
				"Accept":        "text/event-stream",
				"Authorization": "Bearer <TOK>",
				"Content-Type":  "application/json",
			},
		},
		{
			pid:           "opencode",
			creds:         domain.Credentials{},
			wantURLStream: "https://opencode.ai/zen/v1/chat/completions",
			wantURLNon:    "https://opencode.ai/zen/v1/chat/completions",
			wantHeaders: map[string]any{
				"Accept":            "text/event-stream",
				"Authorization":     "Bearer <TOK>",
				"Content-Type":      "application/json",
				"x-opencode-client": "desktop",
			},
		},
		{
			pid:           "opencode-go",
			creds:         apiKeyCred,
			wantURLStream: "https://opencode.ai/zen/go/v1/chat/completions",
			wantURLNon:    "https://opencode.ai/zen/go/v1/chat/completions",
			wantHeaders: map[string]any{
				"Accept":        "text/event-stream",
				"Authorization": "Bearer <TOK>",
				"Content-Type":  "application/json",
			},
		},
		{
			pid:           "perplexity-web",
			creds:         apiKeyCred,
			wantURLStream: "https://www.perplexity.ai/rest/sse/perplexity_ask",
			wantURLNon:    "https://www.perplexity.ai/rest/sse/perplexity_ask",
			wantHeaders: map[string]any{
				"Accept":           "text/event-stream",
				"Content-Type":     "application/json",
				"Cookie":           "__Secure-next-auth.session-token=<CRED>",
				"Origin":           "https://www.perplexity.ai",
				"Referer":          "https://www.perplexity.ai/",
				"User-Agent":       "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
				"X-App-ApiClient":  "default",
				"X-App-ApiVersion": "2.18",
			},
			oauthHeaders: map[string]any{
				"Accept":           "text/event-stream",
				"Authorization":    "Bearer <TOK>",
				"Content-Type":     "application/json",
				"Origin":           "https://www.perplexity.ai",
				"Referer":          "https://www.perplexity.ai/",
				"User-Agent":       "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
				"X-App-ApiClient":  "default",
				"X-App-ApiVersion": "2.18",
			},
		},
		{
			pid:           "qoder",
			creds:         apiKeyCred,
			wantURLStream: "https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation",
			wantURLNon:    "https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation",
			wantHeaders: map[string]any{
				"Accept":           "text/event-stream",
				"Authorization":    "Bearer <TOK>",
				"Content-Type":     "application/json",
				"X-Cosy-Signature": "<SIG>",
				"X-Qoder-Client":   "qodercli",
				"X-Qoder-Version":  "3",
			},
		},
		{
			pid:           "qwen",
			creds:         apiKeyCred,
			wantURLStream: "https://portal.qwen.ai/v1/chat/completions",
			wantURLNon:    "https://portal.qwen.ai/v1/chat/completions",
			wantHeaders: map[string]any{
				"Accept":                      "text/event-stream",
				"Accept-Language":             "*",
				"Authorization":               "Bearer <TOK>",
				"Connection":                  "keep-alive",
				"Content-Type":                "application/json",
				"Sec-Fetch-Mode":              "cors",
				"User-Agent":                  "QwenCode/0.12.3 (linux; x64)",
				"X-DashScope-AuthType":        "qwen-oauth",
				"X-DashScope-CacheControl":    "enable",
				"X-DashScope-UserAgent":       "QwenCode/0.12.3 (linux; x64)",
				"X-Stainless-Arch":            "x64",
				"X-Stainless-Lang":            "js",
				"X-Stainless-Os":              "Linux",
				"X-Stainless-Package-Version": "5.11.0",
				"X-Stainless-Retry-Count":     "1",
				"X-Stainless-Runtime":         "node",
				"X-Stainless-Runtime-Version": "v18.19.1",
			},
		},
		{
			pid:           "vertex",
			creds:         apiKeyCred,
			wantURLStream: "https://aiplatform.googleapis.com/v1/publishers/google/models/test-model:streamGenerateContent?alt=sse&key=sk-test-APIKEY",
			wantURLNon:    "https://aiplatform.googleapis.com/v1/publishers/google/models/test-model:generateContent?key=sk-test-APIKEY",
			wantHeaders: map[string]any{
				"Accept":       "text/event-stream",
				"Content-Type": "application/json",
			},
		},
		{
			pid:           "xiaomi-tokenplan",
			creds:         apiKeyCred,
			wantURLStream: "https://token-plan-sgp.xiaomimimo.com/v1/chat/completions",
			wantURLNon:    "https://token-plan-sgp.xiaomimimo.com/v1/chat/completions",
			wantHeaders: map[string]any{
				"Accept":        "text/event-stream",
				"Authorization": "Bearer <TOK>",
				"Content-Type":  "application/json",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.pid, func(t *testing.T) {
			p, err := provider.Lookup(tc.pid)
			if err != nil {
				t.Fatalf("lookup %s: %v", tc.pid, err)
			}
			exec := p.Executor()
			gotURLStream := safeString(func() string { return exec.BuildURL("test-model", true, 0, tc.creds) })
			gotURLNon := safeString(func() string { return exec.BuildURL("test-model", false, 0, tc.creds) })
			if gotURLStream != tc.wantURLStream {
				t.Errorf("stream url mismatch\n got: %s\nwant: %s", gotURLStream, tc.wantURLStream)
			}
			if gotURLNon != tc.wantURLNon {
				t.Errorf("non-stream url mismatch\n got: %s\nwant: %s", gotURLNon, tc.wantURLNon)
			}

			apiHdr := sanitizeHeaders(exec.BuildHeaders(tc.creds, true))
			apiHdr = sanitizeDynamicHeaders(apiHdr)
			if diff := cmpHeaders(tc.wantHeaders, apiHdr); diff != "" {
				t.Errorf("apiKey headers mismatch (-want +got):\n%s", diff)
			}

			oauthC := oauthCred
			if tc.oauthHeaders != nil {
				oauthHdr := sanitizeHeaders(exec.BuildHeaders(oauthC, true))
				oauthHdr = sanitizeDynamicHeaders(oauthHdr)
				if diff := cmpHeaders(tc.oauthHeaders, oauthHdr); diff != "" {
					t.Errorf("oauth headers mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func sanitizeDynamicHeaders(h map[string]any) map[string]any {
	uuidRe := regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)
	hexRe := regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	sigRe := regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	out := make(map[string]any, len(h))
	for k, v := range h {
		s, ok := v.(string)
		if !ok {
			out[k] = v
			continue
		}
		switch k {
		case "x-session-id", "session-id", "x-session-affinity":
			if strings.HasPrefix(s, "session-") {
				out[k] = "<SESSION>"
			} else {
				out[k] = "<UUID>"
			}
		case "x-grok-req-id", "x-xai-request-id":
			out[k] = "<REQ>"
		case "Amz-Sdk-Invocation-Id":
			out[k] = "<UUID>"
		case "x-request-id":
			out[k] = "<UUID>"
		case "x-grok-session-id", "x-grok-conv-id", "session_id":
			out[k] = "<SESSION>"
		case "x-iflow-timestamp":
			out[k] = "<TS>"
		case "x-iflow-signature", "X-Cosy-Signature":
			out[k] = "<SIG>"
		case "x-statsig-id":
			out[k] = "<SIG>"
		case "traceparent":
			out[k] = "00-<TRACE>-00"
		case "x-machine-id":
			out[k] = "<MACHINE>"
		default:
			if uuidRe.MatchString(s) {
				out[k] = "<UUID>"
			} else if sigRe.MatchString(s) {
				out[k] = "<SIG>"
			} else if hexRe.MatchString(s) {
				out[k] = "<HEX>"
			} else {
				out[k] = v
			}
		}
	}
	return out
}

func cmpHeaders(want, got map[string]any) string {
	for k, v := range want {
		if got[k] != v {
			return fmt.Sprintf("%s: got %q want %q", k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			return fmt.Sprintf("extra key %s: %q", k, got[k])
		}
	}
	return ""
}
