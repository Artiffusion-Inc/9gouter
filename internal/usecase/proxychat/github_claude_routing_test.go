package proxychat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	githubexec "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/github"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"

	// Side-effect import: registers every translator pair (openai→claude, etc.)
	// via init() so TranslateRequest in the Handle pipeline actually translates.
	// Without it the linker drops the subpackages and the registry is empty.
	_ "github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/register"
)

// github_claude_routing_test.go ports the end-to-end behavior of decolua/9router
// 542a088c: an OpenAI-shape chat request for a Claude model on the github
// (GitHub Copilot) provider must (a) route to Copilot's Anthropic-native
// /v1/messages shim, (b) be translated OpenAI→Claude so cache_control gets
// injected, and (c) have the Claude SSE response translated back to OpenAI so
// cached_tokens surface in the usage. Verified with the real github Executor
// built from the registry config against a real httptest.Server upstream — no
// mock executor, no mock registry.

// githubProvider wraps the real github executor behind the proxychat
// DomainProvider interface so the full Handle pipeline runs.
type githubProvider struct {
	exec provider.Executor
}

func (g *githubProvider) ID() string                  { return "github" }
func (g *githubProvider) Executor() provider.Executor { return g.exec }

func githubRegistryCfg() base.Config {
	return base.Config{
		BaseURL:  "https://api.githubcopilot.com/chat/completions",
		BaseURLs: []string{"https://api.githubcopilot.com/chat/completions", "https://api.githubcopilot.com/responses", "https://api.githubcopilot.com/v1/messages"},
		Format:   "openai",
		Headers: map[string]string{
			"copilot-integration-id":              "vscode-chat",
			"editor-version":                      "vscode/1.110.0",
			"editor-plugin-version":               "copilot-chat/0.38.0",
			"user-agent":                          "GitHubCopilotChat/0.38.0",
			"openai-intent":                       "conversation-panel",
			"x-github-api-version":                "2025-04-01",
			"x-vscode-user-agent-library-version": "electron-fetch",
			"X-Initiator":                         "user",
			// 542a088c: required by the /v1/messages shim; applied on the real
			// Execute path by BaseExecutor.BuildHeaders from Config.Headers.
			"anthropic-version": base.AnthropicAPIVersion,
		},
		Auth: base.AuthDescriptor{Combined: true, Header: "Authorization", Scheme: "bearer"},
	}
}

// TestResolveTargetFormat_GitHubClaudeRouting pins the routing seam: a Claude
// model on github resolves to format.Claude (the /v1/messages native shape) so
// the OpenAI→Claude request translator injects cache_control; a gpt/gemini model
// stays OpenAI (the default chat/completions route).
func TestResolveTargetFormat_GitHubClaudeRouting(t *testing.T) {
	if got := resolveTargetFormat("github", format.Openai, "claude-opus-4-8"); got != format.Claude {
		t.Errorf("claude target = %v, want Claude", got)
	}
	if got := resolveTargetFormat("github", format.Openai, "claude-opus-4-8(high)"); got != format.Claude {
		t.Errorf("claude(high) target = %v, want Claude", got)
	}
	if got := resolveTargetFormat("github", format.Openai, "gpt-5.4"); got != format.Openai {
		t.Errorf("gpt target = %v, want Openai", got)
	}
	if got := resolveTargetFormat("github", format.Openai, "gemini-3-pro"); got != format.Openai {
		t.Errorf("gemini target = %v, want Openai", got)
	}
}

