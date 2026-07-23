package proxychat

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httptransport "github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/http"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

// TestStripEmptyToolCalls ports upstream 602ee405: an empty "tool_calls":[] in
// a streaming delta must be stripped so @ai-sdk/openai-compatible does not
// treat it as a tool-call signal. Non-empty arrays are preserved.
func TestStripEmptyToolCalls(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want map[string]any
	}{
		{
			"empty tool_calls stripped",
			map[string]any{
				"choices": []any{
					map[string]any{
						"delta": map[string]any{"tool_calls": []any{}},
					},
				},
			},
			map[string]any{
				"choices": []any{
					map[string]any{
						"delta": map[string]any{},
					},
				},
			},
		},
		{
			"non-empty tool_calls preserved",
			map[string]any{
				"choices": []any{
					map[string]any{
						"delta": map[string]any{
							"tool_calls": []any{
								map[string]any{"index": 0, "function": map[string]any{"name": "foo"}},
							},
						},
					},
				},
			},
			map[string]any{
				"choices": []any{
					map[string]any{
						"delta": map[string]any{
							"tool_calls": []any{
								map[string]any{"index": 0, "function": map[string]any{"name": "foo"}},
							},
						},
					},
				},
			},
		},
		{
			"no choices left untouched",
			map[string]any{"id": "x", "object": "chat.completion.chunk"},
			map[string]any{"id": "x", "object": "chat.completion.chunk"},
		},
		{
			"reasoning delta with empty tool_calls keeps reasoning",
			map[string]any{
				"choices": []any{
					map[string]any{
						"delta": map[string]any{
							"reasoning_content": "thinking...",
							"tool_calls":        []any{},
						},
					},
				},
			},
			map[string]any{
				"choices": []any{
					map[string]any{
						"delta": map[string]any{
							"reasoning_content": "thinking...",
						},
					},
				},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripEmptyToolCalls(c.in)
			if !mapsEqualJSON(t, got, c.want) {
				t.Fatalf("stripEmptyToolCalls = %#v, want %#v", got, c.want)
			}
		})
	}
}

// TestIsResponsesTerminalEvent ports upstream a9785a5f: response.done must be
// treated as terminal, alongside response.completed/response.failed/error.
func TestIsResponsesTerminalEvent(t *testing.T) {
	terminal := []string{"response.completed", "response.done", "response.failed", "error"}
	for _, ty := range terminal {
		if !isResponsesTerminalEvent(map[string]any{"type": ty}) {
			t.Errorf("type %q should be terminal", ty)
		}
	}
	nonTerminal := []string{"response.created", "response.output_item.added", "response.reasoning_text.delta", ""}
	for _, ty := range nonTerminal {
		if isResponsesTerminalEvent(map[string]any{"type": ty}) {
			t.Errorf("type %q should NOT be terminal", ty)
		}
	}
}

