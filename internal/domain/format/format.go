// Package format defines the LLM request/response format enum and endpoint-based
// format detection ported from open-sse/translator/formats.js.
package format

import (
	"encoding/json"
	"strings"
)

// Format identifies an upstream or client wire format.
type Format int

// Format values must stay synchronized with the JS FORMATS map and any
// code that serializes/deserializes format strings.
const (
	FormatUnknown Format = iota
	Openai
	OpenaiResponses
	OpenaiResponse
	Claude
	Gemini
	GeminiCli
	Vertex
	Codex
	Antigravity
	Kiro
	Cursor
	Ollama
	Commandcode
)

// strings maps each Format to its canonical JS string. OpenaiResponses
// ("openai-responses") and the singular OpenaiResponse ("openai-response")
// are both present because both forms are used in the JS code.
var formatStrings = map[Format]string{
	Openai:          "openai",
	OpenaiResponses: "openai-responses",
	OpenaiResponse:  "openai-response",
	Claude:          "claude",
	Gemini:          "gemini",
	GeminiCli:       "gemini-cli",
	Vertex:          "vertex",
	Codex:           "codex",
	Antigravity:     "antigravity",
	Kiro:            "kiro",
	Cursor:          "cursor",
	Ollama:          "ollama",
	Commandcode:     "commandcode",
}

// parseAliases maps accepted string aliases to Format values. Both
// "openai-responses" and "openai-response" are accepted as separate formats.
var parseAliases = map[string]Format{
	"openai":           Openai,
	"openai-responses": OpenaiResponses,
	"openai-response":  OpenaiResponse,
	"claude":           Claude,
	"gemini":           Gemini,
	"gemini-cli":       GeminiCli,
	"vertex":           Vertex,
	"codex":            Codex,
	"antigravity":      Antigravity,
	"kiro":             Kiro,
	"cursor":           Cursor,
	"ollama":           Ollama,
	"commandcode":      Commandcode,
}

// String returns the canonical string for the format. Unknown formats return
// an empty string.
func (f Format) String() string {
	if s, ok := formatStrings[f]; ok {
		return s
	}
	return ""
}

// Parse returns the Format for a string and reports whether the string was
// recognized. It accepts the canonical names only (no case folding), matching
// the JS FORMATS map usage.
func Parse(s string) (Format, bool) {
	f, ok := parseAliases[s]
	return f, ok
}

// DetectByEndpoint returns a format based on the request URL path and raw body.
// It ports detectFormatByEndpoint from open-sse/translator/formats.js exactly:
//   - /v1/responses -> OpenaiResponses
//   - /v1/messages  -> Claude
//   - /v1/chat/completions with body.input as an array -> Openai
//   - otherwise -> FormatUnknown (caller falls back to body-based detection)
func DetectByEndpoint(path string, body json.RawMessage) Format {
	if strings.Contains(path, "/v1/responses") {
		return OpenaiResponses
	}
	if strings.Contains(path, "/v1/messages") {
		return Claude
	}
	if strings.Contains(path, "/v1/chat/completions") {
		if isArray(body, "input") {
			return Openai
		}
	}
	return FormatUnknown
}

// isArray reports whether raw contains a top-level JSON array at key.
func isArray(raw json.RawMessage, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var wrapper struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return false
	}
	if len(wrapper.Input) == 0 {
		return false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(wrapper.Input, &arr); err != nil {
		return false
	}
	return true
}
