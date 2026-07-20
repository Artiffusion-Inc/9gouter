// Package perplexitywebexec ports the Perplexity Web executor.
package perplexitywebexec

import (
	"encoding/json"
	"net/http"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Executor extends BaseExecutor for Perplexity Web.
type Executor struct {
	*base.BaseExecutor
}

// New creates a Perplexity Web executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("perplexity-web", cfg)}
}

// BuildURL returns the Perplexity SSE ask endpoint.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	url := e.Config.BaseURL
	if url == "" {
		url = "https://www.perplexity.ai/rest/sse/perplexity_ask"
	}
	return url
}

// BuildHeaders returns browser-like Perplexity headers.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := http.Header{}
	base.SetHeaderExact(h, "Content-Type", "application/json")
	base.SetHeaderExact(h, "Accept", "text/event-stream")
	base.SetHeaderExact(h, "Origin", "https://www.perplexity.ai")
	base.SetHeaderExact(h, "Referer", "https://www.perplexity.ai/")
	base.SetHeaderExact(h, "User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36")
	base.SetHeaderExact(h, "X-App-ApiClient", "default")
	base.SetHeaderExact(h, "X-App-ApiVersion", "2.18")
	if creds.AccessToken != "" {
		base.SetHeaderExact(h, "Authorization", "Bearer "+creds.AccessToken)
	} else if creds.APIKey != "" {
		base.SetHeaderExact(h, "Cookie", "__Secure-next-auth.session-token="+creds.APIKey)
	}
	return h
}

// TransformRequest passes the body through.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	return body, nil
}
