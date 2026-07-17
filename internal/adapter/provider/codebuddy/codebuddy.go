// Package codebuddyexec ports the CodeBuddy-CN executor.
package codebuddyexec

import (
	"encoding/json"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	defexec "github.com/Artiffusion-Inc/9router/internal/adapter/provider/default"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// Executor extends DefaultExecutor with CodeBuddy-specific request transforms.
type Executor struct {
	*defexec.DefaultExecutor
}

// New creates a CodeBuddy executor.
func New(cfg base.Config) *Executor {
	return &Executor{DefaultExecutor: defexec.New("codebuddy-cn", cfg)}
}

// TransformRequest forces streaming and adds reasoning_summary when reasoning is requested.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	transformed, err := e.DefaultExecutor.TransformRequest(model, body, stream, creds)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(transformed, &m); err != nil {
		return transformed, nil
	}
	m["stream"] = true

	eff, _ := m["reasoning_effort"].(string)
	switch eff {
	case "none", "off":
		delete(m, "reasoning_effort")
	case "":
		// leave unset
	default:
		m["reasoning_summary"] = "auto"
	}

	out, _ := json.Marshal(m)
	return out, nil
}
