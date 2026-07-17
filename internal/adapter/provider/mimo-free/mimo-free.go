// Package mimofreeexec ports the MiMo Free executor.
package mimofreeexec

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

const mimoMarker = "You are MiMoCode, an interactive CLI tool that helps users with software engineering tasks."

// Executor extends BaseExecutor for MiMo Free.
type Executor struct {
	*base.BaseExecutor
	sessionID string
}

// New creates a MiMo Free executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("mimo-free", cfg), sessionID: "session-placeholder"}
}

// BuildURL returns the free chat endpoint.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	url := e.Config.BaseURL
	if url == "" {
		url = "https://api.xiaomimimo.com/api/free-ai/openai/chat"
	}
	return url
}

// BuildHeaders returns the MiMo Free anti-abuse headers.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := http.Header{}
	base.SetHeaderExact(h, "Content-Type", "application/json")
	base.SetHeaderExact(h, "X-Mimo-Source", "mimocode-cli-free")
	base.SetHeaderExact(h, "User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	base.SetHeaderExact(h, "x-session-affinity", e.sessionID)
	if stream {
		base.SetHeaderExact(h, "Accept", "text/event-stream")
	} else {
		base.SetHeaderExact(h, "Accept", "application/json")
	}
	return h
}

// TransformRequest injects the MiMo system marker.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	var m map[string]any
	if len(body) == 0 {
		m = map[string]any{}
	} else if err := json.Unmarshal(body, &m); err != nil {
		return body, nil
	}
	messages, ok := m["messages"].([]any)
	if !ok {
		messages = []any{}
	}
	hasMarker := false
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] == "system" {
			if content, ok := msg["content"].(string); ok && strings.Contains(content, mimoMarker) {
				hasMarker = true
				break
			}
		}
	}
	if !hasMarker {
		messages = append([]any{map[string]any{"role": "system", "content": mimoMarker}}, messages...)
		m["messages"] = messages
	}
	out, _ := json.Marshal(m)
	return out, nil
}
