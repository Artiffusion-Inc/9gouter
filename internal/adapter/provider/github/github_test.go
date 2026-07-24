// Package githubexec ports the GitHub Copilot executor.
package githubexec

import (
	"net/http"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// github_test.go ports the routing half of decolua/9router 542a088c: Claude
// models (detected by name pattern) route to Copilot's Anthropic-native
// /v1/messages shim (BaseURLs index 2), while gpt/gemini/grok models stay on
// the default chat/completions endpoint. Verified against the real Executor
// built from the registry config — no mock executor.

// githubConfig is the same BaseURLs ordering the registry carries for "github"
// (registry.go): [chat/completions, responses, /v1/messages]. Re-declared here
// so the test pins the routing to the real URL list without importing the whole
// provider registry.
func githubConfig() base.Config {
	return base.Config{
		BaseURL:  "https://api.githubcopilot.com/chat/completions",
		BaseURLs: []string{"https://api.githubcopilot.com/chat/completions", "https://api.githubcopilot.com/responses", "https://api.githubcopilot.com/v1/messages"},
	}
}

func TestIsClaudeModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"claude-opus-4-8", true},
		{"claude-sonnet-4-5", true},
		{"Claude-Haiku-4.5", true}, // case-insensitive, like the JS /claude/i regex
		{"claude-opus-4-8(high)", true},
		{"anthropic/claude-opus-4-8", true},
		{"gpt-5.4", false},
		{"gemini-3-pro", false},
		{"grok-4", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsClaudeModel(c.model); got != c.want {
			t.Errorf("IsClaudeModel(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}

func TestBuildURL_ClaudeRoutesToMessagesShim(t *testing.T) {
	e := New(githubConfig())
	creds := provider.Credentials{}
	urls := e.GetBaseUrls()
	want := urls[messagesURLIndex] // https://api.githubcopilot.com/v1/messages
	if got := e.BuildURL("claude-opus-4-8", true, 0, creds); got != want {
		t.Errorf("claude BuildURL = %q, want %q (the /v1/messages shim)", got, want)
	}
	// Claude routing must override the default urlIndex=0 chat/completions URL —
	// otherwise the shim is never selected and cache tokens never surface.
	if got := e.BuildURL("claude-opus-4-8", true, 0, creds); got == urls[0] {
		t.Errorf("claude routed to chat/completions %q — shim selection broken", got)
	}
	// Thinking-suffix model name still routes to the shim (suffix is appended by
	// the UI; the regex must match through it).
	if got := e.BuildURL("claude-opus-4-8(high)", true, 0, creds); got != want {
		t.Errorf("claude(high) BuildURL = %q, want %q", got, want)
	}
}

func TestBuildURL_NonClaudeStaysOnChatCompletions(t *testing.T) {
	e := New(githubConfig())
	creds := provider.Credentials{}
	urls := e.GetBaseUrls()
	// gpt/gemini/grok models keep the default fallback chain (urlIndex selects the
	// endpoint); they must NOT be forced onto the /v1/messages shim.
	for _, model := range []string{"gpt-5.4", "gpt-5.3-codex", "gemini-3-pro", "grok-4"} {
		if got := e.BuildURL(model, true, 0, creds); got != urls[0] {
			t.Errorf("non-claude %q BuildURL = %q, want default %q", model, got, urls[0])
		}
		// urlIndex=1 (responses) is still honored for non-claude models.
		if got := e.BuildURL(model, true, 1, creds); got != urls[1] {
			t.Errorf("non-claude %q urlIndex=1 BuildURL = %q, want %q", model, got, urls[1])
		}
	}
}

func TestBuildURL_ClaudeShimFallbackWhenBaseURLsMissing(t *testing.T) {
	// A config without the messages entry (only chat/completions + responses)
	// must not panic — Claude routing falls back to the default chain so the
	// request still reaches Copilot instead of crashing the chat path.
	e := New(base.Config{
		BaseURL:  "https://api.githubcopilot.com/chat/completions",
		BaseURLs: []string{"https://api.githubcopilot.com/chat/completions", "https://api.githubcopilot.com/responses"},
	})
	creds := provider.Credentials{}
	if got := e.BuildURL("claude-opus-4-8", true, 0, creds); got != "https://api.githubcopilot.com/chat/completions" {
		t.Errorf("claude fallback BuildURL = %q, want chat/completions", got)
	}
}

// TestBuildHeaders_AnthropicVersionAlwaysSet verifies the anthropic-version
// header (required by /v1/messages, harmless on the other routes) is present for
// every request — the JS 542a088c note this is a no-op on /chat/completions.
//
// The github executor sets headers via base.SetHeaderExact, which stores them
// under their exact (lower-case) map key rather than the textproto-canonicalized
// key http.Header.Get looks up — so read them with direct map access, mirroring
// how the real http round-tripper serializes them.
func TestBuildHeaders_AnthropicVersionAlwaysSet(t *testing.T) {
	e := New(githubConfig())
	h := e.BuildHeaders(provider.Credentials{}, true)
	if got := headerExact(h, "anthropic-version"); got != base.AnthropicAPIVersion {
		t.Errorf("anthropic-version = %q, want %q", got, base.AnthropicAPIVersion)
	}
	// Copilot IDE fingerprint headers are the Copilot shim's required identity.
	for _, k := range []string{"copilot-integration-id", "editor-version", "editor-plugin-version", "user-agent", "x-github-api-version"} {
		if headerExact(h, k) == "" {
			t.Errorf("missing required Copilot header %q", k)
		}
	}
}

// headerExact reads a header value via direct map access (the form
// SetHeaderExact stores under), not http.Header.Get which canonicalizes the key.
func headerExact(h http.Header, k string) string {
	if vs := h[k]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}
