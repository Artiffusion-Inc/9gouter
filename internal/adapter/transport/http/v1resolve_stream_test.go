package http

import (
	"net/http"
	"testing"
)

// TestResolveStream ports upstream c842dc8f: a stream-only provider
// (forceStream) must keep stream=true even for JSON clients that did not
// request streaming. Pre-fix the empty forceStream map meant no provider ever
// hit the short-circuit, so a JSON client on codex/commandcode/grok-cli/
// codebuddy-cn/openai had stream flipped to false and the stream-only upstream
// mis-handled the request.
func TestResolveStream(t *testing.T) {
	jsonAccept := http.Header{}
	jsonAccept.Set("Accept", "application/json")

	sseAccept := http.Header{}
	sseAccept.Set("Accept", "text/event-stream")

	emptyAccept := http.Header{}

	cases := []struct {
		name      string
		body      string
		headers   http.Header
		provider  string
		wantStream bool
	}{
		// forceStream providers: always true regardless of client/Accept/body.
		{"codex forceStream, JSON client, no stream field", `{"model":"x"}`, jsonAccept, "codex", true},
		{"codex forceStream, JSON client, stream=false", `{"model":"x","stream":false}`, jsonAccept, "codex", true},
		{"commandcode forceStream, JSON client", `{"model":"x"}`, jsonAccept, "commandcode", true},
		{"grok-cli forceStream, JSON client", `{"model":"x"}`, jsonAccept, "grok-cli", true},
		{"codebuddy-cn forceStream, JSON client", `{"model":"x"}`, jsonAccept, "codebuddy-cn", true},
		{"openai forceStream, JSON client", `{"model":"x"}`, jsonAccept, "openai", true},
		{"codex alias 'codex' resolves", `{"model":"x"}`, jsonAccept, "codex", true},

		// Non-forceStream provider: JSON client without stream=true → false.
		{"non-force provider, JSON client, no stream → false", `{"model":"x"}`, jsonAccept, "claude", false},
		{"non-force provider, JSON client, stream=false → false", `{"model":"x","stream":false}`, jsonAccept, "claude", false},
		{"non-force provider, JSON client, stream=true → true", `{"model":"x","stream":true}`, jsonAccept, "claude", true},

		// SSE/empty Accept: stream follows the body field (default true).
		{"non-force provider, SSE accept, no stream → true", `{"model":"x"}`, sseAccept, "claude", true},
		{"non-force provider, empty accept, stream=false → false", `{"model":"x","stream":false}`, emptyAccept, "claude", false},
		{"non-force provider, empty accept, no stream → true (default)", `{"model":"x"}`, emptyAccept, "claude", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveStream([]byte(tc.body), tc.headers, tc.provider)
			if got != tc.wantStream {
				t.Fatalf("resolveStream(%q) = %v, want %v", tc.provider, got, tc.wantStream)
			}
		})
	}
}