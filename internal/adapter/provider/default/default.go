// Package defaultexec ports the DefaultExecutor from open-sse/executors/default.js.
// The directory is "default" to match the plan, but the package name cannot be
// the Go keyword "default".
package defaultexec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// setHeaderExact assigns a header preserving the exact key casing, bypassing
// net/http's canonicalization. This is required to match the JS golden
// snapshots which keep original header casing (e.g. "x-api-key").
func setHeaderExact(h http.Header, k, v string) {
	h[k] = []string{v}
}

func delHeaderExact(h http.Header, k string) {
	delete(h, k)
}

// AppVersion mirrors package.json version used by Cline headers.
const AppVersion = "0.5.35"

// DefaultExecutor extends BaseExecutor with config-driven auth hooks.
type DefaultExecutor struct {
	*base.BaseExecutor
}

// New creates a DefaultExecutor for a provider.
func New(provider string, cfg base.Config) *DefaultExecutor {
	return &DefaultExecutor{BaseExecutor: base.NewBaseExecutor(provider, cfg)}
}

// HeaderHook overrides base to inject provider-specific static headers.
func (e *DefaultExecutor) HeaderHook(name string) func(http.Header, provider.Credentials) {
	switch name {
	case "clineHeaders":
		return e.clineHeaders
	case "kimiHeaders":
		return e.kimiHeaders
	case "claudeOverlay":
		return e.claudeOverlay
	case "kilocodeOrg":
		return e.kilocodeOrg
	}
	return nil
}

func (e *DefaultExecutor) clineHeaders(h http.Header, creds provider.Credentials) {
	token := creds.APIKey
	if token == "" {
		token = creds.AccessToken
	}
	if token != "" {
		setHeaderExact(h, "Authorization", "Bearer workos:"+strings.TrimPrefix(token, "workos:"))
	}
	setHeaderExact(h, "HTTP-Referer", "https://cline.bot")
	setHeaderExact(h, "X-Title", "Cline")
	setHeaderExact(h, "User-Agent", fmt.Sprintf("9Router/%s", AppVersion))
	setHeaderExact(h, "X-PLATFORM", runtime.GOOS)
	setHeaderExact(h, "X-PLATFORM-VERSION", "v24.18.0")
	setHeaderExact(h, "X-CLIENT-TYPE", "9router")
	setHeaderExact(h, "X-CLIENT-VERSION", AppVersion)
	setHeaderExact(h, "X-CORE-VERSION", AppVersion)
	setHeaderExact(h, "X-IS-MULTIROOT", "false")
}

func (e *DefaultExecutor) kimiHeaders(h http.Header, creds provider.Credentials) {
	setHeaderExact(h, "X-Msh-Platform", "9router")
	setHeaderExact(h, "X-Msh-Version", "2.1.2")
	setHeaderExact(h, "X-Msh-Device-Model", fmt.Sprintf("%s %s", runtime.GOOS, runtime.GOARCH))
	setHeaderExact(h, "X-Msh-Device-Id", fmt.Sprintf("kimi-%d", time.Now().UnixMilli()))
}

func (e *DefaultExecutor) claudeOverlay(h http.Header, creds provider.Credentials) {
	// The real cache is populated from incoming client headers; golden tests run
	// without a cache so the static config headers remain.
}

func (e *DefaultExecutor) kilocodeOrg(h http.Header, creds provider.Credentials) {
	if v, ok := creds.ProviderSpecificData["orgId"].(string); ok && v != "" {
		setHeaderExact(h, "X-Kilocode-OrganizationID", v)
	}
}