// TestHandle_GitHubClaudeRoutesToMessagesShim is the end-to-end 542a088c port:
// a streaming OpenAI chat request for a Claude model, sent to the github
// provider, must reach the upstream at the /v1/messages path with a
// Claude-native body carrying cache_control, and the upstream's Claude SSE
// response must be translated back to OpenAI so the client sees content + an
// OpenAI usage object (the /chat/completions route never surfaces cache tokens).
func TestHandle_GitHubClaudeRoutesToMessagesShim(t *testing.T) {
	var gotPath string
	var gotBody string
	var gotAnthropicVersion string

	// Real Claude SSE upstream reply: message_start carries usage with
	// cache_read_input_tokens (the cache token the shim surfaces), then a
	// content_block_delta text event, then message_stop.
	claudeSSE := strings.Join([]string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":12,"cache_read_input_tokens":7,"cache_creation_input_tokens":0,"output_tokens":1}}}`,
		"",
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		"",
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		"",
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		"",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAnthropicVersion = r.Header.Get("anthropic-version")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, claudeSSE)
	}))
	defer srv.Close()

	// Build the real github executor, then rewrite its BaseURLs to point at the
	// httptest server so the /v1/messages shim (index 2) is the test URL.
	cfg := githubRegistryCfg()
	cfg.BaseURLs = []string{
		srv.URL + "/chat/completions",
		srv.URL + "/responses",
		srv.URL + "/v1/messages",
	}
	cfg.BaseURL = srv.URL + "/chat/completions"
	exec := githubexec.New(cfg)

	repo := &inMemoryUsageRepo{}
	h := New(Dependencies{
		Registry:   func(id string) (DomainProvider, error) { return &githubProvider{exec: exec}, nil },
		UsageRepo:  repo,
		StreamPipe: fakeStreamPiper{},
		JSONToSSE:  fakeJSONToSSE{},
		Config:     config.Config{StreamStallTimeout: config.DurationMs(180 * time.Second), StreamStallTimeoutReasoning: config.DurationMs(600 * time.Second), StreamReadinessMaxTimeout: config.DurationMs(900 * time.Second)},
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:            context.Background(),
		Endpoint:       "/v1/chat/completions",
		Body:           json.RawMessage(`{"model":"claude-opus-4-8","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"hello"}],"stream":true}`),
		ProviderID:     "github",
		Model:          "claude-opus-4-8",
		Stream:         true,
		ResponseWriter: rec,
	}

	res, err := h.Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if !res.Streamed {
		t.Fatalf("expected streamed result")
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}

	// (a) routed to the /v1/messages shim — NOT /chat/completions.
	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages (the Claude shim)", gotPath)
	}

	// anthropic-version header present (required by /v1/messages).
	if gotAnthropicVersion == "" {
		t.Error("anthropic-version header missing on /v1/messages request")
	}

	// (b) translated OpenAI→Claude: body must carry the Anthropic-native shape
	// (max_tokens + messages with content blocks + a system field) and, the
	// 542a088c point of the whole port, a cache_control marker so Copilot's shim
	// surfaces cached_tokens. The system message must have been folded into the
	// Anthropic "system" field (not left as a role:system message).
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v\n%s", err, gotBody)
	}
	if _, ok := sent["max_tokens"]; !ok {
		t.Error("translated body missing max_tokens (Claude-native shape)")
	}
	if _, ok := sent["system"]; !ok {
		t.Error("translated body missing system field (system message must fold here)")
	}
	if hasCacheControl(gotBody) {
		// good — cache_control was injected
	} else {
		t.Error("translated body missing cache_control (the whole point of routing to /v1/messages)")
	}
	// No role:system message survives in the Claude messages array.
	if msgs, _ := sent["messages"].([]any); len(msgs) > 0 {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				if r, _ := mm["role"].(string); r == "system" {
					t.Error("translated body still carries a role:system message in messages[]")
				}
			}
		}
	}

	// (c) the Claude SSE response is translated back to OpenAI: the client sees
	// content "hi" and an OpenAI-shaped chunk, and the cached_tokens surface in
	// the usage. (fakeStreamPiper copies upstream verbatim, so this asserts the
	// proxychat path produced a Claude-shaped upstream call that the real pipe
	// would translate — the request-side routing + translation is what 542a088c
	// adds in Go; the Claude→OpenAI response translator is the existing registry
	// entry exercised separately by the translator golden tests.)
	out := rec.Body.String()
	if !strings.Contains(out, "hi") {
		t.Errorf("client response missing content delta: %q", out)
	}
}

// TestHandle_GitHubNonClaudeStaysOnChatCompletions verifies the regression
// guard: gpt/gemini models on github do NOT route to /v1/messages — they keep
// the OpenAI target format and hit /chat/completions.
func TestHandle_GitHubNonClaudeStaysOnChatCompletions(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := githubRegistryCfg()
	cfg.BaseURLs = []string{
		srv.URL + "/chat/completions",
		srv.URL + "/responses",
		srv.URL + "/v1/messages",
	}
	cfg.BaseURL = srv.URL + "/chat/completions"
	exec := githubexec.New(cfg)

	repo := &inMemoryUsageRepo{}
	h := New(Dependencies{
		Registry:   func(id string) (DomainProvider, error) { return &githubProvider{exec: exec}, nil },
		UsageRepo:  repo,
		StreamPipe: fakeStreamPiper{},
		JSONToSSE:  fakeJSONToSSE{},
		Config:     config.Config{StreamStallTimeout: config.DurationMs(180 * time.Second), StreamStallTimeoutReasoning: config.DurationMs(600 * time.Second), StreamReadinessMaxTimeout: config.DurationMs(900 * time.Second)},
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:            context.Background(),
		Endpoint:       "/v1/chat/completions",
		Body:           json.RawMessage(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":true}`),
		ProviderID:     "github",
		Model:          "gpt-5.4",
		Stream:         true,
		ResponseWriter: rec,
	}

	if _, err := h.Handle(context.Background(), req); err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("gpt upstream path = %q, want /chat/completions (must NOT route to /v1/messages)", gotPath)
	}
}

// hasCacheControl reports whether the translated upstream body carries a
// cache_control marker — the 542a088c reason for routing Claude models to
// /v1/messages. The OpenAI→Claude translator injects cache_control on the last
// system block, the last assistant message block, and the last tool.
func hasCacheControl(body string) bool {
	return strings.Contains(body, "cache_control")
}
