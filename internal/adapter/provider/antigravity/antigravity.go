// Package antigravityexec ports the Antigravity executor.
package antigravityexec

import (
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Executor extends BaseExecutor with Antigravity URL building.
type Executor struct {
	*base.BaseExecutor
}

// New creates an Antigravity executor. It wires the 639f1204 transient-retry
// hook (computeRetryDelay) into the base executor via Config.ComputeRetryDelay
// — the embedded-method override does not dispatch from the promoted
// BaseExecutor.Execute (see #142), so the hook rides on the config field.
func New(cfg base.Config) *Executor {
	if cfg.ComputeRetryDelay == nil {
		cfg.ComputeRetryDelay = antigravityComputeRetryDelay
	}
	return &Executor{BaseExecutor: base.NewBaseExecutor("antigravity", cfg)}
}

// BuildURL returns the v1internal generate/streamGenerateContent endpoint.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	baseURLs := e.GetBaseUrls()
	url := baseURLs[urlIndex]
	if url == "" {
		url = baseURLs[0]
	}
	url = strings.TrimSuffix(url, "/")
	action := "generateContent"
	if stream {
		action = "streamGenerateContent?alt=sse"
	}
	return url + "/v1internal:" + action
}
