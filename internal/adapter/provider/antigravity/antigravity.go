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

// New creates an Antigravity executor.
func New(cfg base.Config) *Executor {
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