// BuildURL overrides base with default.js URL building.
func (e *DefaultExecutor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	if rt := e.resolveRuntimeTransport(creds); rt != nil && rt.BaseURL != "" {
		if rt.URLSuffix != "" {
			return rt.BaseURL + rt.URLSuffix
		}
		return rt.BaseURL
	}

	if e.Provider != "" && strings.HasPrefix(e.Provider, "openai-compatible-") {
		baseURL := base.OpenAICompatBase
		if v, ok := creds.ProviderSpecificData["baseUrl"].(string); ok && v != "" {
			baseURL = v
		}
		normalized := strings.TrimSuffix(baseURL, "/")
		path := "/chat/completions"
		if strings.Contains(e.Provider, "responses") {
			path = "/responses"
		}
		return normalized + path
	}
	if e.Provider != "" && strings.HasPrefix(e.Provider, "anthropic-compatible-") {
		baseURL := base.AnthropicCompatBase
		if v, ok := creds.ProviderSpecificData["baseUrl"].(string); ok && v != "" {
			baseURL = v
		}
		normalized := strings.TrimSuffix(baseURL, "/")
		return normalized + "/messages"
	}

	if e.Config.Format == "gemini" {
		action := "generateContent"
		if stream {
			action = "streamGenerateContent?alt=sse"
		}
		return fmt.Sprintf("%s/%s:%s", e.Config.BaseURL, model, action)
	}

	if e.Config.URLSuffix != "" {
		return e.Config.BaseURL + e.Config.URLSuffix
	}

	url := e.Config.BaseURL
	if url == "" {
		urls := e.GetBaseUrls()
		if urlIndex >= 0 && urlIndex < len(urls) {
			url = urls[urlIndex]
		} else if len(urls) > 0 {
			url = urls[0]
		}
	}

	if strings.Contains(url, "{accountId}") {
		accountID, _ := creds.ProviderSpecificData["accountId"].(string)
		if accountID == "" {
			panic(fmt.Sprintf("%s requires accountId in providerSpecificData", e.Provider))
		}
		url = strings.ReplaceAll(url, "{accountId}", accountID)
	}
	return url
}

func (e *DefaultExecutor) resolveRuntimeTransport(creds provider.Credentials) *base.RuntimeTransport {
	v, ok := creds.ProviderSpecificData["runtimeTransport"]
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	rt := &base.RuntimeTransport{}
	if s, ok := m["baseUrl"].(string); ok {
		rt.BaseURL = s
	}
	if s, ok := m["urlSuffix"].(string); ok {
		rt.URLSuffix = s
	}
	if h, ok := m["headers"].(map[string]any); ok {
		rt.Headers = make(map[string]string, len(h))
		for k, v2 := range h {
			if s2, ok := v2.(string); ok {
				rt.Headers[k] = s2
			}
		}
	}
	if a, ok := m["auth"].(map[string]any); ok {
		rt.Auth = mapAuthDescriptor(a)
	}
	return rt
}

func mapAuthDescriptor(a map[string]any) base.AuthDescriptor {
	var d base.AuthDescriptor
	if v, ok := a["combined"].(bool); ok {
		d.Combined = v
	}
	if v, ok := a["header"].(string); ok {
		d.Header = v
	}
	if v, ok := a["scheme"].(string); ok {
		d.Scheme = v
	}
	if v, ok := a["anthropicVersion"].(bool); ok {
		d.AnthropicVersion = v
	}
	return d
}

// BuildHeaders overrides base to apply auth hooks + default.js quirks.
func (e *DefaultExecutor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := http.Header{}
	setHeaderExact(h, "Content-Type", "application/json")

	for k, v := range e.Config.Headers {
		setHeaderExact(h, k, v)
	}

	desc := e.Config.Auth
	if desc.Header == "" && desc.APIKey == nil && desc.OAuth == nil {
		desc = e.resolveAuthDescriptor()
	}

	for _, hook := range desc.Hooks {
		if fn := e.HeaderHook(hook); fn != nil {
			fn(h, creds)
		}
	}

	e.applyAuth(h, desc, creds)

	// Anthropic-compatible third-party gateway cleanup.
	if e.Provider != "" && strings.HasPrefix(e.Provider, "anthropic-compatible-") {
		baseURL := ""
		if v, ok := creds.ProviderSpecificData["baseUrl"].(string); ok {
			baseURL = v
		}
		isOfficial := baseURL == "" || strings.Contains(baseURL, "api.anthropic.com")
		if !isOfficial {
			if creds.APIKey != "" && h.Get("Authorization") == "" {
				setHeaderExact(h, "Authorization", "Bearer "+creds.APIKey)
			}
			delHeaderExact(h, "anthropic-dangerous-direct-browser-access")
			delHeaderExact(h, "Anthropic-Dangerous-Direct-Browser-Access")
			delHeaderExact(h, "x-app")
			delHeaderExact(h, "X-App")
			for _, betaKey := range []string{"anthropic-beta", "Anthropic-Beta"} {
				if v := h.Get(betaKey); v != "" {
					parts := strings.Split(v, ",")
					var filtered []string
					for _, p := range parts {
						p = strings.TrimSpace(p)
						if p != "" && p != "claude-code-20250219" {
							filtered = append(filtered, p)
						}
					}
					if len(filtered) > 0 {
						setHeaderExact(h, betaKey, strings.Join(filtered, ","))
					} else {
						delHeaderExact(h, betaKey)
					}
				}
			}
		}
	}

	if stream {
		setHeaderExact(h, "Accept", "text/event-stream")
	}
	return h
}

