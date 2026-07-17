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
