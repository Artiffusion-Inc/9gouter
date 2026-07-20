// Package xiaomitokenplanexec ports the Xiaomi Tokenplan executor.
package xiaomitokenplanexec

import (
	"encoding/json"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	defexec "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/default"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Executor extends DefaultExecutor for Xiaomi Tokenplan.
type Executor struct {
	*defexec.DefaultExecutor
}

// New creates a Xiaomi Tokenplan executor.
func New(cfg base.Config) *Executor {
	return &Executor{DefaultExecutor: defexec.New("xiaomi-tokenplan", cfg)}
}

// BuildURL resolves the region-specific base URL and endpoint format.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	region, _ := creds.ProviderSpecificData["region"].(string)
	baseURLs := e.GetBaseUrls()
	baseURL := ""
	for _, u := range baseURLs {
		if strings.Contains(u, region) || region == "" {
			baseURL = u
			break
		}
	}
	if baseURL == "" {
		baseURL = "https://token-plan-sgp.xiaomimimo.com/v1"
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	format := "openai"
	if rt, ok := creds.ProviderSpecificData["runtimeTransport"].(map[string]any); ok {
		if f, ok := rt["format"].(string); ok {
			format = f
		}
	}
	if format == "claude" {
		return baseURL + "/anthropic/v1/messages"
	}
	return baseURL + "/chat/completions"
}

// TransformRequest passes the body through.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	return body, nil
}
