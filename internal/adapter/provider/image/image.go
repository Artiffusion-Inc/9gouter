// Package image ports the static image-generation provider registry from
// open-sse/handlers/imageProviders/index.js + the per-provider adapters'
// static transport config (baseUrl, authType, authHeader, format, bodyFields
// whitelist). The imageproxy usecase dispatches by Format; Lookup returns
// the config. Providers whose transport is not yet implemented in the Go
// build (async polling, multipart, executor-delegated) keep the Unsupported
// flag so the usecase can return 501 honestly; sync providers are implemented
// and carry no flag.
//
// authHeader mirrors the JS buildAuthHeaders schemes:
// "bearer" → `Bearer <tok>`, "key" → `?key=<tok>` query param (Gemini),
// "x-key" → `x-key: <tok>` (Black Forest Labs), "fal-key" → `Authorization: Key
// <tok>` (fal-ai), "bearer-account" → Bearer + chatgpt-account-id (Codex),
// "none" → no auth (sdwebui, comfyui).
package image

// Format is the image-generation upstream protocol shape.
type Format string

const (
	// FormatOpenAI is the OpenAI /v1/images/generations JSON shape
	// {model,prompt,n,size,quality,style,response_format} → {created,data:[…]}.
	// Used by openai, minimax, openrouter, recraft, xai (bodyFields whitelist),
	// vercel-ai-gateway, venice.
	FormatOpenAI Format = "openai"
	// FormatGemini is generateContent with responseModalities ["TEXT","IMAGE"]
	// → candidates[].content.parts[].inlineData.data (base64).
	FormatGemini Format = "gemini"
	// FormatCodex is the OpenAI Responses API with tools:[{type:"image_generation",
	// …}], streaming SSE, Bearer + chatgpt-account-id. Result base64 in
	// response.output_item.done.item.result.
	FormatCodex Format = "codex"
	// Deferred formats (not implemented in the Go MVP — 501):
	FormatSDWebUI      Format = "sdwebui"           // noAuth local /sdapi/v1/txt2img
	FormatComfyUI      Format = "comfyui"           // noAuth local
	FormatHuggingFace  Format = "huggingface"       // {inputs:prompt} → raw binary
	FormatFalAI        Format = "fal-ai"            // async polling
	FormatBlackForest  Format = "black-forest-labs" // async polling, x-key
	FormatStability    Format = "stability-ai"      // {image} → b64_json
	FormatRunwayML     Format = "runwayml"          // async polling /tasks/{id}
	FormatCloudflareAI Format = "cloudflare-ai"     // JSON or multipart
	FormatNanobanana   Format = "nanobanana"        // async polling
	FormatAntigravity  Format = "antigravity"       // executor/Gemini format
)

// AuthHeader selects the auth scheme.
type AuthHeader string

const (
	AuthBearer        AuthHeader = "bearer"
	AuthKey           AuthHeader = "key" // query param ?key=<tok> (Gemini)
	AuthXKey          AuthHeader = "x-key"
	AuthFalKey        AuthHeader = "fal-key"        // Authorization: Key <tok>
	AuthBearerAccount AuthHeader = "bearer-account" // Bearer + chatgpt-account-id
	AuthNone          AuthHeader = "none"
)

// AuthType is the credential requirement.
type AuthType string

const (
	AuthTypeAPIKey AuthType = "apikey"
	AuthTypeNone   AuthType = "none"
)

// Config is the static per-provider image-generation configuration, mirroring
// the JS adapter static fields. BodyFields is the OpenAI-shape whitelist
// (nil/empty = pass all OpenAI fields); only xai sets one in the JS build.
type Config struct {
	BaseURL    string
	AuthType   AuthType
	AuthHeader AuthHeader
	Format     Format
	// BodyFields whitelists which OpenAI request fields are forwarded upstream.
	// Empty/nil → forward all known OpenAI image fields. xai uses
	// ["model","prompt","n","response_format"].
	BodyFields []string
	// Unsupported marks providers whose transport is not implemented in the Go
	// build (async polling, multipart, executor-delegated). The usecase
	// returns 501 instead of attempting the call. Sync providers (sdwebui,
	// comfyui, huggingface, stability-ai) are implemented in step 5 and do NOT
	// set this flag; async/deferred providers (fal-ai, black-forest-labs,
	// runwayml, cloudflare-ai, nanobanana, antigravity) keep it until steps 6–8
	// lift their transport.
	Unsupported bool
	// PollURL is the base URL for a provider's separate poll endpoint when the
	// provider's submit and poll URLs live on different paths/hosts. Only
	// nanobanana uses this (submit = BaseURL ".../nanobanana/generate",
	// poll = PollURL ".../nanobanana/record-info"); the adapter appends
	// "?taskId=<id>". Empty for providers whose poll URL is returned in the
	// submit response (fal-ai, BFL, runwayml) — those poll URLs come from
	// upstream and are validated against the host allowlist at runtime.
	PollURL string
}

// Lookup returns the image-generation config for a provider id, or
// (zero, false) if the provider has no image support (matching the JS adapter
// absence → 400).
func Lookup(providerID string) (Config, bool) {
	c, ok := configs[providerID]
	return c, ok
}

// SetConfig replaces the image-generation config for a provider id. It is
// intended for tests that need to point a provider at a test endpoint; the
// production registry is static and never calls it at runtime.
func SetConfig(providerID string, cfg Config) { configs[providerID] = cfg }

