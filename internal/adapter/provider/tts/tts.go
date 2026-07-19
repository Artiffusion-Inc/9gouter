// Package tts ports the static TTS provider registry from
// open-sse/providers/registry/*.js ttsConfig blocks. Each provider's static
// transport config (baseUrl, authType, authHeader, format, defaultModel,
// defaultVoice) is captured so the ttsproxy usecase can dispatch the right
// upstream pipeline and return audio bytes in the client-requested envelope
// (raw binary or {"audio":base64,"format"} JSON).
//
// authHeader values mirror the JS buildAuthHeaders + per-adapter schemes:
// "bearer" → `Bearer <tok>`, "key" → `?key=<tok>` query param (Gemini),
// "xi-api-key" → `xi-api-key: <tok>` (ElevenLabs), "basic" → `Basic <tok>`
// (Inworld), "x-api-key" → `X-API-Key: <tok>` (Cartesia), "playht" → split
// `userId:apiKey` into `X-USER-ID` + `Authorization: Bearer <key>` (PlayHT),
// "token" → `Token <tok>` (Deepgram TTS).
//
// noAuth providers (edge-tts, google-tts, local-device, coqui, tortoise) and
// the AWS-SigV4 (aws-polly) and OpenRouter SSE-accumulate flows are NOT
// implemented in the Go build (they require web-scrape / OS exec / sigv4 /
// SSE audio-chunk accumulation that the MVP scope defers). Lookup returns
// their config so the handler can 501 them cleanly rather than 400.
package tts

// Format is the TTS upstream protocol shape.
type Format string

const (
	FormatOpenAI      Format = "openai"          // {model,voice,input} → raw binary (OpenAI/compatible)
	FormatGemini      Format = "gemini-tts"      // generateContent → base64 PCM → WAV
	FormatElevenLabs  Format = "elevenlabs"      // text-to-speech/{voiceId} → raw binary
	FormatMiniMax     Format = "minimax-tts"     // t2a_v2 hex → base64
	FormatInworld     Format = "inworld"         // JSON {audioContent} base64
	FormatCartesia    Format = "cartesia"        // raw binary
	FormatPlayHT      Format = "playht"          // raw binary
	FormatNvidia      Format = "nvidia-tts"      // raw binary
	FormatHyperbolic  Format = "hyperbolic"      // JSON {audio:base64}
	FormatDeepgram    Format = "deepgram"        // {text} → raw binary
	FormatHuggingFace Format = "huggingface-tts" // {inputs:text} → raw binary
	FormatCoqui       Format = "coqui"           // noAuth local → raw binary (deferred)
	FormatTortoise    Format = "tortoise"        // noAuth local → raw binary (deferred)
	FormatEdgeTTS     Format = "edge-tts"        // noAuth web-scrape (deferred)
	FormatGoogleTTS   Format = "google-tts"      // noAuth web-scrape (deferred)
	FormatLocalDevice Format = "local-device"    // OS exec (deferred)
	FormatAWSPolly    Format = "aws-polly"       // sigv4 (deferred)
	FormatOpenRouter  Format = "openrouter"      // SSE audio-chunk accumulate (deferred)
)

// AuthHeader selects the auth scheme.
type AuthHeader string

const (
	AuthBearer   AuthHeader = "bearer"
	AuthKey      AuthHeader = "key" // query param ?key=<tok>
	AuthXAPIKey  AuthHeader = "x-api-key"
	AuthXiAPIKey AuthHeader = "xi-api-key"
	AuthBasic    AuthHeader = "basic"
	AuthPlayHT   AuthHeader = "playht"
	AuthToken    AuthHeader = "token"
	AuthNone     AuthHeader = "none"
)

// AuthType is the credential requirement.
type AuthType string

const (
	AuthTypeAPIKey AuthType = "apikey"
	AuthTypeNone   AuthType = "none"
)

