// Package geminicliexec ports the Gemini CLI executor.
package geminicliexec

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Executor extends BaseExecutor for Gemini CLI.
type Executor struct {
	*base.BaseExecutor
	currentModel string
}

// New creates a Gemini CLI executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("gemini-cli", cfg)}
}

// BuildURL returns the model-scoped generate/stream endpoint.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	baseURL := e.Config.BaseURL
	if baseURL == "" {
		baseURL = "https://cloudcode-pa.googleapis.com/v1internal"
	}
	action := "generateContent"
	if stream {
		action = "streamGenerateContent?alt=sse"
	}
	return fmt.Sprintf("%s/models/%s:%s", baseURL, model, action)
}

// BuildHeaders returns Gemini CLI identity headers.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := e.BaseExecutor.BuildHeaders(creds, stream)
	base.SetHeaderExact(h, "X-Goog-Api-Client", "google-genai-sdk/1.41.0 gl-node/v22.19.0")
	if stream {
		base.SetHeaderExact(h, "Accept", "text/event-stream")
	} else {
		base.SetHeaderExact(h, "Accept", "application/json")
	}
	return h
}

// TransformRequest wraps the Gemini payload when needed.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	e.currentModel = model
	var m map[string]any
	if len(body) == 0 {
		return body, nil
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return body, nil
	}
	if _, ok := m["request"]; ok {
		if _, ok := m["model"]; ok {
			return body, nil
		}
	}
	projectID := ""
	if v, ok := creds.ProviderSpecificData["projectId"].(string); ok {
		projectID = v
	}
	if projectID == "" {
		if v, ok := m["project"].(string); ok {
			projectID = v
		}
	}
	out := map[string]any{
		"project": projectID,
		"model":   model,
		"request": m,
	}
	// Port upstream 7610f28f (#2486): emit toolConfig.functionCallingConfig.mode
	// = VALIDATED for Gemini CLI tool requests so the backend validates tool call
	// shapes instead of returning MALFORMED_FUNCTION_CALL. The JS wrapper writes
	// toolConfig into envelope.request; here `m` IS envelope.request, so the
	// toolConfig lands at request.toolConfig (matching the upstream placement).
	if hasGeminiTools(m) {
		m["toolConfig"] = map[string]any{
			"functionCallingConfig": map[string]any{"mode": "VALIDATED"},
		}
	}
	b, _ := json.Marshal(out)
	return b, nil
}

// hasGeminiTools reports whether the translated Gemini body carries a non-empty
// tools array (functionDeclarations), mirroring the `geminiCLI.tools?.length > 0`
// guard in openai-to-gemini.js wrapInCloudCodeEnvelope.
func hasGeminiTools(m map[string]any) bool {
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) == 0 {
		return false
	}
	return true
}
