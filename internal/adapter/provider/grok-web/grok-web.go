// Package grokwebexec ports the Grok Web executor.
package grokwebexec

import (
	"encoding/json"
	"net/http"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// Executor extends BaseExecutor for Grok Web.
type Executor struct {
	*base.BaseExecutor
}

// New creates a Grok Web executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("grok-web", cfg)}
}

// BuildURL returns the Grok web chat endpoint.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	url := e.Config.BaseURL
	if url == "" {
		url = "https://grok.com/rest/app-chat/conversations/new"
	}
	return url
}

// BuildHeaders returns the browser-like header set the web client uses.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := http.Header{}
	base.SetHeaderExact(h, "Accept", "*/*")
	base.SetHeaderExact(h, "Accept-Encoding", "gzip, deflate, br, zstd")
	base.SetHeaderExact(h, "Accept-Language", "en-US,en;q=0.9")
	base.SetHeaderExact(h, "Cache-Control", "no-cache")
	base.SetHeaderExact(h, "Content-Type", "application/json")
	base.SetHeaderExact(h, "Origin", "https://grok.com")
	base.SetHeaderExact(h, "Pragma", "no-cache")
	base.SetHeaderExact(h, "Referer", "https://grok.com/")
	base.SetHeaderExact(h, "Sec-Ch-Ua", `"Google Chrome";v="136", "Chromium";v="136", "Not(A:Brand";v="24"`)
	base.SetHeaderExact(h, "Sec-Ch-Ua-Mobile", "?0")
	base.SetHeaderExact(h, "Sec-Ch-Ua-Platform", `"macOS"`)
	base.SetHeaderExact(h, "Sec-Fetch-Dest", "empty")
	base.SetHeaderExact(h, "Sec-Fetch-Mode", "cors")
	base.SetHeaderExact(h, "Sec-Fetch-Site", "same-origin")
	base.SetHeaderExact(h, "User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36")
	base.SetHeaderExact(h, "x-statsig-id", "statsig-placeholder")
	base.SetHeaderExact(h, "x-xai-request-id", "xai-req-placeholder")
	base.SetHeaderExact(h, "traceparent", "00-trace-span-00")
	return h
}

// TransformRequest passes the body through.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	return body, nil
}
