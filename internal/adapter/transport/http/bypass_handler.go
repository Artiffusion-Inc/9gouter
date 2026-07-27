package http

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

// Package http — CLI bypass handler, porting open-sse/utils/bypassHandler.js.
//
// Claude Code CLI (user-agent contains "claude-cli") sends a handful of
// housekeeping requests that the user does not want billed against a real
// upstream: Warmup probes, "count" token counters, conversation-title
// extraction ("{"), the SKIP_PATTERNS title prompt, and (when the dashboard
// toggle ccFilterNaming is on) the "isNewTopic" naming probe. The legacy JS
// chat path short-circuited these with a fake response BEFORE combo rotation
// so they neither waste rotation slots nor cost upstream tokens; the Go
// rewrite dropped the whole path, so the dashboard "cc filter naming" toggle
// did nothing and these probes went to the provider.
//
// This handler reproduces the 5 patterns from bypassHandler.js:11-92 and emits
// the fake response directly in the request's SOURCE format (OpenAI for
// /v1/chat/completions, Claude for /v1/messages, OpenAI-Responses for
// /v1/responses) — matching the JS createNonStreamingResponse /
// createStreamingResponse which translated a synthetic OpenAI completion back
// into the source format. Only OpenAI / Claude / Responses are handled here;
// any other format falls through to the normal chat path (the JS path likewise
// only ran for formats it could translate).

// SkipPatterns mirrors open-sse/config/runtimeConfig.js SKIP_PATTERNS. Text
// present in the concatenated user-message content triggers a bypass.
var bypassSkipPatterns = []string{
	"Please write a 5-10 word title for the following conversation:",
}

// defaultBypassText mirrors bypassHandler.js DEFAULT_BYPASS_TEXT.
const defaultBypassText = "CLI Command Execution: Clear Terminal"

// handleBypassRequest ports open-sse/utils/bypassHandler.js. It returns true
// when a bypass pattern matched and a fake response was written to w (the
// caller must not touch w further), false when the request should proceed to
// the normal chat path.
//
// ccFilterNaming enables Pattern 5 (the "isNewTopic" naming probe); patterns
// 1-4 run regardless of the setting, exactly as in JS.
func (h *v1Handler) handleBypassRequest(w http.ResponseWriter, body []byte, model, userAgent, endpoint string, ccFilterNaming bool) bool {
	if !strings.Contains(userAgent, "claude-cli") {
		return false
	}
	if len(body) == 0 {
		return false
	}
	var b map[string]any
	if err := json.Unmarshal(body, &b); err != nil {
		return false
	}
	messages, _ := b["messages"].([]any)
	if len(messages) == 0 {
		return false
	}

	shouldBypass := false
	namingBypass := false

	// Pattern 1: title extraction — last message is an assistant turn whose
	// first content part text is exactly "{".
	if last, ok := messages[len(messages)-1].(map[string]any); ok {
		if role, _ := last["role"].(string); role == "assistant" {
			if firstTextInContent(last["content"]) == "{" {
				shouldBypass = true
			}
		}
	}

	// Pattern 2: Warmup — first message text is "Warmup".
	if !shouldBypass {
		if bypassMessageText(messages[0]) == "Warmup" {
			shouldBypass = true
		}
	}

	// Pattern 3: count — single user message whose text is "count".
	if !shouldBypass && len(messages) == 1 {
		if first, ok := messages[0].(map[string]any); ok {
			if role, _ := first["role"].(string); role == "user" {
				if bypassMessageText(first) == "count" {
					shouldBypass = true
				}
			}
		}
	}

	// Pattern 4: SKIP_PATTERNS — any skip string present in the concatenated
	// user-message text.
	if !shouldBypass {
		userText := bypassJoinUserText(messages)
		for _, p := range bypassSkipPatterns {
			if strings.Contains(userText, p) {
				shouldBypass = true
				break
			}
		}
	}

	// Pattern 5: ccFilterNaming — system text (from messages or top-level
	// body.system) contains "isNewTopic".
	if !shouldBypass && ccFilterNaming {
		systemText := bypassSystemText(messages, b)
		if strings.Contains(systemText, "isNewTopic") {
			shouldBypass = true
			namingBypass = true
		}
	}

	if !shouldBypass {
		return false
	}

	stream := true
	if s, ok := b["stream"].(bool); ok {
		stream = s
	}

	src := detectBypassSourceFormat(endpoint, body)
	text := defaultBypassText
	if namingBypass {
		text = bypassNamingText(messages)
	}

	h.writeBypassResponse(w, src, model, text, stream)
	return true
}

