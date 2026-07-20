// Package githubexec ports the GitHub Copilot executor.
package githubexec

import (
	"crypto/rand"
	"fmt"
	"net/http"

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

// BuildURL returns the chat completions URL by default.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	urls := e.GetBaseUrls()
	if urlIndex >= 0 && urlIndex < len(urls) {
		return urls[urlIndex]
	}
	if len(urls) > 0 {
		return urls[0]
	}
	return e.Config.BaseURL
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
	base.SetHeaderExact(h, "anthropic-version", base.AnthropicAPIVersion)
	return h
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
