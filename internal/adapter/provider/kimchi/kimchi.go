// Package kimchiexec ports the Kimchi executor.
package kimchiexec

import (
	"encoding/json"
	"strings"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	defexec "github.com/Artiffusion-Inc/9router/internal/adapter/provider/default"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
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
