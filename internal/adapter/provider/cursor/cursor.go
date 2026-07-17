// Package cursorexec ports the Cursor executor.
package cursorexec

import (
	"encoding/json"
	"net/http"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// Executor extends BaseExecutor for Cursor.
type Executor struct {
	*base.BaseExecutor
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