// TestSanitizeOpenAISSEFrameSkipNonJSON ports upstream c22f11de: non-JSON
// "data:" lines (plain-text/HTML errors injected into the SSE stream) are
// skipped so they do not break downstream JSON decoders. Valid JSON lines are
// forwarded (re-encoded).
func TestSanitizeOpenAISSEFrameSkipNonJSON(t *testing.T) {
	frame := []byte("data: {\"choices\":[]}\n\ndata: <html>rate limit</html>\n\ndata: {\"choices\":[]}\n\n")
	state := map[string]any{}
	out, err := sanitizeOpenAISSEFrame(frame, state, true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	joined := string(bytesJoin(out))
	if strings.Contains(joined, "rate limit") || strings.Contains(joined, "<html>") {
		t.Fatalf("non-JSON line leaked: %q", joined)
	}
	if !strings.Contains(joined, "{\"choices\":[]}") {
		t.Fatalf("valid JSON line dropped: %q", joined)
	}
}

// TestSanitizeOpenAISSEFrameInlineDONESetsState verifies an inline
// data: [DONE] sentinel is forwarded verbatim and recorded in state so the EOF
// emitter does not duplicate it (upstream c22f11de dedup).
func TestSanitizeOpenAISSEFrameInlineDONESetsState(t *testing.T) {
	frame := []byte("data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
	state := map[string]any{}
	out, err := sanitizeOpenAISSEFrame(frame, state, true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state["streamDoneSent"] != true {
		t.Fatalf("streamDoneSent not set after inline [DONE]: %#v", state)
	}
	if !strings.Contains(string(bytesJoin(out)), "data: [DONE]") {
		t.Fatalf("inline [DONE] dropped: %q", string(bytesJoin(out)))
	}
}

// TestSanitizeResponsesFrameTerminalSetsState ports upstream a9785a5f: a
// response.done terminal event sets streamDoneSent so the EOF [DONE] emitter
// does not double-emit.
func TestSanitizeResponsesFrameTerminalSetsState(t *testing.T) {
	frame := []byte("event: response.done\ndata: {\"type\":\"response.done\",\"response\":{\"id\":\"r\"}}\n\n")
	state := map[string]any{}
	out, err := sanitizeOpenAISSEFrame(frame, state, false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state["streamDoneSent"] != true {
		t.Fatalf("streamDoneSent not set after response.done: %#v", state)
	}
	joined := string(bytesJoin(out))
	if !strings.Contains(joined, "event: response.done") {
		t.Fatalf("event line dropped: %q", joined)
	}
	if !strings.Contains(joined, "\"type\":\"response.done\"") {
		t.Fatalf("data line dropped: %q", joined)
	}
}

// TestPassthroughSanitizerDispatch verifies the format-keyed dispatch returns
// the right sanitizer (or nil) per source/target combination.
func TestPassthroughSanitizerDispatch(t *testing.T) {
	if passthroughSanitizer(format.Openai, format.Openai) == nil {
		t.Error("Openai→Openai should have a sanitizer (602ee405)")
	}
	if passthroughSanitizer(format.OpenaiResponses, format.OpenaiResponses) == nil {
		t.Error("OpenaiResponses→OpenaiResponses should have a sanitizer (a9785a5f)")
	}
	if passthroughSanitizer(format.Claude, format.Claude) != nil {
		t.Error("Claude→Claude should be raw passthrough (nil sanitizer)")
	}
	if passthroughSanitizer(format.Gemini, format.Gemini) != nil {
		t.Error("Gemini→Gemini should be raw passthrough (nil sanitizer)")
	}
	if passthroughSanitizer(format.Openai, format.OpenaiResponses) != nil {
		t.Error("cross-format should return nil (translate path owns it)")
	}
}

// === End-to-end Pipe tests (real SSE streams, no mocks) ===

// TestPipeResponsesPassthroughEmitsDONEOnEOF ports upstream a9785a5f end-to-end:
// a same-format OpenAI Responses passthrough stream that ends without a [DONE]
// sentinel must get one emitted by the Pipe on EOF, so clients do not hang.
func TestPipeResponsesPassthroughEmitsDONEOnEOF(t *testing.T) {
	rec := httptest.NewRecorder()
	w := httptransport.New(rec, context.Background())
	upstream := strings.NewReader(
		"event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
			"event: response.done\ndata: {\"type\":\"response.done\",\"response\":{\"id\":\"r\"}}\n\n",
	)
	err := httptransport.Pipe(context.Background(), upstream, w, httptransport.PipeOpts{
		StallTimeout:          5 * time.Second,
		StallTimeoutReasoning: 10 * time.Second,
		PassthroughSanitizer:  openaiResponsesPassthroughSanitizer,
		EmitDoneOnEOF:         true,
	})
	if err != nil {
		t.Fatalf("Pipe err: %v", err)
	}
	body := rec.Body.String()
	// response.done is terminal → streamDoneSent set → EOF must NOT duplicate [DONE].
	count := strings.Count(body, "data: [DONE]")
	if count != 0 {
		t.Fatalf("expected 0 EOF [DONE] (terminal seen), got %d in %q", count, body)
	}
}

// TestPipeResponsesPassthroughEmitsDONEWhenNoTerminal ports upstream a9785a5f:
// a Responses passthrough stream ending without any terminal event gets a
// single [DONE] from the EOF emitter.
func TestPipeResponsesPassthroughEmitsDONEWhenNoTerminal(t *testing.T) {
	rec := httptest.NewRecorder()
	w := httptransport.New(rec, context.Background())
	upstream := strings.NewReader(
		"event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n",
	)
	err := httptransport.Pipe(context.Background(), upstream, w, httptransport.PipeOpts{
		StallTimeout:          5 * time.Second,
		StallTimeoutReasoning: 10 * time.Second,
		PassthroughSanitizer:  openaiResponsesPassthroughSanitizer,
		EmitDoneOnEOF:         true,
	})
	if err != nil {
		t.Fatalf("Pipe err: %v", err)
	}
	body := rec.Body.String()
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("missing EOF [DONE] terminator: %q", body)
	}
}

// TestPipeChatPassthroughStripsEmptyToolCalls ports upstream 602ee405
// end-to-end: an OpenAI Chat passthrough stream with empty tool_calls in every
// delta has them stripped before reaching the client.
func TestPipeChatPassthroughStripsEmptyToolCalls(t *testing.T) {
	rec := httptest.NewRecorder()
	w := httptransport.New(rec, context.Background())
	upstream := strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\",\"tool_calls\":[]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\",\"tool_calls\":[]}}]}\n\n" +
			"data: [DONE]\n\n",
	)
	err := httptransport.Pipe(context.Background(), upstream, w, httptransport.PipeOpts{
		StallTimeout:          5 * time.Second,
		StallTimeoutReasoning: 10 * time.Second,
		PassthroughSanitizer:  openaiChatPassthroughSanitizer,
	})
	if err != nil {
		t.Fatalf("Pipe err: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "tool_calls") {
		t.Fatalf("empty tool_calls leaked to client: %q", body)
	}
	if !strings.Contains(body, "reasoning_content") {
		t.Fatalf("reasoning_content dropped: %q", body)
	}
	if !strings.Contains(body, "\"content\":\"hi\"") {
		t.Fatalf("content dropped: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("inline [DONE] dropped: %q", body)
	}
}

// TestPipeChatPassthroughSkipsNonJSON ports upstream c22f11de end-to-end:
// non-JSON data lines injected into an OpenAI Chat SSE stream are skipped.
func TestPipeChatPassthroughSkipsNonJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	w := httptransport.New(rec, context.Background())
	upstream := strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
			"data: rate limit exceeded\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n" +
			"data: [DONE]\n\n",
	)
	err := httptransport.Pipe(context.Background(), upstream, w, httptransport.PipeOpts{
		StallTimeout:          5 * time.Second,
		StallTimeoutReasoning: 10 * time.Second,
		PassthroughSanitizer:  openaiChatPassthroughSanitizer,
	})
	if err != nil {
		t.Fatalf("Pipe err: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "rate limit") {
		t.Fatalf("non-JSON line leaked: %q", body)
	}
	if !strings.Contains(body, "\"content\":\"a\"") || !strings.Contains(body, "\"content\":\"b\"") {
		t.Fatalf("valid JSON dropped: %q", body)
	}
}

// TestPipePassthroughSanitizerNilKeepsRawByteContract verifies that a nil
// sanitizer preserves the historical byte-for-byte passthrough contract
// (TestPipePassthrough equivalent), so the new opt-in does not regress
// same-format Claude/Gemini streams.
func TestPipePassthroughSanitizerNilKeepsRawByteContract(t *testing.T) {
	rec := httptest.NewRecorder()
	w := httptransport.New(rec, context.Background())
	raw := "data: {\"chunk\":1}\n\ndata: {\"chunk\":2}\n\n"
	upstream := strings.NewReader(raw)
	err := httptransport.Pipe(context.Background(), upstream, w, httptransport.PipeOpts{
		StallTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Pipe err: %v", err)
	}
	if body := rec.Body.String(); body != raw {
		t.Fatalf("raw passthrough broken: %q want %q", body, raw)
	}
}

// === helpers ===

func bytesJoin(b [][]byte) []byte {
	var out []byte
	for _, e := range b {
		out = append(out, e...)
	}
	return out
}

// mapsEqualJSON compares two maps by their JSON encoding (order-independent).
func mapsEqualJSON(t *testing.T, a, b map[string]any) bool {
	t.Helper()
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}
