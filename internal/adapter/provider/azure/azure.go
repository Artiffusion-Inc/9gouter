// Package azureexec ports the Azure executor.
package azureexec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// Executor extends BaseExecutor for Azure OpenAI.
type Executor struct {
	*base.BaseExecutor
}

// New creates an Azure executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("azure", cfg)}
}

// BuildURL builds the Azure OpenAI deployment-scoped URL.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	endpoint := os.Getenv("AZURE_ENDPOINT")
	apiVersion := os.Getenv("AZURE_API_VERSION")
	deployment := os.Getenv("AZURE_DEPLOYMENT")

	psd := creds.ProviderSpecificData
	if v, ok := psd["azureEndpoint"].(string); ok && v != "" {
		endpoint = v
	}
	if v, ok := psd["apiVersion"].(string); ok && v != "" {
		apiVersion = v
	}
	if v, ok := psd["deployment"].(string); ok && v != "" {
		deployment = v
	}
	if endpoint == "" {
		endpoint = "https://api.openai.com"
	}
	if apiVersion == "" {
		apiVersion = "2024-10-01-preview"
	}
	if deployment == "" {
		if model == "" {
			deployment = "gpt-4"
		} else {
			deployment = model
		}
	}
	endpoint = strings.TrimSuffix(endpoint, "/")
	return fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s", endpoint, deployment, apiVersion)
}

// BuildHeaders sets the Azure api-key header and optional organization header.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := http.Header{}
	base.SetHeaderExact(h, "Content-Type", "application/json")

	apiKey := creds.APIKey
	if apiKey == "" {
		apiKey = creds.AccessToken
	}
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey != "" {
		base.SetHeaderExact(h, "api-key", apiKey)
	}

	org := os.Getenv("AZURE_ORGANIZATION")
	if v, ok := creds.ProviderSpecificData["organization"].(string); ok && v != "" {
		org = v
	}
	if org != "" {
		base.SetHeaderExact(h, "OpenAI-Organization", org)
	}

	if stream {
		base.SetHeaderExact(h, "Accept", "text/event-stream")
	}
	return h
}

// TransformRequest passes the body through unchanged.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	return body, nil
}
