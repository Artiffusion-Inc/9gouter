// Package opencodeexec ports the OpenCode executor.
package opencodeexec

import (
	"encoding/json"
	"net/http"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Executor extends BaseExecutor for OpenCode.
type Executor struct {
	*base.BaseExecutor
	lastModel string
}

// New creates an OpenCode executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("opencode", cfg)}
}

// BuildURL selects the /messages or /chat/completions endpoint based on model.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	e.lastModel = model
	baseURL := e.Config.BaseURL
	if baseURL == "" {
		baseURL = "https://opencode.ai"
	}
	if isOpenCodeMessagesModel(model) {
		return baseURL + "/zen/v1/messages"
	}
	return baseURL + "/zen/v1/chat/completions"
}

// BuildHeaders returns the public auth header and client tag.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := e.BaseExecutor.BuildHeaders(creds, stream)
	base.SetHeaderExact(h, "Authorization", "Bearer public")
	base.SetHeaderExact(h, "x-opencode-client", "desktop")
	if stream {
		base.SetHeaderExact(h, "Accept", "text/event-stream")
	}
	return h
}

// TransformRequest injects reasoning_content placeholders.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	var m map[string]any
	if len(body) == 0 {
		m = map[string]any{}
	} else if err := json.Unmarshal(body, &m); err != nil {
		return body, nil
	}
	if messages, ok := m["messages"].([]any); ok {
		for _, raw := range messages {
			msg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if msg["role"] == "assistant" {
				if rc, ok := msg["reasoning_content"].(string); !ok || rc == "" {
					msg["reasoning_content"] = " "
				}
			}
		}
	}
	out, _ := json.Marshal(m)
	return out, nil
}

func isOpenCodeMessagesModel(model string) bool {
	return model == "minimax-m3" || model == "minimax-m2.7" || model == "minimax-m2.5" ||
		model == "qwen3.7-max" || model == "qwen3.7-plus" || model == "qwen3.6-plus"
}
