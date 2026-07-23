package rtk

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
	// Side-effect import so translator/openai's init() registers the
	// Responses↔OpenAI and Claude↔OpenAI request translators the headroom
	// branches route through. Production gets this via internal/app/wire's
	// import of translator/register; tests in rtk must trigger it themselves.
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/register"
)

// headroomCompressHandler returns an httptest handler that echoes back the
// incoming messages with a marker prefix on each content string, simulating a
// /v1/compress proxy that compresses text. It is a real HTTP server, not a
// dependency mock — it exercises the full callCompress + translator path.
func headroomCompressHandler(t *testing.T, prefix string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/compress") {
			t.Errorf("compress endpoint path = %s, want suffix /v1/compress", r.URL.Path)
			http.Error(w, "bad endpoint", http.StatusBadRequest)
			return
		}
		var payload struct {
			Messages []map[string]any `json:"messages"`
			Model    string           `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode compress payload: %v", err)
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		out := map[string]any{"messages": []map[string]any{}}
		for _, m := range payload.Messages {
			content := m["content"]
			if s, ok := content.(string); ok {
				content = prefix + s
			}
			out["messages"] = append(out["messages"].([]map[string]any), map[string]any{
				"role":    m["role"],
				"content": content,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
}

// TestHasUnsafeResponsesInputForCompression ports #2132: input with only
// message items is safe; any non-message item (function_call /
// function_call_output / reasoning) is unsafe.
func TestHasUnsafeResponsesInputForCompression(t *testing.T) {
	cases := map[string]struct {
		body map[string]any
		want bool
	}{
		"only messages": {
			body: map[string]any{"input": []any{
				map[string]any{"type": "message", "role": "user", "content": "hi"},
				map[string]any{"type": "message", "role": "assistant", "content": "hello"},
			}},
			want: false,
		},
		"has function_call": {
			body: map[string]any{"input": []any{
				map[string]any{"type": "message", "role": "user", "content": "hi"},
				map[string]any{"type": "function_call", "name": "f", "arguments": "{}"},
			}},
			want: true,
		},
		"has function_call_output": {
			body: map[string]any{"input": []any{
				map[string]any{"type": "function_call_output", "call_id": "x", "output": "y"},
			}},
			want: true,
		},
		"has reasoning": {
			body: map[string]any{"input": []any{
				map[string]any{"type": "reasoning", "summary": []any{}},
			}},
			want: true,
		},
		"item without type is safe": {
			body: map[string]any{"input": []any{
				map[string]any{"role": "user", "content": "hi"},
			}},
			want: false,
		},
		"no input array": {body: map[string]any{}, want: false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := hasUnsafeResponsesInputForCompression(c.body); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestCompressResponsesViaHeadroomSkipsUnsafeInput verifies #2132: when
// body.input contains a function_call, compression is skipped (returns nil,
// nil) and a diagnostic is set, WITHOUT calling the proxy.
func TestCompressResponsesViaHeadroomSkipsUnsafeInput(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	body := map[string]any{
		"model": "codex-1",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hello"},
			map[string]any{"type": "function_call", "name": "f", "arguments": "{}"},
		},
	}
	diag := &HeadroomDiagnostics{}
	stats, err := compressResponsesViaHeadroomImpl(body, HeadroomConfig{
		Enabled: true, URL: srv.URL, Model: "codex-1", Format: format.OpenaiResponses, Diagnostics: diag,
	}, srv.Client(), 3000)
	if err != nil {
		t.Fatalf("expected fail-open nil,nil; got err %v", err)
	}
	if stats != nil {
		t.Errorf("expected nil stats on unsafe skip, got %+v", stats)
	}
	if called {
		t.Error("proxy should NOT be called on unsafe input")
	}
	if !strings.Contains(diag.Reason, "not safe to compress") {
		t.Errorf("diagnostic reason = %q, want 'not safe to compress'", diag.Reason)
	}
}

// TestCompressResponsesViaHeadroomRoundTrip verifies #1998/d4d11357: a safe
// Responses input (message items only) is translated to OpenAI, compressed,
// and written back as a Responses input array.
func TestCompressResponsesViaHeadroomRoundTrip(t *testing.T) {
	srv := httptest.NewServer(headroomCompressHandler(t, "[c] "))
	defer srv.Close()

	body := map[string]any{
		"model": "codex-1",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hello world"},
		},
	}
	stats, err := compressResponsesViaHeadroomImpl(body, HeadroomConfig{
		Enabled: true, URL: srv.URL, Model: "codex-1", Format: format.OpenaiResponses,
	}, srv.Client(), 3000)
	if err != nil {
		t.Fatalf("compress responses: %v", err)
	}
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	in, ok := body["input"].([]any)
	if !ok || len(in) == 0 {
		t.Fatalf("body.input not rewritten: %v", body["input"])
	}
	first, ok := in[0].(map[string]any)
	if !ok {
		t.Fatalf("first input item not a map: %v", in[0])
	}
	if first["type"] != "message" {
		t.Errorf("input[0].type = %v, want message (Responses contract preserved)", first["type"])
	}
	// The compressed content should carry the proxy marker prefix.
	content := responsesMessageText(first["content"])
	if !strings.Contains(content, "[c] ") {
		t.Errorf("input[0] content = %q, want it to contain the compressed marker", content)
	}
}

// TestCompressClaudeViaHeadroomRoundTrip verifies the claude branch: a Claude
// body is translated to OpenAI, compressed, and written back as Claude messages.
func TestCompressClaudeViaHeadroomRoundTrip(t *testing.T) {
	srv := httptest.NewServer(headroomCompressHandler(t, "[c] "))
	defer srv.Close()

	body := map[string]any{
		"model":      "claude-opus-4-8",
		"max_tokens": 1024,
		"messages": []any{
			map[string]any{"role": "user", "content": "hello world"},
		},
	}
	stats, err := compressClaudeViaHeadroomImpl(body, HeadroomConfig{
		Enabled: true, URL: srv.URL, Model: "claude-opus-4-8", Format: format.Claude,
	}, srv.Client(), 3000)
	if err != nil {
		t.Fatalf("compress claude: %v", err)
	}
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("body.messages not rewritten to 1 entry: %v", body["messages"])
	}
	// Claude messages carry content as a block array; extract the text.
	text := claudeMessageText(msgs[0])
	if !strings.Contains(text, "[c] ") {
		t.Errorf("claude message text = %q, want compressed marker", text)
	}
}

// TestCompressResponsesViaHeadroomNoMessages verifies fail-open when the proxy
// returns an empty messages array.
func TestCompressResponsesViaHeadroomNoMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
	}))
	defer srv.Close()

	body := map[string]any{
		"model": "codex-1",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hi"},
		},
	}
	_, err := compressResponsesViaHeadroomImpl(body, HeadroomConfig{
		Enabled: true, URL: srv.URL, Model: "codex-1", Format: format.OpenaiResponses,
	}, srv.Client(), 3000)
	if err == nil {
		t.Error("expected error on empty messages, got nil")
	}
}

// responsesMessageText extracts a text string from a Responses message item's
// content (string or array of {text} parts).
func responsesMessageText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if s, ok := p.(string); ok {
				parts = append(parts, s)
				continue
			}
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// claudeMessageText extracts text from a Claude message (content string or
// block array of {type:text,text}).
func claudeMessageText(msg any) string {
	m, ok := msg.(map[string]any)
	if !ok {
		return ""
	}
	switch c := m["content"].(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, b := range c {
			if block, ok := b.(map[string]any); ok {
				if t, ok := block["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}
