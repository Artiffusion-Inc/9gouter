// Package codexec ports the OpenAI Codex executor.
package codexec

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Executor extends BaseExecutor for Codex.
type Executor struct {
	*base.BaseExecutor
	isCompact bool
	sessionID string
}

// New creates a Codex executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("codex", cfg)}
}

// BuildURL appends /compact when the request is marked compact.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	url := e.BaseExecutor.BuildURL(model, stream, urlIndex, creds)
	if e.isCompact {
		return url + "/compact"
	}
	return url
}

// BuildHeaders adds Codex identity headers.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := e.BaseExecutor.BuildHeaders(creds, stream)
	if e.sessionID == "" {
		if v, ok := creds.ProviderSpecificData["sessionId"].(string); ok && v != "" {
			e.sessionID = v
		} else {
			e.sessionID = "session-default"
		}
	}
	base.SetHeaderExact(h, "session_id", e.sessionID)
	if h.Get("originator") == "" {
		base.SetHeaderExact(h, "originator", "codex_cli_rs")
	}
	accountID := ""
	if v, ok := creds.ProviderSpecificData["workspaceId"].(string); ok && v != "" {
		accountID = v
	}
	if accountID == "" {
		if v, ok := creds.ProviderSpecificData["chatgptAccountId"].(string); ok && v != "" {
			accountID = v
		}
	}
	if accountID == "" {
		if v, ok := creds.ProviderSpecificData["accountId"].(string); ok && v != "" {
			accountID = v
		}
	}
	if accountID != "" && h.Get("ChatGPT-Account-ID") == "" {
		base.SetHeaderExact(h, "ChatGPT-Account-ID", accountID)
	}
	return h
}

// TransformRequest normalizes the Codex request shape.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	var m map[string]any
	if len(body) == 0 {
		m = map[string]any{}
	} else if err := json.Unmarshal(body, &m); err != nil {
		return body, nil
	}
	if compact, ok := m["_compact"].(bool); ok {
		e.isCompact = compact
		delete(m, "_compact")
	}
	if sid, ok := m["session_id"].(string); ok && sid != "" {
		e.sessionID = sid
	}
	if e.sessionID == "" {
		if sid, ok := creds.ProviderSpecificData["sessionId"].(string); ok {
			e.sessionID = sid
		}
	}

	if input, ok := m["input"].(string); ok {
		m["input"] = []any{map[string]any{"type": "message", "role": "user", "content": input}}
	}
	if arr, ok := m["input"].([]any); ok && len(arr) == 0 {
		m["input"] = []any{map[string]any{"type": "message", "role": "user", "content": "..."}}
	}
	if _, ok := m["input"].([]any); !ok {
		m["input"] = []any{map[string]any{"type": "message", "role": "user", "content": "..."}}
	}

	// Convert role=system to role=developer for cacheable prefix.
	if arr, ok := m["input"].([]any); ok {
		for _, it := range arr {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := item["role"].(string); role == "system" {
				item["role"] = "developer"
			}
		}
	}

	m["stream"] = true
	if instructions, ok := m["instructions"].(string); !ok || strings.TrimSpace(instructions) == "" {
		m["instructions"] = ""
	}
	m["store"] = false
	if e.sessionID != "" {
		m["prompt_cache_key"] = e.sessionID
	}

	// Model mapping: strip effort suffix and keep upstream id.
	effort := ""
	for _, level := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
		if strings.HasSuffix(model, "-"+level) {
			effort = level
			model = strings.TrimSuffix(model, "-"+level)
			break
		}
	}
	m["model"] = model

	if reasoning, ok := m["reasoning"].(map[string]any); !ok || reasoning == nil {
		m["reasoning"] = map[string]any{"effort": effortOrDefault(effort, "low"), "summary": "auto"}
	} else {
		if reasoning["effort"] == nil && reasoning["effort"] == "" {
			reasoning["effort"] = effortOrDefault(effort, "low")
		}
		if reasoning["summary"] == nil || reasoning["summary"] == "" {
			reasoning["summary"] = "auto"
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

	// Strip unsupported params.
	for _, key := range []string{"temperature", "top_p", "frequency_penalty", "presence_penalty", "logprobs", "top_logprobs", "n", "seed", "max_tokens", "max_completion_tokens", "max_output_tokens", "user", "prompt_cache_retention", "metadata", "stream_options", "safety_identifier", "previous_response_id"} {
		delete(m, key)
	}

	out, err := json.Marshal(m)
	if err != nil {
		return body, err
	}
	return out, nil
}

func effortOrDefault(v, def string) string {
	if v == "" {
		return def
	}
	if v == "max" {
		return "xhigh"
	}
	return v
}
