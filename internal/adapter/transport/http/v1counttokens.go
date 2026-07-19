package http

import (
	"encoding/json"
	"io"
	"net/http"
)

// POST /v1/messages/count_tokens — Anthropic-compatible token-count estimate.
//
// Mirrors legacy JS src/app/api/v1/messages/count_tokens/route.js: a pure
// local estimate (chars/4) over the request body's system/tools/messages,
// with no upstream call. The JS route walked content blocks (text,
// tool_use, tool_result, thinking) and any nested object/array keys+values
// via countValueChars; we reproduce that walk in Go.
//
// Auth gate is the same as /v1/messages (requireApiKey + local bypass) so
// the SDK's preflight count request is gated consistently with the chat
// request that follows it.

// estimateAnthropicInputTokens computes the chars/4 token estimate for an
// Anthropic messages request body. Split out for unit testing.
func estimateAnthropicInputTokens(body []byte) int {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0
	}
	var total int
	if v, ok := raw["system"]; ok {
		total += countValueChars(v)
	}
	if v, ok := raw["tools"]; ok {
		total += countValueChars(v)
	}
	if v, ok := raw["messages"]; ok {
		var msgs []json.RawMessage
		if err := json.Unmarshal(v, &msgs); err == nil {
			for _, m := range msgs {
				total += countMessageChars(m)
			}
		}
	}
	return (total + 3) / 4 // ceil(total/4)
}

// countValueChars mirrors JS countValueChars: the character footprint of a
// JSON value, recursing into arrays (sum elements) and objects (sum keys +
// values). For scalars it returns the length of the JSON-lexed string form
// (string content, number/bool literal) — matching the JS path where
// strings contributed their content length and numbers/booleans their
// String() length.
func countValueChars(v json.RawMessage) int {
	if len(v) == 0 {
		return 0
	}
	trimmed := trimSpace(v)
	if len(trimmed) == 0 {
		return 0
	}
	// null
	if string(trimmed) == "null" {
		return 0
	}
	// string
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return len(s)
		}
		return len(trimmed)
	}
	// array
	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err == nil {
			sum := 0
			for _, e := range arr {
				sum += countValueChars(e)
			}
			return sum
		}
		return len(trimmed)
	}
	// object
	if trimmed[0] == '{' {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err == nil {
			sum := 0
			for k, val := range obj {
				sum += len(k) + countValueChars(val)
			}
			return sum
		}
		return len(trimmed)
	}
	// number/bool: JS used String(value).length; for JSON literals the byte
	// length of the literal matches the JS String() length for ints and
	// booleans (true=4, false=5). Use the literal length.
	return len(trimmed)
}

// countMessageChars mirrors JS countMessageChars: the footprint of a single
// message — ONLY its content. When content is a string, return its length;
// when it is an array, sum countContentBlockChars over the blocks; otherwise
// fall back to countValueChars(content). role and other keys are NOT counted
// (the JS function returns content.length / the block sum directly).
func countMessageChars(m json.RawMessage) int {
	if len(m) == 0 {
		return 0
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(m, &obj); err != nil {
		return countValueChars(m)
	}
	content, ok := obj["content"]
	if !ok {
		return 0
	}
	ct := trimSpace(content)
	if len(ct) == 0 {
		return 0
	}
	if ct[0] == '"' {
		var s string
		if err := json.Unmarshal(ct, &s); err == nil {
			return len(s)
		}
		return len(ct)
	}
	if ct[0] == '[' {
		var blocks []json.RawMessage
		if err := json.Unmarshal(ct, &blocks); err == nil {
			sum := 0
			for _, b := range blocks {
				sum += countContentBlockChars(b)
			}
			return sum
		}
	}
	return countValueChars(content)
}

// countContentBlockChars mirrors JS countContentBlockChars: per-type
// extraction of the heavy field.
func countContentBlockChars(b json.RawMessage) int {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return countValueChars(b)
	}
	typ, _ := unquoteString(obj["type"])
	switch typ {
	case "text":
		return countValueChars(obj["text"])
	case "tool_use":
		return countValueChars(obj["name"]) + countValueChars(obj["input"])
	case "tool_result":
		return countValueChars(obj["content"])
	case "thinking":
		return countValueChars(obj["thinking"])
	default:
		return countValueChars(b)
	}
}

func trimSpace(v []byte) []byte {
	for len(v) > 0 && (v[0] == ' ' || v[0] == '\t' || v[0] == '\n' || v[0] == '\r') {
		v = v[1:]
	}
	return v
}

func unquoteString(v json.RawMessage) (string, bool) {
	if len(v) == 0 || v[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", false
	}
	return s, true
}

func (h *v1Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Auth gate identical to /v1/messages.
	apiKey := extractAPIKey(r)
	requireKey, err := h.requireAPIKey(ctx)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Auth check failed")
		return
	}
	if requireKey || !isLocalRequest(r) {
		if apiKey == "" {
			h.writeError(w, http.StatusUnauthorized, "Missing API key")
			return
		}
		valid, err := h.deps.APIKeysRepo.Validate(ctx, apiKey)
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, "Auth check failed")
			return
		}
		if !valid {
			h.writeError(w, http.StatusUnauthorized, "Invalid API key")
			return
		}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	inputTokens := estimateAnthropicInputTokens(body)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"input_tokens": inputTokens,
	})
}