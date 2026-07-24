// Package kiroexec ports the Kiro executor.
package kiroexec

import (
	"encoding/json"
	"net/http"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
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

// kiroDefaultProfileARNs are the public shared CodeWhisperer profile ARNs
// (us-east-1) keyed by auth method, mirroring open-sse/config/kiroConstants.js
// KIRO_DEFAULT_PROFILE_ARNS. Used when an account cannot resolve its own
// profileArn — only for OAuth/social auth (builder-id, google, github).
const (
	kiroDefaultProfileArnBuilderID = "arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX"
	kiroDefaultProfileArnSocial    = "arn:aws:codewhisperer:us-east-1:699475941385:profile/EHGA3GRVQMUK"
)

// resolveDefaultProfileArn mirrors kiroConstants.js resolveDefaultProfileArn:
// social (google/github) sign-ins map to the social shared profile; everything
// else (builder-id OAuth) maps to the builder-id shared profile.
func resolveDefaultProfileArn(authMethod string) string {
	if authMethod == "google" || authMethod == "github" {
		return kiroDefaultProfileArnSocial
	}
	return kiroDefaultProfileArnBuilderID
}

// accountBoundAuthMethods are the auth methods whose token is bound to a
// specific AWS account (api_key / idc / external_idp). The shared builder-id /
// social default profileArn belongs to a different account and triggers 403
// "bearer token invalid", so they must never fall back to it — send the
// resolved ARN, or an empty string so CodeWhisperer uses the token's own
// default profile. Mirrors abc0add0 accountBoundAuth.
func accountBoundAuthMethod(authMethod string) bool {
	return authMethod == "api_key" || authMethod == "idc" || authMethod == "external_idp"
}

// resolveKiroProfileArn picks the profileArn to send upstream for a connection,
// mirroring openai-to-kiro.js / claude-to-kiro.js (decolua/9router abc0add0):
//
//   - account-bound auth (api_key / idc / external_idp): the resolved
//     profileArn from the connection, or "" (never the shared default — it
//     belongs to another account → 403 "bearer token invalid").
//   - OAuth/social (builder-id, google, github): the resolved profileArn, or
//     the shared default placeholder for that auth method.
func resolveKiroProfileArn(creds provider.Credentials) string {
	authMethod, _ := creds.ProviderSpecificData["authMethod"].(string)
	resolved, _ := creds.ProviderSpecificData["profileArn"].(string)
	if accountBoundAuthMethod(authMethod) {
		return resolved
	}
	if resolved != "" {
		return resolved
	}
	return resolveDefaultProfileArn(authMethod)
}

// applyKiroProfileArn rewrites the profileArn field on an already-translated
// Kiro request body with the connection-resolved value. The request translator
// (openai→kiro / claude→kiro) writes a placeholder builder-id ARN because Go's
// translator seam does not receive credentials (unlike the JS
// translateRequest(model, body, stream, credentials) signature); the executor
// is the first place credentials are available, so it rewrites the field here
// before the upstream call. Mirrors the profileArn resolution in
// openai-to-kiro.js:538-543 / claude-to-kiro.js (abc0add0).
//
// On any parse/encode failure the body is returned unchanged so a malformed
// payload still reaches the upstream (and surfaces its own error) rather than
// crashing the chat path.
func applyKiroProfileArn(body json.RawMessage, creds provider.Credentials) json.RawMessage {
	if len(body) == 0 {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	payload["profileArn"] = resolveKiroProfileArn(creds)
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}
