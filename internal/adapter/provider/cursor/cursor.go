// Package cursorexec ports the Cursor executor.
package cursorexec

import (
	"encoding/json"
	"net/http"

	"golang.org/x/net/http2"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Executor extends BaseExecutor for Cursor.
type Executor struct {
	*base.BaseExecutor

	// agentTransport, when non-nil, overrides the direct HTTP/2 transport used
	// for the AgentService Run RPC. Tests inject an h2 transport whose
	// DialTLSContext targets an in-process httptest server; production leaves it
	// nil so OpenAgentSession uses a default direct transport.
	agentTransport *http2.Transport
	// agentBaseURL overrides the AgentService endpoint base URL
	// (https://agent.api5.cursor.sh by default). Tests point this at an
	// in-process h2 server.
	agentBaseURL string
}

// New creates a Cursor executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("cursor", cfg)}
}

// BuildURL returns baseURL + chatPath.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	url := e.Config.BaseURL
	if e.Config.URLSuffix != "" {
		url += e.Config.URLSuffix
	}
	return url
}

// BuildHeaders requires machineId and returns a deterministic Cursor header set.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := e.BaseExecutor.BuildHeaders(creds, stream)
	machineID, _ := creds.ProviderSpecificData["machineId"].(string)
	if machineID == "" {
		panic("Machine ID is required for Cursor API")
	}
	base.SetHeaderExact(h, "x-machine-id", machineID)
	base.SetHeaderExact(h, "x-cursor-client-version", "3.1.0")
	base.SetHeaderExact(h, "x-cursor-client-type", "ide")
	return h
}

// TransformRequest passes the already-translated body through.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	return body, nil
}