func (e *DefaultExecutor) resolveAuthDescriptor() base.AuthDescriptor {
	if e.Provider != "" && strings.HasPrefix(e.Provider, "anthropic-compatible-") {
		return base.AuthDescriptor{
			APIKey: &base.AuthSpec{Header: "x-api-key", Scheme: "raw"},
			OAuth:  &base.AuthSpec{Header: "Authorization", Scheme: "bearer"},
			AnthropicVersion: true,
		}
	}
	if e.Config.Format == "claude" {
		return base.AuthDescriptor{
			Combined: true,
			Header:   "x-api-key",
			Scheme:   "raw",
			AnthropicVersion: true,
		}
	}
	return base.AuthDescriptor{Combined: true, Header: "Authorization", Scheme: "bearer"}
}

func (e *DefaultExecutor) applyAuth(h http.Header, desc base.AuthDescriptor, creds provider.Credentials) {
	if desc.Combined {
		token := creds.APIKey
		if token == "" {
			token = creds.AccessToken
		}
		header := desc.Header
		scheme := desc.Scheme
		if header == "" {
			header = "Authorization"
			scheme = "bearer"
		}
		if scheme == "bearer" {
			value := token
			if value == "" {
				value = "undefined"
			}
			setHeaderExact(h, header, "Bearer "+value)
		} else {
			setHeaderExact(h, header, token)
		}
		if desc.AnthropicVersion && h.Get("anthropic-version") == "" && h.Get("Anthropic-Version") == "" {
			setHeaderExact(h, "anthropic-version", base.AnthropicAPIVersion)
		}
		return
	}
	if creds.APIKey != "" && desc.APIKey != nil {
		if desc.APIKey.Scheme == "bearer" {
			setHeaderExact(h, desc.APIKey.Header, "Bearer "+creds.APIKey)
		} else {
			setHeaderExact(h, desc.APIKey.Header, creds.APIKey)
		}
	} else if creds.AccessToken != "" && desc.OAuth != nil {
		if desc.OAuth.Scheme == "bearer" {
			setHeaderExact(h, desc.OAuth.Header, "Bearer "+creds.AccessToken)
		} else {
			setHeaderExact(h, desc.OAuth.Header, creds.AccessToken)
		}
	}
	if desc.AnthropicVersion && h.Get("anthropic-version") == "" && h.Get("Anthropic-Version") == "" {
		setHeaderExact(h, "anthropic-version", base.AnthropicAPIVersion)
	}
}

// TransformRequest applies default.js transformations.
func (e *DefaultExecutor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	var v any
	if len(body) == 0 {
		v = map[string]any{}
	} else if err := json.Unmarshal(body, &v); err != nil {
		return body, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return body, nil
	}

	// Apply json_schema -> json_object fallback for openai-compatible providers.
	if strings.HasPrefix(e.Provider, "openai-compatible-") {
		m = e.applyJSONSchemaFallback(m)
	}

	// Drop client_metadata quirk.
	if e.Config.Quirks.DropClientMetadata {
		delete(m, "client_metadata")
	}

	// Inject reasoning placeholder when configured.
	if e.Config.ReasoningInject != nil {
		m = e.injectReasoning(m, e.Config.ReasoningInject.Scope)
	} else {
		m = e.applyModelReasoningInject(model, m)
	}

	out, err := json.Marshal(m)
	if err != nil {
		return body, err
	}
	return out, nil
}