// Config is the static per-provider TTS configuration, mirroring the JS
// ttsConfig block. DefaultModel/DefaultVoice are used by parseModelVoice when
// the client sends a bare model or model without a voice.
type Config struct {
	BaseURL      string
	AuthType     AuthType
	AuthHeader   AuthHeader
	Format       Format
	DefaultModel string
	DefaultVoice string
	// Unsupported marks providers whose transport is not implemented in the Go
	// build (web-scrape / OS exec / sigv4 / SSE-accumulate). The usecase
	// returns 501 instead of attempting the call.
	Unsupported bool
}

// Lookup returns the TTS config for a provider id, or (zero, false) if the
// provider has no TTS support (matching the JS `cfg` absence → 400).
func Lookup(providerID string) (Config, bool) {
	c, ok := configs[providerID]
	return c, ok
}

var configs = map[string]Config{
	"openai": {
		BaseURL:      "https://api.openai.com/v1/audio/speech",
		AuthType:     AuthTypeAPIKey,
		AuthHeader:   AuthBearer,
		Format:       FormatOpenAI,
		DefaultModel: "gpt-4o-mini-tts",
	},
	"gemini": {
		BaseURL:    "https://generativelanguage.googleapis.com/v1beta/models",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthKey,
		Format:     FormatGemini,
	},
	"elevenlabs": {
		BaseURL:    "https://api.elevenlabs.io/v1/text-to-speech",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthXiAPIKey,
		Format:     FormatElevenLabs,
	},
	"minimax": {
		BaseURL:    "https://api.minimax.io/v1/t2a_v2",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBearer,
		Format:     FormatMiniMax,
	},
	"minimax-cn": {
		BaseURL:    "https://api.minimaxi.com/v1/t2a_v2",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBearer,
		Format:     FormatMiniMax,
	},
	"inworld": {
		BaseURL:    "https://api.inworld.ai/tts/v1/voice",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBasic,
		Format:     FormatInworld,
	},
	"cartesia": {
		BaseURL:    "https://api.cartesia.ai/tts/bytes",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthXAPIKey,
		Format:     FormatCartesia,
	},
	"playht": {
		BaseURL:    "https://api.play.ht/api/v2/tts/stream",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthPlayHT,
		Format:     FormatPlayHT,
	},
	"nvidia": {
		BaseURL:    "https://integrate.api.nvidia.com/v1/audio/speech",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBearer,
		Format:     FormatNvidia,
	},
	"deepgram": {
		BaseURL:    "https://api.deepgram.com/v1/speak",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthToken,
		Format:     FormatDeepgram,
	},
	// No-auth / OS-exec / web-scrape / sigv4 / SSE-accumulate providers are
	// registered so the handler can 501 them honestly.
	"edge-tts":     {Format: FormatEdgeTTS, AuthType: AuthTypeNone, AuthHeader: AuthNone, Unsupported: true},
	"google-tts":   {Format: FormatGoogleTTS, AuthType: AuthTypeNone, AuthHeader: AuthNone, Unsupported: true},
	"local-device": {Format: FormatLocalDevice, AuthType: AuthTypeNone, AuthHeader: AuthNone, Unsupported: true},
	"coqui":        {BaseURL: "http://localhost:5002/api/tts", Format: FormatCoqui, AuthType: AuthTypeNone, AuthHeader: AuthNone, Unsupported: true},
	"tortoise":     {BaseURL: "http://localhost:5000/api/tts", Format: FormatTortoise, AuthType: AuthTypeNone, AuthHeader: AuthNone, Unsupported: true},
	"aws-polly":    {Format: FormatAWSPolly, AuthType: AuthTypeAPIKey, AuthHeader: "aws-sigv4", Unsupported: true},
	"openrouter":   {BaseURL: "https://openrouter.ai/api/v1/chat/completions", Format: FormatOpenRouter, AuthType: AuthTypeAPIKey, AuthHeader: AuthBearer, DefaultModel: "openai/gpt-4o-mini-tts", Unsupported: true},
}