// detectBypassSourceFormat mirrors bypassHandler.js detectFormat(body): the
// source format the client used, which the fake response must match. It defers
// to format.DetectByEndpoint for /v1/messages (Claude) and /v1/responses
// (OpenaiResponses), and defaults to OpenAI for /v1/chat/completions.
func detectBypassSourceFormat(endpoint string, body []byte) format.Format {
	if f := format.DetectByEndpoint(endpoint, body); f != format.FormatUnknown {
		return f
	}
	return format.Openai
}

// writeBypassResponse writes a synthetic completion in the source format. For
// streaming it emits a single-content SSE chunk + [DONE]; for non-streaming a
// full completion object. Mirrors createStreamingResponse /
// createNonStreamingResponse but emits the native source-format shape directly
// (OpenAI / Claude / Responses) rather than translating through the
// OpenAI intermediate — the response is a single static assistant turn with no
// tool calls, so direct authoring is byte-equivalent and avoids pulling the
// translator into the HTTP layer.
func (h *v1Handler) writeBypassResponse(w http.ResponseWriter, src format.Format, model, text string, stream bool) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		switch src {
		case format.Claude:
			writeClaudeBypassSSE(w, model, text)
		case format.OpenaiResponses:
			writeResponsesBypassSSE(w, model, text)
		default:
			writeOpenAIBypassSSE(w, model, text)
		}
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	switch src {
	case format.Claude:
		writeClaudeBypassJSON(w, model, text)
	case format.OpenaiResponses:
		writeResponsesBypassJSON(w, model, text)
	default:
		writeOpenAIBypassJSON(w, model, text)
	}
}

// --- text extraction helpers (ports bypassHandler.js getText) ---

func bypassMessageText(msg any) string {
	m, ok := msg.(map[string]any)
	if !ok {
		return ""
	}
	return bypassTextOf(m["content"])
}

// bypassTextOf mirrors getText(content): string content → as-is; array content
// → joined text parts.
func bypassTextOf(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	if parts, ok := content.([]any); ok {
		var b strings.Builder
		for _, p := range parts {
			if pm, ok := p.(map[string]any); ok {
				if t, _ := pm["type"].(string); t == "text" {
					if s, _ := pm["text"].(string); s != "" {
						if b.Len() > 0 {
							b.WriteString(" ")
						}
						b.WriteString(s)
					}
				}
			}
		}
		return b.String()
	}
	return ""
}

// firstTextInContent returns the text of the first text part of a content
// array (or the string itself). Used by Pattern 1 (assistant content[0].text
// === "{").
func firstTextInContent(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	if parts, ok := content.([]any); ok && len(parts) > 0 {
		if pm, ok := parts[0].(map[string]any); ok {
			if t, _ := pm["text"].(string); ok {
				return t
			}
		}
	}
	return ""
}

// bypassJoinUserText joins all user-message text, mirroring
// bypassHandler.js Pattern 4.
func bypassJoinUserText(messages []any) string {
	var b strings.Builder
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(bypassTextOf(m["content"]))
	}
	return b.String()
}