var configs = map[string]Config{
	"openai": {
		BaseURL:    "https://api.openai.com/v1/images/generations",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBearer,
		Format:     FormatOpenAI,
	},
	"minimax": {
		BaseURL:    "https://api.minimaxi.com/v1/images/generations",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBearer,
		Format:     FormatOpenAI,
	},
	"openrouter": {
		BaseURL:    "https://openrouter.ai/api/v1/images/generations",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBearer,
		Format:     FormatOpenAI,
	},
	"recraft": {
		BaseURL:    "https://external.api.recraft.ai/v1/images/generations",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBearer,
		Format:     FormatOpenAI,
	},
	"xai": {
		BaseURL:    "https://api.x.ai/v1/images/generations",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBearer,
		Format:     FormatOpenAI,
		BodyFields: []string{"model", "prompt", "n", "response_format"},
	},
	"vercel-ai-gateway": {
		BaseURL:    "https://ai-gateway.vercel.sh/v1/images/generations",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBearer,
		Format:     FormatOpenAI,
	},
	"venice": {
		BaseURL:    "https://api.venice.ai/api/v1/images/generations",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBearer,
		Format:     FormatOpenAI,
	},
	"gemini": {
		BaseURL:    "https://generativelanguage.googleapis.com/v1beta/models",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthKey,
		Format:     FormatGemini,
	},
	"codex": {
		// BaseURL is the Codex Responses API base; the usecase appends
		// /responses. chatgpt-account-id is taken from the credentials'
		// providerSpecificData["chatgptAccountID"] (or idToken claim).
		BaseURL:    "https://chatgpt.com/backend-api/codex",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBearerAccount,
		Format:     FormatCodex,
	},
	// Sync image providers — transport implemented in step 5 of the
	// image-provider-parity plan (sdwebui, comfyui, huggingface, stability-ai).
	// They are no longer marked Unsupported; the imageproxy usecase dispatches
	// them by Format.
	"sdwebui":      {BaseURL: "http://127.0.0.1:7860/sdapi/v1/txt2img", AuthType: AuthTypeNone, AuthHeader: AuthNone, Format: FormatSDWebUI},
	"comfyui":      {BaseURL: "http://127.0.0.1:8188", AuthType: AuthTypeNone, AuthHeader: AuthNone, Format: FormatComfyUI},
	"huggingface":  {BaseURL: "https://api-inference.huggingface.co/models", AuthType: AuthTypeAPIKey, AuthHeader: AuthBearer, Format: FormatHuggingFace},
	"stability-ai": {BaseURL: "https://api.stability.ai/v2beta/stable-image/generate", AuthType: AuthTypeAPIKey, AuthHeader: AuthBearer, Format: FormatStability},
	// Async image providers — transport implemented in step 7 of the
	// image-provider-parity plan (fal-ai, black-forest-labs, runwayml,
	// nanobanana). They are no longer marked Unsupported; the imageproxy
	// usecase dispatches them by Format with submit → poll → result lifecycle.
	// nanobanana uses a separate PollURL (record-info) distinct from its
	// submit BaseURL (.../nanobanana/generate). The other async providers
	// receive the poll URL in the submit response and validate it against the
	// host allowlist at runtime.
	"fal-ai":            {BaseURL: "https://queue.fal.run", AuthType: AuthTypeAPIKey, AuthHeader: AuthFalKey, Format: FormatFalAI},
	"black-forest-labs": {BaseURL: "https://api.bfl.ai/v1", AuthType: AuthTypeAPIKey, AuthHeader: AuthXKey, Format: FormatBlackForest},
	"runwayml":          {BaseURL: "https://api.dev.runwayml.com/v1", AuthType: AuthTypeAPIKey, AuthHeader: AuthBearer, Format: FormatRunwayML},
	// cloudflare-ai transport implemented in step 6.
	"cloudflare-ai": {BaseURL: "https://api.cloudflare.com/client/v4/accounts", AuthType: AuthTypeAPIKey, AuthHeader: AuthBearer, Format: FormatCloudflareAI},
	"nanobanana": {
		BaseURL:    "https://api.nanobananaapi.ai/api/v1/nanobanana/generate",
		PollURL:    "https://api.nanobananaapi.ai/api/v1/nanobanana/record-info",
		AuthType:   AuthTypeAPIKey,
		AuthHeader: AuthBearer,
		Format:     FormatNanobanana,
	},
	// antigravity transport implemented in step 8 (executor-delegated via the
	// image-capable AntigravityImageExecutor boundary; OAuth bearer + project
	// ID + proxy route preserved by the wire adapter). No longer Unsupported.
	"antigravity": {AuthType: AuthTypeAPIKey, AuthHeader: AuthBearer, Format: FormatAntigravity},
}

// KnownProviders is the static set of provider ids with an image config, used
// for the bare-model fallback scan and tests. It mirrors the configs map.
var KnownProviders = []string{
	"openai", "minimax", "openrouter", "recraft", "xai",
	"vercel-ai-gateway", "venice", "gemini", "codex",
	"sdwebui", "comfyui", "huggingface", "fal-ai", "black-forest-labs",
	"stability-ai", "runwayml", "cloudflare-ai", "nanobanana", "antigravity",
}
