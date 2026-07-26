package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/register"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

// slowReader blocks forever, simulating an upstream that never sends data.
type slowReader struct{}

func (slowReader) Read([]byte) (int, error) {
	time.Sleep(time.Hour)
	return 0, nil
}

func TestPipeStallEmitsErrorSSEAndDone(t *testing.T) {
	rec := httptest.NewRecorder()
	w := New(rec, context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := Pipe(ctx, slowReader{}, w, PipeOpts{
		StallTimeout:          50 * time.Millisecond,
		StallTimeoutReasoning: 200 * time.Millisecond,
		Reason:                "test_stall",
	})
	if err != nil {
		t.Fatalf("Pipe returned error: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"code":"stream_stall_timeout"`) {
		t.Fatalf("body missing error SSE: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("body missing [DONE] terminator: %q", body)
	}
}

func TestPipeContextCancelEmitsErrorSSEAndDone(t *testing.T) {
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	w := New(rec, ctx)

	pipeDone := make(chan struct{})
	go func() {
		defer close(pipeDone)
		_ = Pipe(ctx, slowReader{}, w, PipeOpts{
			StallTimeout:          5 * time.Second,
			StallTimeoutReasoning: 10 * time.Second,
		})
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-pipeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Pipe did not return after context cancel")
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"code":"client_disconnect"`) {
		t.Fatalf("body missing client_disconnect error SSE: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("body missing [DONE] terminator: %q", body)
	}
}

func TestPipePassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := New(rec, context.Background())

	upstream := strings.NewReader("data: {\"chunk\":1}\n\ndata: {\"chunk\":2}\n\n")

	err := Pipe(context.Background(), upstream, w, PipeOpts{StallTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Pipe returned error: %v", err)
	}

	body := rec.Body.String()
	want := "data: {\"chunk\":1}\n\ndata: {\"chunk\":2}\n\n"
	if body != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

func TestPipeIsThinkingModelUsesReasoningTimeout(t *testing.T) {
	rec := httptest.NewRecorder()
	w := New(rec, context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- Pipe(context.Background(), slowReader{}, w, PipeOpts{
			StallTimeout:          50 * time.Millisecond,
			StallTimeoutReasoning: 200 * time.Millisecond,
			IsThinkingModel:       true,
		})
	}()

	select {
	case err := <-errCh:
		t.Fatalf("Pipe returned too early: %v", err)
	case <-time.After(120 * time.Millisecond):
		// expected: reasoning timeout (200ms) has not fired yet
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Pipe returned error: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Pipe did not return within reasoning timeout")
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"code":"stream_stall_timeout"`) {
		t.Fatalf("body missing error SSE: %q", body)
	}
}

// TestPipeCrossFormatTranslate_GeminiSSE is the T026 regression test: a gemini
// upstream emits its real SSE shape ("data: {candidates:...}\n\n") and the
// client expects OpenAI chat.completion.chunk events. Before the fix the
// frameReader handed the raw "data: {...}" frame to TranslateResponse,
// json.Unmarshal failed on the leading "data:" and Pipe emitted 0 bytes (gemini
// /v1/chat/completions stream returned 200 + headers but empty body). With the
// fix the "data:" prefix is stripped before translation so the chunk is parsed
// and emitted as OpenAI SSE.
func TestPipeCrossFormatTranslate_GeminiSSE(t *testing.T) {
	// Side-effect import ensures the gemini<->openai response translator is
	// registered even though this test lives in package http (mirrors how
	// internal/app wires register for the production binary).
	_ = translator.TranslateResponse

	// Realistic gemini streamGenerateContent?alt=sse upstream body: a role
	// chunk, a content chunk, and a final chunk carrying usage + STOP.
	upstream := strings.NewReader(
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\"},\"index\":0}]}\n\n" +
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}],\"role\":\"model\"},\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1,\"totalTokenCount\":4},\"modelVersion\":\"gemini-2.5-flash\"}\n\n" +
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\"},\"finishReason\":\"STOP\",\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1,\"totalTokenCount\":4},\"modelVersion\":\"gemini-2.5-flash\"}\n\n",
	)

	rec := httptest.NewRecorder()
	w := New(rec, context.Background())

	err := Pipe(context.Background(), upstream, w, PipeOpts{
		StallTimeout: 10 * time.Second,
		FrameMode:    "sse",
		TranslateResponse: func(frame []byte, state map[string]any) ([][]byte, error) {
			out, err := translator.TranslateResponse(format.Gemini, format.Openai, json.RawMessage(frame), state)
			if err != nil {
				return nil, err
			}
			res := make([][]byte, len(out))
			for i, c := range out {
				res[i] = c
			}
			return res, nil
		},
	})
	if err != nil {
		t.Fatalf("Pipe returned error: %v", err)
	}

	body := rec.Body.String()
	if body == "" {
		t.Fatalf("body is empty — the data: prefix was not stripped before translation (T026 regression)")
	}
	// Must contain at least one OpenAI chat.completion.chunk with the content.
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Fatalf("body missing OpenAI chunk object: %q", body)
	}
	if !strings.Contains(body, `"content":"ok"`) {
		t.Fatalf("body missing content delta: %q", body)
	}
	// Every emitted frame is a well-formed "data: <json>\n\n" SSE block.
	for _, ev := range strings.Split(body, "\n\n") {
		ev = strings.TrimSpace(ev)
		if ev == "" {
			continue
		}
		if !strings.HasPrefix(ev, "data: ") {
			t.Fatalf("emitted frame is not a data: SSE block: %q", ev)
		}
		payload := strings.TrimSpace(strings.TrimPrefix(ev, "data: "))
		if payload == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("emitted frame payload is not JSON: %v (frame=%q)", err, payload)
		}
		if m["object"] != "chat.completion.chunk" {
			t.Fatalf("emitted frame object = %v, want chat.completion.chunk: %q", m["object"], payload)
		}
	}
	// The usage from the upstream usageMetadata must reach the client.
	if !strings.Contains(body, `"prompt_tokens":3`) || !strings.Contains(body, `"completion_tokens":1`) {
		t.Fatalf("body missing usage: %q", body)
	}
}

