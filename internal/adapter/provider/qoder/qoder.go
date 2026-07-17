// Package qoderexec ports the Qoder executor.
package qoderexec

import (
	"net/http"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// Executor extends BaseExecutor for Qoder.
type Executor struct {
	*base.BaseExecutor
}

// New creates a Qoder executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("qoder", cfg)}
}

// BuildURL returns the Qoder chat generation URL.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	url := e.Config.BaseURL
	if url == "" {
		url = "https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation"
	}
	return url
}

// BuildHeaders returns a deterministic set of Qoder request headers.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := e.BaseExecutor.BuildHeaders(creds, stream)
	h.Set("X-Qoder-Client", "qodercli")
	h.Set("X-Qoder-Version", "3")
	h.Set("X-Cosy-Signature", "cosy-placeholder")
	return h
}
