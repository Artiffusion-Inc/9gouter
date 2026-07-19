// Package embedding ports the per-provider embeddings adapters from
// open-sse/handlers/embeddingProviders/*.js. Each adapter knows how to build
// the upstream URL, headers, and request body for a /v1/embeddings call, and
// how to normalize the upstream response into the OpenAI embeddings shape:
//
//	{ "object":"list", "data":[{"object":"embedding","index":0,"embedding":[...]}],
//	  "model":"...", "usage":{"prompt_tokens":N,"total_tokens":N} }
//
// The package mirrors the JS getEmbeddingAdapter(provider) registry: an
// OpenAI-compatible adapter covers most providers, a Gemini adapter handles
// embedContent/batchEmbedContents, and a node adapter derives the URL from the
// connection's providerSpecificData.baseUrl for openai-compatible-* /
// custom-embedding-* nodes.
//
// Auth refresh-on-401 and account fallback are NOT in this package — they are
// separate slices (T027 follow-ups for the other refreshers; account fallback
// is a later shared service). This package only builds and normalizes.
package embedding

import (
	"net/http"

	domainProv "github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// Params carries the parsed /v1/embeddings request fields an adapter needs.
// Mirrors the JS buildBody(model, { input, encoding_format, dimensions }).
type Params struct {
	Input          any // string or []string
	EncodingFormat string
	Dimensions     any // int, float64, or string — validated per-adapter
}

// Adapter is the per-provider embeddings port, mirroring the JS adapter shape
// { buildUrl, buildHeaders, buildBody, normalize }.
type Adapter interface {
	// BuildURL returns the full upstream embeddings endpoint URL.
	BuildURL(model string, creds domainProv.Credentials, p Params) string
	// BuildHeaders returns the headers for the upstream POST.
	BuildHeaders(creds domainProv.Credentials, p Params) http.Header
	// BuildBody returns the JSON-marshaled upstream request body.
	BuildBody(model string, p Params) ([]byte, error)
	// Normalize converts an upstream JSON response body into the OpenAI
	// embeddings shape. Providers that already return OpenAI shape pass through.
	Normalize(body []byte, model string) ([]byte, error)
}

// specialAdapters maps provider ids that need a non-OpenAI adapter, mirroring
// the JS ADAPTERS overrides (gemini, google_ai_studio -> gemini).
var specialAdapters = map[string]Adapter{
	"gemini":           geminiAdapter{},
	"google_ai_studio": geminiAdapter{},
}

// Lookup returns the embeddings adapter for a provider id, mirroring the JS
// getEmbeddingAdapter. Returns (nil, false) for providers with no embeddings
// support so the caller can 404 cleanly rather than guessing.
func Lookup(providerID string) (Adapter, bool) {
	if a, ok := specialAdapters[providerID]; ok {
		return a, true
	}
	if isNodeProvider(providerID) {
		return nodeOpenAIAdapter{}, true
	}
	if _, ok := openAICompatEmbedURLs[providerID]; ok {
		return openAIAdapter{providerID: providerID}, true
	}
	// Derive-from-chat fallback: any provider whose chat BaseURL follows the
	// ".../v1/chat/completions" or ".../v1/messages" convention gets an
	// embeddings endpoint by replacing the suffix with "/embeddings". This
	// covers providers not listed explicitly (e.g. cerebras, groq).
	if u, ok := deriveEmbedURL(providerID); ok {
		return openAIAdapter{providerID: providerID, derivedURL: u}, true
	}
	return nil, false
}