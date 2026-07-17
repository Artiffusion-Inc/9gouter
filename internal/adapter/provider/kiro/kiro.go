// Package kiroexec ports the Kiro executor.
package kiroexec

import (
	"encoding/json"
	"net/http"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// Executor extends BaseExecutor for Kiro.
type Executor struct {
	*base.BaseExecutor
}

// New creates a Kiro executor.
func New(cfg base.Config) *Executor {
	return &Executor{BaseExecutor: base.NewBaseExecutor("kiro", cfg)}
}

// BuildURL reorders base URLs for API-key / external-idp / idc auth methods.
func (e *Executor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	baseURLs := e.GetBaseUrls()
	authMethod, _ := creds.ProviderSpecificData["authMethod"].(string)
	isCodeWhisperer := authMethod == "api_key" || authMethod == "external_idp" || authMethod == "idc"
	if !isCodeWhisperer {
		if urlIndex >= 0 && urlIndex < len(baseURLs) {
			return baseURLs[urlIndex]
		}
		return baseURLs[0]
	}
	region, _ := creds.ProviderSpecificData["region"].(string)
	if region == "" {
		region = "us-east-1"
	}
	var amazon []string
	var others []string
	for _, u := range baseURLs {
		if contains(u, "amazonaws.com") {
			if region != "us-east-1" {
				u = regionalize(u, region)
			}
			amazon = append(amazon, u)
		} else {
			others = append(others, u)
		}
	}
	ordered := append(amazon, others...)
	if len(ordered) == 0 {
		ordered = baseURLs
	}
	if urlIndex >= 0 && urlIndex < len(ordered) {
		return ordered[urlIndex]
	}
	return ordered[0]
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && indexOf(s, substr) >= 0
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func regionalize(u, region string) string {
	// Replace the middle region in *.[region].amazonaws.com style endpoints.
	parts := split(u, ".")
	for i := 0; i < len(parts)-2; i++ {
		if parts[i+2] == "amazonaws" {
			parts[i+1] = region
			break
		}
	}
	return join(parts, ".")
}

func split(s, sep string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s)-len(sep); i++ {
		if s[i:i+len(sep)] == sep {
			out = append(out, s[start:i])
			start = i + len(sep)
		}
	}
	out = append(out, s[start:])
	return out
}

func join(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for i := 1; i < len(parts); i++ {
		out += sep + parts[i]
	}
	return out
}

// BuildHeaders sets the Kiro auth and SDK headers.
func (e *Executor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := e.BaseExecutor.BuildHeaders(creds, stream)
	base.SetHeaderExact(h, "Accept", "application/vnd.amazon.eventstream")
	base.SetHeaderExact(h, "Amz-Sdk-Request", "attempt=1; max=3")
	base.SetHeaderExact(h, "Amz-Sdk-Invocation-Id", "invocation-placeholder")

	authMethod, _ := creds.ProviderSpecificData["authMethod"].(string)
	isApiKey := authMethod == "api_key"
	isExternalIdp := authMethod == "external_idp"
	apiKey := creds.APIKey
	if isApiKey && apiKey == "" {
		apiKey = creds.AccessToken
	}
	if isApiKey && apiKey != "" {
		base.SetHeaderExact(h, "Authorization", "Bearer "+apiKey)
		base.SetHeaderExact(h, "tokentype", "API_KEY")
	} else if creds.AccessToken != "" {
		base.SetHeaderExact(h, "Authorization", "Bearer "+creds.AccessToken)
		if isExternalIdp {
			base.SetHeaderExact(h, "TokenType", "EXTERNAL_IDP")
		}
	}
	return h
}

// TransformRequest passes the body through.
func (e *Executor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	return body, nil
}
