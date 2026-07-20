// Package qwenexec ports the Qwen Code executor.
package qwenexec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	defexec "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/default"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Executor extends DefaultExecutor for Qwen Code.
type Executor struct {
	*defexec.DefaultExecutor
}

// New creates a Qwen executor.
func New(cfg base.Config) *Executor {
	return &Executor{DefaultExecutor: defexec.New("qwen", cfg)}
}

// BuildURL resolves the Qwen resource URL from credentials.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	host := "portal.qwen.ai"
	if v, ok := creds.ProviderSpecificData["resourceUrl"].(string); ok && v != "" {
		host = strings.TrimPrefix(v, "https://")
		host = strings.TrimPrefix(host, "http://")
		host = strings.TrimSuffix(host, "/")
	}
	return fmt.Sprintf("https://%s/v1/chat/completions", host)
}

// BuildHeaders returns the Qwen static fingerprint.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := http.Header{}
	token := creds.APIKey
	if token == "" {
		token = creds.AccessToken
	}
	base.SetHeaderExact(h, "Content-Type", "application/json")
	base.SetHeaderExact(h, "Authorization", "Bearer "+token)
	base.SetHeaderExact(h, "User-Agent", "QwenCode/0.12.3 (linux; x64)")
	base.SetHeaderExact(h, "X-DashScope-AuthType", "qwen-oauth")
	base.SetHeaderExact(h, "X-DashScope-CacheControl", "enable")
	base.SetHeaderExact(h, "X-DashScope-UserAgent", "QwenCode/0.12.3 (linux; x64)")
	base.SetHeaderExact(h, "X-Stainless-Arch", "x64")
	base.SetHeaderExact(h, "X-Stainless-Lang", "js")
	base.SetHeaderExact(h, "X-Stainless-Os", "Linux")
	base.SetHeaderExact(h, "X-Stainless-Package-Version", "5.11.0")
	base.SetHeaderExact(h, "X-Stainless-Retry-Count", "1")
	base.SetHeaderExact(h, "X-Stainless-Runtime", "node")
	base.SetHeaderExact(h, "X-Stainless-Runtime-Version", "v18.19.1")
	base.SetHeaderExact(h, "Connection", "keep-alive")
	base.SetHeaderExact(h, "Accept-Language", "*")
	base.SetHeaderExact(h, "Sec-Fetch-Mode", "cors")
	if stream {
		base.SetHeaderExact(h, "Accept", "text/event-stream")
	} else {
		base.SetHeaderExact(h, "Accept", "application/json")
	}
	return h
}

var defaultSystem = map[string]any{
	"role": "system",
	"content": []any{map[string]any{"type": "text", "text": "", "cache_control": map[string]any{"type": "ephemeral"}}},
}

// TransformRequest injects Qwen system message and usage options.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	transformed, err := e.DefaultExecutor.TransformRequest(model, body, stream, creds)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(transformed, &m); err != nil {
		return transformed, nil
	}
	messages, _ := m["messages"].([]any)
	messages = append([]any{defaultSystem}, messages...)
	m["messages"] = messages
	if stream && m["stream_options"] == nil && !isThinkingActive(m) {
		m["stream_options"] = map[string]any{"include_usage": true}
	}
	if tc, ok := m["tool_choice"]; ok && isThinkingActive(m) {
		switch tc.(type) {
		case string:
			if tc == "required" {
				m["tool_choice"] = "auto"
			}
		case map[string]any:
			m["tool_choice"] = "auto"
		}
	}
	out, _ := json.Marshal(m)
	return out, nil
}

func isThinkingActive(m map[string]any) bool {
	if m["thinking"] == true || m["enable_thinking"] == true {
		return true
	}
	if t, ok := m["thinking"].(map[string]any); ok {
		return t["type"] == "enabled"
	}
	return false
}
