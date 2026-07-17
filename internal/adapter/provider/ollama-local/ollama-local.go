// Package ollamalocalexec ports the Ollama Local executor.
package ollamalocalexec

import (
	"strings"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	defexec "github.com/Artiffusion-Inc/9router/internal/adapter/provider/default"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// Executor extends DefaultExecutor for Ollama Local.
type Executor struct {
	*defexec.DefaultExecutor
}

// New creates an Ollama Local executor.
func New(cfg base.Config) *Executor {
	return &Executor{DefaultExecutor: defexec.New("ollama-local", cfg)}
}

// BuildURL resolves the Ollama host from credentials or default localhost.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	host := "http://localhost:11434"
	if v, ok := creds.ProviderSpecificData["baseUrl"].(string); ok && v != "" {
		host = strings.TrimSuffix(v, "/")
	}
	if e.Config.BaseURL != "" {
		host = strings.TrimSuffix(e.Config.BaseURL, "/api/chat")
	}
	return host + "/api/chat"
}
