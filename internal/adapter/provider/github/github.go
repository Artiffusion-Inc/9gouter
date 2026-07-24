// Package githubexec ports the GitHub Copilot executor.
package githubexec

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"regexp"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Executor extends BaseExecutor for GitHub Copilot.
type Executor struct {
	*base.BaseExecutor
}

// New creates a GitHub Copilot executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("github", cfg)}
}

// claudeModelRe detects Claude models by name pattern. Mirrors decolua/9router
// 542a088c (isClaudeModel): Claude models get routed to Copilot's Anthropic-native
// /v1/messages shim — the only Copilot endpoint that surfaces prompt-cache token
// counts for Claude. The check is by NAME, not a registry field: Copilot's live
// model catalog regularly exposes claude-* variants ahead of the static registry.
var claudeModelRe = regexp.MustCompile(`(?i)claude`)

// IsClaudeModel reports whether model should be routed through Copilot's
// Anthropic-native /v1/messages shim. Exported so the proxychat routing seam can
// share the same name-pattern detection (it decides the target format = Claude).
func IsClaudeModel(model string) bool {
	return claudeModelRe.MatchString(model)
}

// messagesURLIndex is the BaseURLs slot for the /v1/messages shim. The registry
// (registry.go "github") carries [chat/completions, responses, /v1/messages] in
// that order, so index 2 is the Anthropic-native endpoint.
const messagesURLIndex = 2

// messagesURL returns the Anthropic-native /v1/messages shim URL from the
// executor's BaseURLs, or "" when the config lacks that entry.
func (e *Executor) messagesURL() string {
	urls := e.GetBaseUrls()
	if messagesURLIndex >= 0 && messagesURLIndex < len(urls) {
		return urls[messagesURLIndex]
	}
	return ""
}

// BuildURL selects the upstream endpoint. Claude models route to Copilot's
// Anthropic-native /v1/messages shim (542a088c); every other model stays on the
// default chat/completions (or the fallback chain) the BaseExecutor already
// walks. The shim is what makes the OpenAI→Claude request translator inject
// cache_control and lets cached_tokens surface in the response — /chat/completions
// never sees cache tokens for Claude.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	urls := e.GetBaseUrls()
	if IsClaudeModel(model) {
		if messagesURLIndex >= 0 && messagesURLIndex < len(urls) {
			return urls[messagesURLIndex]
		}
	}
	if urlIndex >= 0 && urlIndex < len(urls) {
		return urls[urlIndex]
	}
	if len(urls) > 0 {
		return urls[0]
	}
	return e.Config.BaseURL
}

// Execute routes Claude models to Copilot's Anthropic-native /v1/messages shim
// (decolua/9router 542a088c). Go's embedded-method promotion means an overridden
// BuildURL is NOT dispatched from the promoted BaseExecutor.Execute (the base
// method calls BaseExecutor.BuildURL statically), so overriding BuildURL alone
// is not enough — we intercept Execute, point a throwaway BaseExecutor at the
// /v1/messages URL only, and delegate the real fetch (retry/auth/proxy-aware) to
// it. By the time Execute runs, proxychat has already translated the request
// body OpenAI→Claude (resolveTargetFormat returns format.Claude for github Claude
// models), so cache_control is already injected; this executor only needs to
// pick the right endpoint. Non-Claude models fall through to the base executor.
func (e *Executor) Execute(ctx context.Context, req provider.ExecRequest) (provider.Resp, error) {
	if !IsClaudeModel(req.Model) {
		return e.BaseExecutor.Execute(ctx, req)
	}
	messagesURL := e.messagesURL()
	if messagesURL == "" {
		// Config lacks the shim entry — fall back to the default route rather
		// than crashing the chat path (mirrors the JS guard).
		return e.BaseExecutor.Execute(ctx, req)
	}
	// Shallow-copy this executor with a Config that exposes only the /v1/messages
	// endpoint (index 0). The cloned BaseExecutor shares Fetch/ProxyOpts/Logger
	// (the per-request wiring) but owns its own BaseURLs slice, so concurrent
	// requests on the original executor are not mutated. Headers/auth come from
	// the same Config + the github BuildHeaders override applied below.
	shim := *e.BaseExecutor
	shimCfg := e.Config
	shimCfg.BaseURL = messagesURL
	shimCfg.BaseURLs = []string{messagesURL}
	shim.Config = shimCfg
	// Re-wrap so the github BuildHeaders/BuildURL overrides still apply (the
	// shim executor keeps the github header fingerprint + anthropic-version).
	wrapped := &Executor{BaseExecutor: &shim}
	return wrapped.BaseExecutor.Execute(ctx, req)
}

// BuildHeaders returns Copilot IDE headers plus anthropic-version for messages route.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := e.BaseExecutor.BuildHeaders(creds, stream)
	base.SetHeaderExact(h, "copilot-integration-id", "vscode-chat")
	base.SetHeaderExact(h, "editor-version", "vscode/1.110.0")
	base.SetHeaderExact(h, "editor-plugin-version", "copilot-chat/0.38.0")
	base.SetHeaderExact(h, "user-agent", "GitHubCopilotChat/0.38.0")
	base.SetHeaderExact(h, "openai-intent", "conversation-panel")
	base.SetHeaderExact(h, "x-github-api-version", "2025-04-01")
	base.SetHeaderExact(h, "x-request-id", randHex(16))
	base.SetHeaderExact(h, "x-vscode-user-agent-library-version", "electron-fetch")
	base.SetHeaderExact(h, "X-Initiator", "user")
	// Harmless no-op on /chat/completions and /responses; required by /v1/messages
	// (the Anthropic-native Claude shim, 542a088c).
	base.SetHeaderExact(h, "anthropic-version", base.AnthropicAPIVersion)
	return h
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
