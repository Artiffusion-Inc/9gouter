package embedding

import (
	"encoding/json"
	"net/http"
	"strings"

	reg "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// openAICompatEmbedURLs is the explicit per-provider embeddings base URL map
// for OpenAI-compatible providers, mirroring the JS ENDPOINTS + the
// embeddingConfig.baseUrl values from open-sse/providers/registry/*.js.
// Providers not listed here are handled by deriveEmbedURL (chat→embeddings).
var openAICompatEmbedURLs = map[string]string{
	"openai":            "https://api.openai.com/v1/embeddings",
	"openrouter":        "https://openrouter.ai/api/v1/embeddings",
	"mistral":           "https://api.mistral.ai/v1/embeddings",
	"voyage-ai":         "https://api.voyageai.com/v1/embeddings",
	"fireworks":         "https://api.fireworks.ai/inference/v1/embeddings",
	"together":         "https://api.together.ai/v1/embeddings",
	"nebius":            "https://api.nebius.ai/v1/embeddings",
	"github":           "https://api.githubcopilot.com/embeddings",
	"nvidia":           "https://integrate.api.nvidia.com/v1/embeddings",
	"jina-ai":          "https://api.jina.ai/v1/embeddings",
	"vercel-ai-gateway": "https://ai-gateway.vercel.app/v1/embeddings",
	"cohere":           "https://api.cohere.ai/v1/embeddings",
	"deepseek":         "https://api.deepseek.com/embeddings",
	"deepinfra":        "https://api.deepinfra.com/v1/openai/embeddings",
	"hyperbolic":       "https://api.hyperbolic.xyz/v1/embeddings",
	"xinference":       "https://api.xinference.ai/v1/embeddings",
}

// openAIAdapter builds OpenAI-compatible /v1/embeddings requests for a fixed
// provider id. It mirrors open-sse/handlers/embeddingProviders/openai.js.
type openAIAdapter struct {
	providerID string
	// derivedURL is set when the URL was derived from the chat BaseURL rather
	// than looked up in openAICompatEmbedURLs; empty means use the map.
	derivedURL string
}

func (a openAIAdapter) url() string {
	if a.derivedURL != "" {
		return a.derivedURL
	}
	if u, ok := openAICompatEmbedURLs[a.providerID]; ok {
		return u
	}
	return "https://api.openai.com/v1/embeddings"
}

func (a openAIAdapter) BuildURL(_ string, _ domainProv.Credentials, _ Params) string {
	return a.url()
}

func (a openAIAdapter) BuildHeaders(creds domainProv.Credentials, _ Params) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	if creds.APIKey != "" {
		h.Set("Authorization", "Bearer "+creds.APIKey)
	} else if creds.AccessToken != "" {
		h.Set("Authorization", "Bearer "+creds.AccessToken)
	}
	return h
}

func (a openAIAdapter) BuildBody(model string, p Params) ([]byte, error) {
	body := map[string]any{"model": model, "input": p.Input}
	if p.EncodingFormat != "" {
		body["encoding_format"] = p.EncodingFormat
	}
	if d := normalizeDimensions(p.Dimensions); d > 0 {
		body["dimensions"] = d
	}
	return json.Marshal(body)
}

// Normalize passes the response through — OpenAI-compatible providers already
// return the OpenAI embeddings shape.
func (a openAIAdapter) Normalize(body []byte, _ string) ([]byte, error) {
	return body, nil
}

// nodeOpenAIAdapter handles openai-compatible-* / custom-embedding-* nodes,
// deriving the embeddings URL from the connection's
// providerSpecificData.baseUrl. Mirrors openaiCompatNode.js.
type nodeOpenAIAdapter struct{}

func (nodeOpenAIAdapter) BuildURL(_ string, creds domainProv.Credentials, _ Params) string {
	raw := baseURLFromCreds(creds)
	// Strip trailing slash and any trailing /embeddings so a baseUrl that
	// already includes the suffix is not doubled.
	base := strings.TrimRight(raw, "/")
	base = strings.TrimSuffix(base, "/embeddings")
	return base + "/embeddings"
}

func (nodeOpenAIAdapter) BuildHeaders(creds domainProv.Credentials, _ Params) http.Header {
	return openAIAdapter{}.BuildHeaders(creds, Params{})
}

func (nodeOpenAIAdapter) BuildBody(model string, p Params) ([]byte, error) {
	return openAIAdapter{}.BuildBody(model, p)
}

func (nodeOpenAIAdapter) Normalize(body []byte, _ string) ([]byte, error) {
	return body, nil
}

// baseURLFromCreds reads the per-connection baseUrl from
// providerSpecificData, defaulting to the public OpenAI endpoint.
func baseURLFromCreds(creds domainProv.Credentials) string {
	if creds.ProviderSpecificData != nil {
		if v, ok := creds.ProviderSpecificData["baseUrl"].(string); ok && v != "" {
			return v
		}
	}
	return "https://api.openai.com/v1"
}

// normalizeDimensions coerces the dimensions field (int / float64 / numeric
// string) to a positive int, mirroring the JS Number.isFinite(dim) && dim > 0
// guard. Zero/negative/non-numeric means "omit dimensions" (returns 0).
func normalizeDimensions(v any) int {
	var f float64
	switch x := v.(type) {
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	case float64:
		f = x
	case string:
		if x == "" {
			return 0
		}
		if err := json.Unmarshal([]byte(x), &f); err != nil {
			return 0
		}
	default:
		return 0
	}
	if f <= 0 {
		return 0
	}
	return int(f)
}

// isNodeProvider reports whether providerID is an openai-compatible-* /
// custom-embedding-* node (per-connection baseUrl). Matches the JS
// provider.startsWith("openai-compatible-") || startsWith("custom-embedding-").
func isNodeProvider(providerID string) bool {
	return strings.HasPrefix(providerID, "openai-compatible-") ||
		strings.HasPrefix(providerID, "custom-embedding-")
}

// deriveEmbedURL derives an embeddings endpoint from a provider's chat
// BaseURL for providers not in openAICompatEmbedURLs. Returns (url, true) when
// the chat URL ends in a known chat suffix that can be swapped for
// /embeddings under a versioned API root; (..., false) otherwise. Mirrors the
// JS fallback behaviour for providers whose registry has no embeddingConfig
// but a conventional chat URL (e.g. cerebras, groq).
func deriveEmbedURL(providerID string) (string, bool) {
	chatURL := reg.ChatBaseURL(providerID)
	if chatURL == "" {
		return "", false
	}
	for _, suffix := range []string{"/chat/completions", "/messages", "/responses", "/completions"} {
		if strings.HasSuffix(chatURL, suffix) {
			base := strings.TrimSuffix(chatURL, suffix)
			if isAPIRoot(base) {
				return base + "/embeddings", true
			}
		}
	}
	return "", false
}

// isAPIRoot reports whether a path (with scheme) ends in a versioned API root.
func isAPIRoot(base string) bool {
	for _, root := range []string{"/v1", "/v1beta", "/api/v1", "/inference/v1", "/v2"} {
		if strings.HasSuffix(base, root) {
			return true
		}
	}
	return false
}