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

// BuildHeaders returns the Cursor ChatService header set for the legacy path
// (tool turns, dispatched via the inherited BaseExecutor.Execute).
//
// Upstream 6994cd1f bumped the shared x-cursor-client-version from 3.1.0 to
// 3.12.17 across both ChatService and AgentService and added the
// x-cursor-client-commit fingerprint header, so the gateway stops returning
// HTTP 429 "Update Required" for retired headers. The legacy path here only
// needs the same two-line bump on top of the Base header set (which already
// carries the connect-* / Content-Type / Authorization canonical headers from
// the registry config); pulling the full AgentService proto header set in
// would create canonical-vs-lowercase duplicates of Authorization/Content-Type.
// The AgentService path keeps its own BuildCursorHeaders set in execute.go.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := e.BaseExecutor.BuildHeaders(creds, stream)
	machineID, _ := creds.ProviderSpecificData["machineId"].(string)
	if machineID == "" {
		panic("Machine ID is required for Cursor API")
	}
	base.SetHeaderExact(h, "x-machine-id", machineID)
	base.SetHeaderExact(h, "x-cursor-client-version", cursorClientVersion)
	base.SetHeaderExact(h, "x-cursor-client-commit", cursorClientCommit)
	base.SetHeaderExact(h, "x-cursor-client-type", "ide")
	return h
}

// TransformRequest passes the already-translated body through.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	return body, nil
}
