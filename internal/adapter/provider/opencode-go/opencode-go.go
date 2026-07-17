// Package opencodegoexec ports the OpenCode Go executor.
package opencodegoexec

import (
	"encoding/json"
	"net/http"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// Executor extends BaseExecutor for OpenCode Go.
type Executor struct {
	*base.BaseExecutor
	lastModel string
}

// New creates an OpenCode Go executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("opencode-go", cfg)}
}

// BuildURL selects the /messages or /chat/completions endpoint based on model.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	e.lastModel = model
	baseURL := "https://opencode.ai/zen/go/v1"
	if e.Config.BaseURL != "" {
		baseURL = e.Config.BaseURL
		if idx := len(baseURL) - len("/chat/completions"); idx > 0 && baseURL[idx:] == "/chat/completions" {
			baseURL = baseURL[:idx]
		}
	}
	if isOpenCodeGoMessagesModel(model) {
		return baseURL + "/messages"
	}
	return baseURL + "/chat/completions"
}

// BuildHeaders returns x-api-key auth for Claude-format models, otherwise Bearer.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	key := creds.APIKey
	if key == "" {
		key = creds.AccessToken
	}
	if isOpenCodeGoMessagesModel(e.lastModel) {
		h.Set("x-api-key", key)
		h.Set("anthropic-version", base.AnthropicAPIVersion)
	} else {
		h.Set("Authorization", "Bearer "+key)
	}
	if stream {
		h.Set("Accept", "text/event-stream")
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

func isOpenCodeGoMessagesModel(model string) bool {
	return model == "minimax-m3" || model == "minimax-m2.7" || model == "minimax-m2.5" ||
		model == "qwen3.7-max" || model == "qwen3.7-plus" || model == "qwen3.6-plus"
}
