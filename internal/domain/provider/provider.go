// Package provider defines the Executor and Provider ports used to talk to
// upstream LLM services. It ports the interfaces and Credentials shape from
// open-sse/executors/base.js.
package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Credentials holds the auth material for an upstream provider connection.
// ProviderSpecificData carries per-provider extras such as baseUrl, proxy
// settings, and OAuth metadata.
type Credentials struct {
	APIKey               string
	AccessToken          string
	ExpiresAt            *time.Time
	ProviderSpecificData map[string]any
}

// ExecRequest is the input to Executor.Execute.
type ExecRequest struct {
	Model       string
	Body        json.RawMessage
	Stream      bool
	Credentials Credentials
}

// Resp is the result of a successful upstream call.
type Resp struct {
	Response       *http.Response
	URL            string
	Headers        http.Header
	TransformedBody json.RawMessage

	// Done, when non-nil, releases resources tied to the upstream call's
	// fetch context. The fetch context owns resp.Response.Body's lifetime for
	// streaming responses: cancelling it (as a doFetch defer would) closes the
	// body mid-stream and the Pipe only ever reads the first buffered chunk
	// before getting context.Canceled (the ollama/llama.cpp NDJSON 90s hang).
	// The caller MUST call Done after it has finished reading/closing
	// Response.Body, so the fetch context stays alive for the full stream.
	Done func()
}

// Executor is the per-provider port for building request URLs, headers,
// transforming request bodies, and executing upstream calls.
type Executor interface {
	BuildURL(model string, stream bool, urlIndex int, creds Credentials) string
	BuildHeaders(creds Credentials, stream bool) http.Header
	TransformRequest(model string, body json.RawMessage, stream bool, creds Credentials) (json.RawMessage, error)
	Execute(ctx context.Context, req ExecRequest) (Resp, error)
}

// Provider is a resolved provider instance. ID is the canonical provider ID
// used for logging, usage, and registry lookup.
type Provider interface {
	ID() string
	Executor() Executor
}

// Model is a static catalog entry for a provider, mirroring the JS registry
// `models: [{ id, name, kind?, upstreamModelId? }]` shape. Kind is the service
// kind ("llm"|"image"|"tts"|"embedding"|"stt"|"imageToText"|"video"|"webSearch"|
// "webFetch"); empty defaults to "llm". UpstreamModelID, when set, is the raw
// upstream model id the request body.model must be remapped to before the
// upstream call (e.g. blackbox exposes "claude-opus-4.8" but the upstream
// expects "blackboxai/anthropic/claude-opus-4.8"). Empty => use ID verbatim.
type Model struct {
	ID   string
	Name string
	Kind string
	// UpstreamModelID is the upstream model id to send when it differs from ID.
	UpstreamModelID string
}

// ProviderCatalog is the static, connection-independent metadata for a
// provider: its alias, static model list, and service kinds. It is the Go
// analog of open-sse/providers/registry/<provider>.js (the subset needed by
// GET /v1/models and kind filtering — display/notice/transport live in
// base.Config). serviceKinds empty defaults to ["llm"] per the JS
// getProvidersByKind convention.
type ProviderCatalog struct {
	ID           string
	Alias        string
	Models       []Model
	ServiceKinds []string
}