// bypassSystemText mirrors bypassHandler.js Pattern 5: system text from the
// first system message, else the top-level body.system (array of text blocks
// or string).
func bypassSystemText(messages []any, body map[string]any) string {
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role == "system" {
			if t := bypassTextOf(m["content"]); t != "" {
				return t
			}
		}
	}
	switch s := body["system"].(type) {
	case string:
		return s
	case []any:
		var b strings.Builder
		for _, p := range s {
			if pm, ok := p.(map[string]any); ok {
				if t, _ := pm["type"].(string); t == "text" {
					if txt, _ := pm["text"].(string); txt != "" {
						if b.Len() > 0 {
							b.WriteString(" ")
						}
						b.WriteString(txt)
					}
				}
			}
		}
		return b.String()
	}
	return ""
}

// bypassNamingText mirrors the naming-bypass response: first 3 words of the
// first user message, wrapped as {"isNewTopic":true,"title":"..."}.
func bypassNamingText(messages []any) string {
	var userText string
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role == "user" {
			userText = bypassTextOf(m["content"])
			break
		}
	}
	fields := strings.Fields(userText)
	if len(fields) > 3 {
		fields = fields[:3]
	}
	title := strings.Join(fields, " ")
	payload := map[string]any{"isNewTopic": true, "title": title}
	b, _ := json.Marshal(payload)
	return string(b)
}

// --- OpenAI bypass shapes ---

func writeOpenAIBypassJSON(w http.ResponseWriter, model, text string) {
	out := map[string]any{
		"id":      "chatcmpl-bypass",
		"object":  "chat.completion",
		"created": 0,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	}
	_ = json.NewEncoder(w).Encode(out)
}

func writeOpenAIBypassSSE(w http.ResponseWriter, model, text string) {
	chunk := map[string]any{
		"id":     "chatcmpl-bypass",
		"object": "chat.completion.chunk",
		"model":  model,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "content": text},
		}},
	}
	b, _ := json.Marshal(chunk)
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n\n"))

	stop := map[string]any{
		"id":     "chatcmpl-bypass",
		"object": "chat.completion.chunk",
		"model":  model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	}
	sb, _ := json.Marshal(stop)
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(sb)
	_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
}

// --- Claude bypass shapes ---

func writeClaudeBypassJSON(w http.ResponseWriter, model, text string) {
	out := map[string]any{
		"id":          "msg_bypass",
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     []any{map[string]any{"type": "text", "text": text}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
	}
	_ = json.NewEncoder(w).Encode(out)
}

func writeClaudeBypassSSE(w http.ResponseWriter, model, text string) {
	writeSSEEvent(w, map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_bypass", "type": "message", "role": "assistant", "model": model,
		"content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": 1, "output_tokens": 0},
	}})
	writeSSEEvent(w, map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""}})
	writeSSEEvent(w, map[string]any{"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text}})
	writeSSEEvent(w, map[string]any{"type": "content_block_stop", "index": 0})
	writeSSEEvent(w, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": 1}})
	writeSSEEvent(w, map[string]any{"type": "message_stop"})
}

// --- OpenAI Responses bypass shapes ---

func writeResponsesBypassJSON(w http.ResponseWriter, model, text string) {
	out := map[string]any{
		"id":     "resp_bypass",
		"object": "response",
		"model":  model,
		"output": []any{map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		}},
		"status": "completed",
		"usage":  map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
	}
	_ = json.NewEncoder(w).Encode(out)
}

func writeResponsesBypassSSE(w http.ResponseWriter, model, text string) {
	writeSSEEvent(w, map[string]any{"type": "response.created",
		"response": map[string]any{"id": "resp_bypass", "object": "response", "model": model, "status": "in_progress"}})
	writeSSEEvent(w, map[string]any{"type": "response.output_text.delta", "delta": text})
	writeSSEEvent(w, map[string]any{"type": "response.output_text.done", "text": text})
	writeSSEEvent(w, map[string]any{"type": "response.completed",
		"response": map[string]any{"id": "resp_bypass", "object": "response", "model": model, "status": "completed",
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
}

func writeSSEEvent(w http.ResponseWriter, evt map[string]any) {
	b, _ := json.Marshal(evt)
	_, _ = w.Write([]byte("event: "))
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n"))
}
