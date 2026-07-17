// Package grokcliexec ports the Grok CLI executor.
package grokcliexec

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// Executor extends BaseExecutor for Grok CLI.
type Executor struct {
	*base.BaseExecutor
	currentSessionID string
	currentReqID     string
	currentTurnIdx   int
	currentModel     string
}

// New creates a Grok CLI executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("grok-cli", cfg), currentTurnIdx: 1}
}

// BuildURL returns the base Codex Responses URL.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	url := e.Config.BaseURL
	if url == "" {
		url = "https://cli-chat-proxy.grok.com/v1/responses"
	}
	return url
}

// BuildHeaders adds Grok CLI identity headers.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := e.BaseExecutor.BuildHeaders(creds, stream)
	return h
}

// TransformRequest normalizes Grok CLI request shape.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	var m map[string]any
	if len(body) == 0 {
		m = map[string]any{}
	} else if err := json.Unmarshal(body, &m); err != nil {
		return body, nil
	}
	if sid, ok := m["session_id"].(string); ok && sid != "" {
		e.currentSessionID = sid
	}
	if e.currentSessionID == "" {
		if sid, ok := creds.ProviderSpecificData["sessionId"].(string); ok {
			e.currentSessionID = sid
		}
	}
	if e.currentSessionID == "" {
		e.currentSessionID = "session-placeholder"
	}
	e.currentReqID = "req-placeholder"

	if input, ok := m["input"].(string); ok {
		m["input"] = []any{map[string]any{"type": "message", "role": "user", "content": input}}
	}
	if arr, ok := m["input"].([]any); ok {
		if len(arr) == 0 {
			m["input"] = []any{map[string]any{"type": "message", "role": "user", "content": "..."}}
		}
	} else if messages, ok := m["messages"].([]any); ok && len(messages) > 0 {
		var input []any
		for _, raw := range messages {
			msg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			content := ""
			if c, ok := msg["content"].(string); ok {
				content = c
			} else {
				content = "..."
			}
			input = append(input, map[string]any{"type": "message", "role": msg["role"], "content": content})
		}
		m["input"] = input
		delete(m, "messages")
	} else {
		m["input"] = []any{map[string]any{"type": "message", "role": "user", "content": "..."}}
	}

	// Resolve model: strip effort suffix.
	effort := ""
	for _, level := range []string{"low", "medium", "high", "xhigh"} {
		if strings.HasSuffix(model, "-"+level) {
			effort = level
			model = strings.TrimSuffix(model, "-"+level)
			break
		}
	}
	if bodyModel, ok := m["model"].(string); ok && bodyModel != "" {
		model = bodyModel
	}
	e.currentModel = model
	m["model"] = model

	if reasoning, ok := m["reasoning"].(map[string]any); !ok || reasoning == nil {
		m["reasoning"] = map[string]any{"summary": "concise"}
		if effort != "" {
			m["reasoning"].(map[string]any)["effort"] = effort
		}
	} else {
		if reasoning["effort"] == nil || reasoning["effort"] == "" {
			if effort != "" {
				reasoning["effort"] = effort
			}
		}
		if reasoning["summary"] == nil || reasoning["summary"] == "" {
			reasoning["summary"] = "concise"
		}
	}
	delete(m, "reasoning_effort")

	if r, ok := m["reasoning"].(map[string]any); ok {
		if eff, _ := r["effort"].(string); eff != "" && eff != "none" {
			include, _ := m["include"].([]any)
			found := false
			for _, v := range include {
				if v == "reasoning.encrypted_content" {
					found = true
					break
				}
			}
			if !found {
				m["include"] = append(include, "reasoning.encrypted_content")
			}
		}
	}

	m["stream"] = true
	m["store"] = false

	for _, key := range []string{"messages", "max_tokens", "max_completion_tokens", "n", "seed", "logprobs", "top_logprobs", "frequency_penalty", "presence_penalty", "logit_bias", "user", "stream_options", "prompt_cache_retention", "safety_identifier", "previous_response_id"} {
		delete(m, key)
	}

	out, _ := json.Marshal(m)
	return out, nil
}
