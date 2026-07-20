// Package vertexexec ports the Vertex executor.
package vertexexec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Executor extends BaseExecutor for Vertex AI.
type Executor struct {
	*base.BaseExecutor
}

// New creates a Vertex executor.
func New(providerID string, cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor(providerID, cfg)}
}

// BuildURL builds the Vertex AI or Vertex Partner URL.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	projectID, _ := creds.ProviderSpecificData["projectId"].(string)
	location, _ := creds.ProviderSpecificData["location"].(string)
	if location == "" {
		location = "us-central1"
	}
	apiKey := creds.APIKey
	hasOAuth := creds.AccessToken != "" || isVertexServiceAccount(apiKey) || isVertexADC(apiKey)

	if e.Provider == "vertex-partner" {
		if projectID == "" {
			panic("Vertex partner models require a project_id")
		}
		url := fmt.Sprintf("https://aiplatform.googleapis.com/v1/projects/%s/locations/global/endpoints/openapi/chat/completions", projectID)
		if apiKey != "" && !hasOAuth {
			url += "?key=" + apiKey
		}
		return url
	}

	action := "generateContent"
	if stream {
		action = "streamGenerateContent"
	}

	if hasOAuth {
		if projectID == "" {
			panic("Vertex OAuth/ADC requires a project_id")
		}
		url := fmt.Sprintf("https://aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:%s", projectID, location, model, action)
		if stream {
			url += "?alt=sse"
		}
		return url
	}

	url := fmt.Sprintf("https://aiplatform.googleapis.com/v1/publishers/google/models/%s:%s", model, action)
	if stream {
		url += "?alt=sse"
	}
	if apiKey != "" {
		if strings.Contains(url, "?") {
			url += "&key=" + apiKey
		} else {
			url += "?key=" + apiKey
		}
	}
	return url
}

// BuildHeaders returns Vertex headers.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if creds.AccessToken != "" {
		h.Set("Authorization", "Bearer "+creds.AccessToken)
	}
	if stream {
		h.Set("Accept", "text/event-stream")
	}
	return h
}

// TransformRequest passes the body through.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	return body, nil
}

func isVertexServiceAccount(apiKey string) bool {
	if !strings.HasPrefix(strings.TrimSpace(apiKey), "{") {
		return false
	}
	var v struct {
		Type string `json:"type"`
	}
	json.Unmarshal([]byte(apiKey), &v)
	return v.Type == "service_account"
}

func isVertexADC(apiKey string) bool {
	if !strings.HasPrefix(strings.TrimSpace(apiKey), "{") {
		return false
	}
	var v struct {
		Type string `json:"type"`
	}
	json.Unmarshal([]byte(apiKey), &v)
	return v.Type == "authorized_user"
}
