package proxychat

import (
	"bytes"
	"encoding/json"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

// passthroughSanitizer returns a Pipe PassthroughSanitizer callback for the
// same-format (sourceFormat == targetFormat) streaming case, mirroring the JS
// stream.js passthrough-frame sanitization that raw byte passthrough otherwise
// lacks. It returns nil for formats with no known upstream quirk to correct,
// preserving byte-for-byte passthrough (the historical behaviour and what
// TestPipePassthrough asserts).
//
// Covered upstream fixes:
//   - c22f11de: skip non-JSON "data:" lines so upstream plain-text/HTML garbage
//     does not break downstream JSON decoders; track [DONE] so it is not
//     duplicated by the EOF emitter.
//   - 602ee405: strip empty "tool_calls":[] arrays from OpenAI Chat deltas
//     that trigger premature reasoning-end in @ai-sdk/openai-compatible.
//   - a9785a5f: track OpenAI Responses terminal events so EOF does not
//     double-emit [DONE] and passthrough streams ending with response.done are
//     not flagged incomplete.
func passthroughSanitizer(sourceFormat, targetFormat format.Format) func([]byte, map[string]any) ([][]byte, error) {
	if sourceFormat != targetFormat {
		return nil
	}
	switch sourceFormat {
	case format.Openai:
		// OpenAI Chat Completions passthrough: strip empty tool_calls (602ee405)
		// and skip non-JSON data lines (c22f11de). No EOF [DONE] emission here
		// (the OpenAI Chat stream carries its own [DONE] from upstream; the
		// Pipe's EmitDoneOnEOF is set only for Responses passthrough).
		return openaiChatPassthroughSanitizer
	case format.OpenaiResponses:
		// OpenAI Responses passthrough: track terminal events for [DONE] dedup
		// (a9785a5f) and skip non-JSON data lines (c22f11de). The Pipe emits
		// the trailing [DONE] on EOF via EmitDoneOnEOF.
		return openaiResponsesPassthroughSanitizer
	default:
		// Claude/Gemini/Cursor/Kiro/etc. same-format passthrough has no known
		// upstream quirk to correct; keep raw byte passthrough.
		return nil
	}
}

// openaiChatPassthroughSanitizer sanitizes a de-framed OpenAI Chat Completions
// passthrough frame (one or more "data: ...\n" lines, possibly with an
// "event:" prefix and a trailing blank). It strips empty tool_calls arrays
// (upstream 602ee405), skips non-JSON data lines (upstream c22f11de), and
// records the inline [DONE] sentinel in state["streamDoneSent"] so the EOF
// emitter does not duplicate it.
func openaiChatPassthroughSanitizer(frame []byte, state map[string]any) ([][]byte, error) {
	return sanitizeOpenAISSEFrame(frame, state, true)
}

// openaiResponsesPassthroughSanitizer sanitizes a de-framed OpenAI Responses
// passthrough frame. It tracks terminal events (response.completed/done/failed
// and error — upstream a9785a5f) in state["streamDoneSent"], skips non-JSON
// data lines (upstream c22f11de), and records an inline [DONE] sentinel.
func openaiResponsesPassthroughSanitizer(frame []byte, state map[string]any) ([][]byte, error) {
	return sanitizeOpenAISSEFrame(frame, state, false)
}

// sanitizeOpenAISSEFrame is the shared per-frame sanitizer. chatMode selects
// the OpenAI Chat-specific transform (empty tool_calls stripping); Responses
// mode instead tracks terminal events by their "type" field.
func sanitizeOpenAISSEFrame(frame []byte, state map[string]any, chatMode bool) ([][]byte, error) {
	lines := splitSSELines(frame)
	var out bytes.Buffer
	for _, line := range lines {
		trimmed := bytes.TrimSpace(line)
		// Preserve blank separators and non-data lines (e.g. "event:") verbatim.
		if len(trimmed) == 0 {
			out.WriteByte('\n')
			continue
		}
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			out.Write(line)
			if !bytes.HasSuffix(line, []byte("\n")) {
				out.WriteByte('\n')
			}
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		// Inline [DONE] sentinel: record it and forward verbatim.
		if bytes.Equal(payload, []byte("[DONE]")) {
			state["streamDoneSent"] = true
			out.Write(line)
			if !bytes.HasSuffix(line, []byte("\n")) {
				out.WriteByte('\n')
			}
			continue
		}
		// Non-JSON data lines (plain-text/HTML errors injected into the SSE
		// stream) would break downstream JSON decoders — skip them silently
		// (upstream c22f11de). Keep a minimal guard: a JSON object starts with
		// '{'; anything else is treated as non-JSON.
		if len(payload) == 0 || payload[0] != '{' {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal(payload, &parsed); err != nil {
			// Not valid JSON — skip (c22f11de).
			continue
		}
		if chatMode {
			parsed = stripEmptyToolCalls(parsed)
		} else {
			// Responses mode: track terminal events (a9785a5f).
			if isResponsesTerminalEvent(parsed) {
				state["streamDoneSent"] = true
			}
		}
		reencoded, err := json.Marshal(parsed)
		if err != nil {
			// Should not happen for a successfully parsed object; forward
			// the original payload to avoid dropping data.
			out.Write(line)
			if !bytes.HasSuffix(line, []byte("\n")) {
				out.WriteByte('\n')
			}
			continue
		}
		out.WriteString("data: ")
		out.Write(reencoded)
		out.WriteByte('\n')
	}
	// Re-terminate the frame with a trailing blank line, matching the SSE
	// framing the historical raw passthrough produced.
	if !bytes.HasSuffix(out.Bytes(), []byte("\n\n")) {
		// Ensure exactly one trailing blank line. The splitSSELines loop wrote
		// per-line newlines; append a final newline if the buffer does not end
		// with a blank line.
		if bytes.HasSuffix(out.Bytes(), []byte("\n")) && !bytes.HasSuffix(out.Bytes(), []byte("\n\n")) {
			out.WriteByte('\n')
		} else if !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
			out.WriteByte('\n')
			out.WriteByte('\n')
		}
	}
	frameOut := out.Bytes()
	if len(bytes.TrimSpace(frameOut)) == 0 {
		// Sanitizer dropped every line (e.g. all non-JSON). Emit nothing but
		// keep the writer alive, mirroring translateOrPassthrough's empty-out
		// contract.
		return nil, nil
	}
	return [][]byte{frameOut}, nil
}

// splitSSELines splits a de-framed SSE frame into its constituent lines,
// preserving the trailing newline of each. A frame is the bytes between two
// "\n\n" boundaries; it may contain "event:", "data:", and blank lines.
func splitSSELines(frame []byte) [][]byte {
	return bytes.SplitAfter(frame, []byte("\n"))
}

// stripEmptyToolCalls removes empty "tool_calls":[] arrays from OpenAI Chat
// streaming deltas (upstream 602ee405). @ai-sdk/openai-compatible treats
// delta.tool_calls != null as a tool-call signal; an empty array passes that
// check and triggers premature reasoning-end on every reasoning chunk. Real
// tool_calls always have at least one element, so stripping is zero-side-effect.
func stripEmptyToolCalls(parsed map[string]any) map[string]any {
	choices, ok := parsed["choices"].([]any)
	if !ok {
		return parsed
	}
	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		tc, ok := delta["tool_calls"].([]any)
		if !ok || len(tc) > 0 {
			continue
		}
		delete(delta, "tool_calls")
	}
	return parsed
}

// isResponsesTerminalEvent reports whether a parsed OpenAI Responses chunk is a
// terminal event that signals stream completion (upstream a9785a5f /
// responsesStreamHelpers.js OPENAI_RESPONSES_TERMINAL_EVENTS).
func isResponsesTerminalEvent(parsed map[string]any) bool {
	t, _ := parsed["type"].(string)
	switch t {
	case "response.completed", "response.done", "response.failed", "error":
		return true
	}
	return false
}