func (e *DefaultExecutor) applyJSONSchemaFallback(body map[string]any) map[string]any {
	rf, ok := body["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_schema" {
		return body
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		return body
	}
	schema, ok := js["schema"]
	if !ok {
		return body
	}
	schemaJSON, _ := json.MarshalIndent(schema, "", "  ")
	prompt := "You must respond with valid JSON that strictly follows this JSON schema:\n```json\n" + string(schemaJSON) + "\n```\nRespond ONLY with the JSON object, no other text."

	var messages []any
	if raw, ok := body["messages"].([]any); ok {
		messages = make([]any, len(raw))
		copy(messages, raw)
	}
	var sys map[string]any
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] == "system" {
			sys = msg
			break
		}
	}
	if sys != nil {
		switch c := sys["content"].(type) {
		case string:
			sys["content"] = c + "\n\n" + prompt
		case []any:
			sys["content"] = append(c, map[string]any{"type": "text", "text": "\n\n" + prompt})
		}
	} else {
		messages = append([]any{map[string]any{"role": "system", "content": prompt}}, messages...)
	}
	body["messages"] = messages
	body["response_format"] = map[string]any{"type": "json_object"}
	return body
}

func (e *DefaultExecutor) injectReasoning(body map[string]any, scope string) map[string]any {
	raw, ok := body["messages"].([]any)
	if !ok {
		return body
	}
	out := make([]any, len(raw))
	for i, m := range raw {
		msg, ok := m.(map[string]any)
		if !ok {
			out[i] = m
			continue
		}
		if msg["role"] != "assistant" {
			out[i] = msg
			continue
		}
		rc, _ := msg["reasoning_content"].(string)
		if rc != "" {
			out[i] = msg
			continue
		}
		if shouldInjectReasoning(msg, scope) {
			msg["reasoning_content"] = " "
		}
		out[i] = msg
	}
	body["messages"] = out
	return body
}

func shouldInjectReasoning(msg map[string]any, scope string) bool {
	if scope == "toolCalls" {
		tc, ok := msg["tool_calls"].([]any)
		return ok && len(tc) > 0
	}
	return true
}

// applyModelReasoningInject applies the model/provider-level reasoning inject
// rule when no static transport.reasoningInject is configured on the provider
// (Config.ReasoningInject == nil). It mirrors the JS reasoningContentInjector:
//   - a provider-level rule keyed on the provider id (the registry single
//     source: PROVIDERS[provider].reasoningInject), then
//   - a model-level rule matched by predicate against the model id.
//
// The provider key is the primary signal: the legacy Go code keyed only on the
// upstream-model prefix ("kimi-"), which silently missed Kimi models whose
// upstream id does not start with "kimi-" (e.g. an alias or a bare id like
// "k2.5"). Keying on e.Provider fixes the silent strip reported in
// decolua/9router #2690. The model-prefix fallback preserves the prior
// behavior for providers routed through a generic executor without a kimi
// provider id (deepseek model-name matching).
func (e *DefaultExecutor) applyModelReasoningInject(model string, body map[string]any) map[string]any {
	// Provider-level rules (registry single source). Mirrors the JS
	// PROVIDERS[provider].reasoningInject transport field.
	switch e.Provider {
	case "kimi", "kimi-coding":
		return e.injectReasoning(body, "toolCalls")
	}
	// Model-level rules: matched by predicate against the model id (fallback
	// when no provider-level rule applies).
	if strings.HasPrefix(model, "kimi-") {
		return e.injectReasoning(body, "toolCalls")
	}
	if strings.Contains(strings.ToLower(model), "deepseek") {
		return e.injectReasoning(body, "all")
	}
	return body
}
