// Package kimchiexec ports the Kimchi executor.
package kimchiexec

import (
	"encoding/json"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	defexec "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/default"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

var topLevelDrops = []string{
	"anthropic_version",
	"anthropic_beta",
	"client_metadata",
	"mcp_servers",
	"stop_sequences",
	"thinking",
	"top_k",
}

// Executor extends DefaultExecutor with Kimchi request cleanup.
type Executor struct {
	*defexec.DefaultExecutor
}

// New creates a Kimchi executor.
func New(cfg base.Config) *Executor {
	return &Executor{DefaultExecutor: defexec.New("kimchi", cfg)}
}

// TransformRequest applies Kimchi-specific cleanup.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	transformed, err := e.DefaultExecutor.TransformRequest(model, body, stream, creds)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(transformed, &m); err != nil {
		return transformed, nil
	}
	for _, k := range topLevelDrops {
		delete(m, k)
	}
	delete(m, "system")

	if isAnthropicBacked(model) {
		delete(m, "reasoning_effort")
		delete(m, "reasoning")
		delete(m, "thinking")
	} else {
		// Port upstream 8c068a1f: Kimi/kimchi ride SGLang backends whose
		// reasoning_effort enum only accepts low/medium/high/max. The client
		// (or an OpenAI-format upstream) may send auto/minimal/xhigh, which
		// SGLang rejects with HTTP 400. Normalize to the SGLang whitelist:
		// auto→high, minimal→low, xhigh→max, {low,medium,high,max} pass
		// through, any other value is dropped so the backend doesn't 400.
		if raw, ok := m["reasoning_effort"].(string); ok && raw != "" {
			if norm, ok := toKimiReasoningEffort(raw); ok {
				m["reasoning_effort"] = norm
			} else {
				delete(m, "reasoning_effort")
			}
		}
	}

	if messages, ok := m["messages"].([]any); ok {
		for _, raw := range messages {
			msg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			delete(msg, "cache_control")
			if content, ok := msg["content"].([]any); ok {
				for _, partRaw := range content {
					part, ok := partRaw.(map[string]any)
					if !ok {
						continue
					}
					delete(part, "cache_control")
					delete(part, "signature")
				}
			}
		}
	}
	if tools, ok := m["tools"].([]any); ok {
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			delete(tool, "cache_control")
		}
	}

	out, _ := json.Marshal(m)
	return out, nil
}

func isAnthropicBacked(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "claude") || strings.Contains(m, "anthropic")
}

// toKimiReasoningEffort normalizes a client reasoning_effort to the Kimi/kimchi
// SGLang backend enum {low, medium, high, max}, mirroring upstream 8c068a1f
// (thinkingUnified.toKimiReasoningEffort). Returns ok=false for values outside
// the whitelist so the caller drops the field instead of sending a value the
// backend rejects.
func toKimiReasoningEffort(level string) (string, bool) {
	switch strings.ToLower(level) {
	case "auto":
		return "high", true
	case "minimal":
		return "low", true
	case "xhigh":
		return "max", true
	case "low", "medium", "high", "max":
		return strings.ToLower(level), true
	}
	return "", false
}
