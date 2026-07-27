package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

// newBypassHandler builds a v1Handler with the minimum deps needed for
// handleBypassRequest (which touches only the response writer, not the DB).
func newBypassHandler() *v1Handler {
	return &v1Handler{}
}

func bypassReq(t *testing.T, body string, ua string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	return httptest.NewRecorder(), req
}

func TestHandleBypassRequest_NonClaudeCLIUserAgentPassesThrough(t *testing.T) {
	h := newBypassHandler()
	body := `{"model":"x","messages":[{"role":"user","content":"Warmup"}]}`
	rec, req := bypassReq(t, body, "curl/8.0")
	handled := h.handleBypassRequest(rec, []byte(body), "x", req.UserAgent(), req.URL.Path, true)
	if handled {
		t.Fatalf("non claude-cli UA must not bypass")
	}
}

func TestHandleBypassRequest_WarmupPatternBypasses(t *testing.T) {
	h := newBypassHandler()
	body := `{"model":"claude-3","stream":false,"messages":[{"role":"user","content":"Warmup"}]}`
	rec, req := bypassReq(t, body, "claude-cli/1.0")
	handled := h.handleBypassRequest(rec, []byte(body), "claude-3", req.UserAgent(), req.URL.Path, false)
	if !handled {
		t.Fatalf("Warmup + claude-cli should bypass")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("non-streaming bypass should be JSON, got %s", ct)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %v", choices)
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != defaultBypassText {
		t.Fatalf("content = %v, want default bypass text", msg["content"])
	}
}

func TestHandleBypassRequest_CountPatternBypasses(t *testing.T) {
	h := newBypassHandler()
	body := `{"model":"claude-3","stream":false,"messages":[{"role":"user","content":"count"}]}`
	rec, req := bypassReq(t, body, "claude-cli/1.0")
	handled := h.handleBypassRequest(rec, []byte(body), "claude-3", req.UserAgent(), req.URL.Path, false)
	if !handled {
		t.Fatalf("count + claude-cli should bypass")
	}
}

func TestHandleBypassRequest_TitleExtractionPatternBypasses(t *testing.T) {
	h := newBypassHandler()
	// Last message is assistant with first content part text "{".
	body := `{"model":"claude-3","stream":false,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"text","text":"{"}]}
	]}`
	rec, req := bypassReq(t, body, "claude-cli/1.0")
	handled := h.handleBypassRequest(rec, []byte(body), "claude-3", req.UserAgent(), req.URL.Path, false)
	if !handled {
		t.Fatalf("title-extraction (assistant '{') + claude-cli should bypass")
	}
}

func TestHandleBypassRequest_SkipPatternBypasses(t *testing.T) {
	h := newBypassHandler()
	body := `{"model":"claude-3","stream":false,"messages":[
		{"role":"user","content":"Please write a 5-10 word title for the following conversation: foo bar"}
	]}`
	rec, req := bypassReq(t, body, "claude-cli/1.0")
	handled := h.handleBypassRequest(rec, []byte(body), "claude-3", req.UserAgent(), req.URL.Path, false)
	if !handled {
		t.Fatalf("SKIP_PATTERNS text + claude-cli should bypass")
	}
}

func TestHandleBypassRequest_NamingPatternRequiresCcFilterNaming(t *testing.T) {
	h := newBypassHandler()
	body := `{"model":"claude-3","stream":false,"system":"You decide isNewTopic","messages":[
		{"role":"user","content":"hello world foo bar baz"}
	]}`
	// Without ccFilterNaming → no bypass.
	rec, req := bypassReq(t, body, "claude-cli/1.0")
	if h.handleBypassRequest(rec, []byte(body), "claude-3", req.UserAgent(), req.URL.Path, false) {
		t.Fatalf("naming pattern must NOT bypass when ccFilterNaming is off")
	}
	// With ccFilterNaming → bypass and title is first 3 words.
	rec2, req2 := bypassReq(t, body, "claude-cli/1.0")
	if !h.handleBypassRequest(rec2, []byte(body), "claude-3", req2.UserAgent(), req2.URL.Path, true) {
		t.Fatalf("naming pattern must bypass when ccFilterNaming is on")
	}
	var out map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices, _ := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	var payload map[string]any
	if err := json.Unmarshal([]byte(msg["content"].(string)), &payload); err != nil {
		t.Fatalf("naming content should be JSON, got %v: %v", msg["content"], err)
	}
	if !payload["isNewTopic"].(bool) {
		t.Fatalf("isNewTopic should be true, got %v", payload["isNewTopic"])
	}
	if payload["title"] != "hello world foo" {
		t.Fatalf("title should be first 3 words, got %q", payload["title"])
	}
}

func TestHandleBypassRequest_NamingPatternReadsSystemFromMessages(t *testing.T) {
	h := newBypassHandler()
	body := `{"model":"claude-3","stream":false,"messages":[
		{"role":"system","content":"isNewTopic helper"},
		{"role":"user","content":"alpha beta gamma"}
	]}`
	rec, req := bypassReq(t, body, "claude-cli/1.0")
	if !h.handleBypassRequest(rec, []byte(body), "claude-3", req.UserAgent(), req.URL.Path, true) {
		t.Fatalf("naming via system message must bypass with ccFilterNaming on")
	}
}

func TestHandleBypassRequest_ClaudeEndpointEmitsClaudeShape(t *testing.T) {
	h := newBypassHandler()
	body := `{"model":"claude-3","stream":false,"messages":[{"role":"user","content":"Warmup"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("User-Agent", "claude-cli/1.0")
	rec := httptest.NewRecorder()
	if !h.handleBypassRequest(rec, []byte(body), "claude-3", req.UserAgent(), req.URL.Path, false) {
		t.Fatalf("Warmup + claude-cli on /v1/messages should bypass")
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["type"] != "message" {
		t.Fatalf("claude bypass should have type=message, got %v", out["type"])
	}
	content, _ := out["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %v", content)
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("content block type = %v, want text", block["type"])
	}
}

func TestHandleBypassRequest_StreamingEmitsSSE(t *testing.T) {
	h := newBypassHandler()
	body := `{"model":"claude-3","stream":true,"messages":[{"role":"user","content":"Warmup"}]}`
	rec, req := bypassReq(t, body, "claude-cli/1.0")
	if !h.handleBypassRequest(rec, []byte(body), "claude-3", req.UserAgent(), req.URL.Path, false) {
		t.Fatalf("Warmup + claude-cli should bypass (streaming)")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("streaming bypass should be SSE, got %s", ct)
	}
	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, "data: [DONE]") {
		t.Fatalf("OpenAI streaming bypass should end with [DONE], got %s", bodyStr)
	}
}

func TestHandleBypassRequest_ClaudeStreamingEmitsMessageStop(t *testing.T) {
	h := newBypassHandler()
	body := `{"model":"claude-3","stream":true,"messages":[{"role":"user","content":"Warmup"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("User-Agent", "claude-cli/1.0")
	rec := httptest.NewRecorder()
	if !h.handleBypassRequest(rec, []byte(body), "claude-3", req.UserAgent(), req.URL.Path, false) {
		t.Fatalf("should bypass")
	}
	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, "event: {\"type\":\"message_stop\"}") {
		t.Fatalf("claude streaming bypass should emit message_stop, got %s", bodyStr)
	}
}

func TestHandleBypassRequest_NoMessagesPassesThrough(t *testing.T) {
	h := newBypassHandler()
	body := `{"model":"claude-3","stream":false,"input":[{"role":"user","content":"hi"}]}`
	rec, req := bypassReq(t, body, "claude-cli/1.0")
	if h.handleBypassRequest(rec, []byte(body), "claude-3", req.UserAgent(), req.URL.Path, false) {
		t.Fatalf("no messages array must not bypass (even with claude-cli)")
	}
}

func TestHandleBypassRequest_InvalidJSONPassesThrough(t *testing.T) {
	h := newBypassHandler()
	rec, req := bypassReq(t, `{not json`, "claude-cli/1.0")
	if h.handleBypassRequest(rec, []byte(`{not json`), "claude-3", req.UserAgent(), req.URL.Path, false) {
		t.Fatalf("invalid JSON must not bypass")
	}
}

func TestDetectBypassSourceFormat(t *testing.T) {
	if f := detectBypassSourceFormat("/v1/messages", nil); f != format.Claude {
		t.Fatalf("/v1/messages → claude, got %v", f)
	}
	if f := detectBypassSourceFormat("/v1/responses", nil); f != format.OpenaiResponses {
		t.Fatalf("/v1/responses → openai-responses, got %v", f)
	}
	if f := detectBypassSourceFormat("/v1/chat/completions", nil); f != format.Openai {
		t.Fatalf("/v1/chat/completions → openai, got %v", f)
	}
}

func TestBypassJoinUserTextAndSystem(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "sys text"},
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "reply"},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "world"}}},
	}
	if got := bypassJoinUserText(msgs); got != "hello world" {
		t.Fatalf("joinUserText = %q, want %q", got, "hello world")
	}
	if got := bypassSystemText(msgs, map[string]any{}); got != "sys text" {
		t.Fatalf("systemText = %q, want %q", got, "sys text")
	}
	// Top-level body.system (array form) when no system message.
	if got := bypassSystemText([]any{}, map[string]any{
		"system": []any{map[string]any{"type": "text", "text": "top"}, map[string]any{"type": "text", "text": "level"}},
	}); got != "top level" {
		t.Fatalf("system array = %q, want %q", got, "top level")
	}
	if got := bypassSystemText([]any{}, map[string]any{"system": "raw string"}); got != "raw string" {
		t.Fatalf("system string = %q, want %q", got, "raw string")
	}
}
