// Package stt ports the static STT provider registry from
// open-sse/providers/registry/{openai,groq,deepgram,gemini,assemblyai}.js.
// Each provider's sttConfig (baseUrl, authType, authHeader, format) is captured
// as a Config so the sttproxy usecase can dispatch the right upstream pipeline
// (deepgram raw-binary, assemblyai upload→submit→poll, gemini generateContent,
// openai-compatible multipart passthrough).
//
// authHeader values mirror the JS buildAuthHeaders: "bearer" → `Bearer <tok>`,
// "token" → `Token <tok>`, "x-api-key" → `x-api-key: <tok>`, "key" → `Key <tok>`,
// "authorization" → `Authorization: <tok>` (AssemblyAI uses the raw key as the
// bearer-style Authorization header — its SDK sends `aai-api-key` historically,
// but the JS registry sends Authorization and AssemblyAI accepts the API key
// directly there; we match the JS behavior verbatim).
package stt

// Format is the STT upstream protocol shape.
type Format string

const (
	FormatOpenAI     Format = "openai"     // multipart passthrough (OpenAI/Groq/Whisper-compatible)
	FormatDeepgram   Format = "deepgram"   // raw binary POST + query params
	FormatAssemblyAI Format = "assemblyai" // upload → submit → poll
	FormatGemini     Format = "gemini-stt" // generateContent with inline_data
)

// AuthHeader selects the Authorization header scheme.
type AuthHeader string

const (
	AuthBearer        AuthHeader = "bearer"
	AuthToken         AuthHeader = "token"
	AuthXAPIKey       AuthHeader = "x-api-key"
	AuthKey           AuthHeader = "key"
	AuthAuthorization AuthHeader = "authorization" // raw key in Authorization header
)

// AuthType is the credential requirement. Only "apikey" is wired; "none"
// providers would skip the credential check.
type AuthType string

const (
	AuthTypeAPIKey AuthType = "apikey"
	AuthTypeNone   AuthType = "none"
)

// Config is the static per-provider STT configuration, mirroring the JS
// sttConfig block.
type Config struct {
	BaseURL    string
	AuthType   AuthType
	AuthHeader AuthHeader
	Format     Format
	// UploadURL is the AssemblyAI upload endpoint. Empty defaults to the
	// canonical https://api.assemblyai.com/v2/upload at call time.
	UploadURL string
}

// Lookup returns the STT config for a provider id, or (zero, false) if the
// provider has no STT support (matching the JS `cfg` absence → 400).
func Lookup(providerID string) (Config, bool) {
	c, ok := configs[providerID]
	return c, ok
}

var configs = map[string]Config{
	"openai": {
		BaseURL:    "https://api.openai.com/v1/audio/transcriptions",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBearer,
		Format:     FormatOpenAI,
	},
	"groq": {
		BaseURL:    "https://api.groq.com/openai/v1/audio/transcriptions",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBearer,
		Format:     FormatOpenAI,
	},
	"deepgram": {
		BaseURL:    "https://api.deepgram.com/v1/listen",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthToken,
		Format:     FormatDeepgram,
	},
	"gemini": {
		BaseURL:    "https://generativelanguage.googleapis.com/v1beta/models",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthKey,
		Format:     FormatGemini,
	},
	"assemblyai": {
		BaseURL:    "https://api.assemblyai.com/v2/transcript",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthAuthorization,
		Format:     FormatAssemblyAI,
	},
}
