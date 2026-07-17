package format

import (
	"encoding/json"
	"testing"
)

func TestDetectByEndpoint(t *testing.T) {
	cases := []struct {
		path string
		body string
		want Format
	}{
		{"/v1/responses", `null`, OpenaiResponses},
		{"/v1/messages", `null`, Claude},
		{"/v1/chat/completions", `{"input":[]}`, Openai},
		{"/v1/chat/completions", `{"messages":[]}`, FormatUnknown},
		{"/v1/unknown", `{}`, FormatUnknown},
	}

	for _, c := range cases {
		got := DetectByEndpoint(c.path, json.RawMessage(c.body))
		if got != c.want {
			t.Errorf("DetectByEndpoint(%q, %s) = %v, want %v", c.path, c.body, got, c.want)
		}
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Format
		ok   bool
	}{
		{"openai", Openai, true},
		{"openai-responses", OpenaiResponses, true},
		{"openai-response", OpenaiResponse, true},
		{"claude", Claude, true},
		{"gemini", Gemini, true},
		{"gemini-cli", GeminiCli, true},
		{"vertex", Vertex, true},
		{"codex", Codex, true},
		{"antigravity", Antigravity, true},
		{"kiro", Kiro, true},
		{"cursor", Cursor, true},
		{"ollama", Ollama, true},
		{"commandcode", Commandcode, true},
		{"unknown", FormatUnknown, false},
		{"", FormatUnknown, false},
	}

	for _, c := range cases {
		got, ok := Parse(c.in)
		if ok != c.ok {
			t.Errorf("Parse(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestFormat_String(t *testing.T) {
	for f, want := range map[Format]string{
		Openai:          "openai",
		OpenaiResponses: "openai-responses",
		OpenaiResponse:  "openai-response",
		Claude:          "claude",
		FormatUnknown:   "",
	} {
		if got := f.String(); got != want {
			t.Errorf("%v.String() = %q, want %q", f, got, want)
		}
	}
}