// TestPipeCrossFormatTranslate_NDJSONStillBare verifies the data:-stripping fix
// is a no-op for NDJSON upstreams (ollama): a bare JSON line (no "data:"
// prefix) is handed to the translator unchanged.
func TestPipeCrossFormatTranslate_NDJSONStillBare(t *testing.T) {
	_ = translator.TranslateResponse
	upstream := strings.NewReader(
		`{"model":"x","message":{"content":"hi"},"done":false}` + "\n" +
			`{"model":"x","message":{},"done":true}` + "\n",
	)
	rec := httptest.NewRecorder()
	w := New(rec, context.Background())

	called := 0
	err := Pipe(context.Background(), upstream, w, PipeOpts{
		StallTimeout: 10 * time.Second,
		FrameMode:    "ndjson",
		TranslateResponse: func(frame []byte, state map[string]any) ([][]byte, error) {
			called++
			// The translator must receive the bare JSON line (no "data:" prefix).
			if strings.HasPrefix(strings.TrimSpace(string(frame)), "data:") {
				t.Errorf("translator received an SSE-prefixed frame for NDJSON: %q", frame)
			}
			// Round-trip the payload so Pipe emits valid OpenAI SSE.
			var m map[string]any
			if err := json.Unmarshal(frame, &m); err != nil {
				return nil, err
			}
			chunk, _ := json.Marshal(map[string]any{
				"object": "chat.completion.chunk",
				"choices": []map[string]any{
					{"delta": map[string]any{"content": m["message"]}, "index": 0},
				},
			})
			return [][]byte{chunk}, nil
		},
	})
	if err != nil {
		t.Fatalf("Pipe returned error: %v", err)
	}
	if called == 0 {
		t.Fatalf("translator never called — NDJSON frames not piped")
	}
	if !strings.Contains(rec.Body.String(), `"object":"chat.completion.chunk"`) {
		t.Fatalf("body missing chunk: %q", rec.Body.String())
	}
}

// TestStripSSEDataPayload unit-pins the helper's behaviour.
func TestStripSSEDataPayload(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare json object", `{"a":1}`, `{"a":1}`},
		{"bare json array", `[1,2]`, `[1,2]`},
		{"data single line", "data: {\"a\":1}\n", `{"a":1}`},
		{"data with event line", "event: message\ndata: {\"a\":1}\n", `{"a":1}`},
		{"data multiline block", "data: {\"a\":\ndata: 1}", "{\"a\":\n1}"},
		{"done sentinel", "data: [DONE]\n", "[DONE]"},
		{"whitespace only", "   \n\n  ", "   \n\n  "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(stripSSEDataPayload([]byte(c.in)))
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
