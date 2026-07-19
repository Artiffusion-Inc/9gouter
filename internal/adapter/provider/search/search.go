// Package search ports the static web-search provider registry from the
// legacy JS build:
//   - open-sse/providers/registry/*.js (per-provider searchConfig / searchViaChat
//     static fields), and
//   - open-sse/handlers/search/callers.js + normalizers.js (the dedicated search
//     upstream request/response shapes).
//
// The searchproxy usecase dispatches by Mode: ModeDedicated calls the provider's
// searchConfig (a direct search API), ModeChat calls searchViaChat (an LLM
// chat call with a search tool, from which citations are extracted). Lookup
// returns the config so the handler can 501 providers whose transport is not
// implemented in the Go MVP.
//
// This is a static-registry slice: the request building and response
// normalization live in the usecase (mirroring how the JS callers.js /
// normalizers.js were per-provider functions, here collapsed into the usecase
// switch keyed by provider id). Auth mirrors the JS buildAuthHeaders schemes.
package search

// Mode is the web-search upstream protocol shape.
type Mode string

const (
	// ModeDedicated is a direct search API (serper, brave, exa, tavily,
	// google-pse, linkup, searchapi, youcom, searxng, perplexity-dedicated).
	// Response has answer: null and a results[] array.
	ModeDedicated Mode = "dedicated"
	// ModeChat is a chat-based search: an LLM generateContent / chat/completions
	// call with a search tool, from which the answer text + citations are
	// extracted (gemini, openai, xai, kimi, minimax, perplexity-chat,
	// perplexity-agent). Response has answer: {source, text, model} and a
	// results[] array built from citations/grounding chunks.
	ModeChat Mode = "chat"
)

// AuthHeader selects the auth scheme for the upstream request.
type AuthHeader string

const (
	AuthBearer           AuthHeader = "bearer"             // Authorization: Bearer <tok>
	AuthXAPIKey          AuthHeader = "x-api-key"           // x-api-key: <tok> (exa)
	AuthXSubscriptionTok AuthHeader = "x-subscription-token" // brave
	AuthXAPIKeySerper    AuthHeader = "x-api-key-serper"    // serper X-API-Key
	AuthKeyQuery         AuthHeader = "key-query"          // ?key=<tok> (google-pse)
	AuthAPIKeyQuery      AuthHeader = "api_key-query"      // ?api_key=<tok> (searchapi)
	AuthXGoogAPIKey      AuthHeader = "x-goog-api-key"     // gemini
	AuthNone             AuthHeader = "none"               // searxng
)

// Config is the static per-provider web-search configuration. Mode selects the
// usecase dispatch path; AuthHeader selects the credential scheme.
type Config struct {
	Mode       Mode
	AuthHeader AuthHeader
	// BaseURL is the dedicated-search endpoint (ModeDedicated) or the chat
	// endpoint (ModeChat). For ModeDedicated providers whose path depends on
	// search_type (serper /search vs /news), BaseURL is the base and the usecase
	// appends the path.
	BaseURL string
	// DefaultModel is the searchViaChat default model (ModeChat only).
	DefaultModel string
	// DefaultResults / MaxResults bound max_results (per JS searchConfig).
	DefaultResults int
	MaxResults     int
	// SearchTypes lists the supported search_type values (per JS searchConfig).
	SearchTypes []string
	// Unsupported marks providers whose transport is not implemented in the Go
	// MVP. The usecase returns 501 instead of attempting the call.
	Unsupported bool
}

// Lookup returns the web-search config for a provider id, or (zero, false) if
// the provider has no search support (matching the JS adapter absence → 400).
func Lookup(providerID string) (Config, bool) {
	c, ok := configs[providerID]
	return c, ok
}

// LookupAlias resolves a provider alias (e.g. "pplx" → "perplexity", "gpse" →
// "google-pse") to its canonical id, mirroring the JS resolveProviderId.
func LookupAlias(alias string) (string, bool) {
	id, ok := aliases[alias]
	return id, ok
}

var aliases = map[string]string{
	"serper":  "serper",
	"brave":   "brave-search",
	"pplx":    "perplexity",
	"exa":     "exa",
	"tavily":  "tavily",
	"gpse":    "google-pse",
	"linkup":  "linkup",
	"searchapi": "searchapi",
	"youcom":  "youcom",
	"searxng": "searxng",
	"gemini":  "gemini",
	"openai":  "openai",
	"xai":     "xai",
	"kimi":    "kimi",
	"minimax": "minimax",
	"perplexity-agent": "perplexity-agent",
}

var configs = map[string]Config{
	// === Dedicated search APIs ===
	"serper": {
		Mode:           ModeDedicated,
		AuthHeader:     AuthXAPIKeySerper,
		BaseURL:        "https://google.serper.dev",
		DefaultResults: 5,
		MaxResults:     100,
		SearchTypes:    []string{"web", "news"},
	},
	"brave-search": {
		Mode:           ModeDedicated,
		AuthHeader:     AuthXSubscriptionTok,
		BaseURL:        "https://api.search.brave.com/res/v1",
		DefaultResults: 5,
		MaxResults:     20,
		SearchTypes:    []string{"web", "news"},
	},
	"perplexity": {
		// Dedicated Perplexity search API (POST https://api.perplexity.ai with
		// {query, max_results, ...}). The same provider id also has a chat-based
		// searchViaChat (chat/completions with sonar); the usecase prefers the
		// dedicated mode and falls back to chat on a retriable error.
		Mode:           ModeDedicated,
		AuthHeader:     AuthBearer,
		BaseURL:        "https://api.perplexity.ai",
		DefaultResults: 5,
		MaxResults:     100,
		SearchTypes:    []string{"web", "news"},
	},
	"exa": {
		Mode:           ModeDedicated,
		AuthHeader:     AuthXAPIKey,
		BaseURL:        "https://api.exa.ai/search",
		DefaultResults: 5,
		MaxResults:     100,
		SearchTypes:    []string{"web", "news"},
	},
	"tavily": {
		Mode:           ModeDedicated,
		AuthHeader:     AuthBearer,
		BaseURL:        "https://api.tavily.com/search",
		DefaultResults: 5,
		MaxResults:     20,
		SearchTypes:    []string{"web", "news"},
	},
	"google-pse": {
		Mode:           ModeDedicated,
		AuthHeader:     AuthKeyQuery,
		BaseURL:        "https://www.googleapis.com/customsearch/v1",
		DefaultResults: 5,
		MaxResults:     10,
		SearchTypes:    []string{"web", "news"},
	},
	"linkup": {
		Mode:           ModeDedicated,
		AuthHeader:     AuthBearer,
		BaseURL:        "https://api.linkup.so/v1/search",
		DefaultResults: 5,
		MaxResults:     50,
		SearchTypes:    []string{"web"},
	},
	"searchapi": {
		Mode:           ModeDedicated,
		AuthHeader:     AuthAPIKeyQuery,
		BaseURL:        "https://www.searchapi.io/api/v1/search",
		DefaultResults: 5,
		MaxResults:     100,
		SearchTypes:    []string{"web", "news"},
	},
	"youcom": {
		Mode:           ModeDedicated,
		AuthHeader:     AuthXAPIKey,
		BaseURL:        "https://ydc-index.io/v1/search",
		DefaultResults: 5,
		MaxResults:     100,
		SearchTypes:    []string{"web", "news"},
	},
	"searxng": {
		// SEARXNG_URL env override is applied by the usecase at call time; the
		// static default mirrors the JS runtimeConfig default.
		Mode:           ModeDedicated,
		AuthHeader:     AuthNone,
		BaseURL:        "http://localhost:8888/search",
		DefaultResults: 5,
		MaxResults:     50,
		SearchTypes:    []string{"web", "news"},
	},

	// === Chat-based search (searchViaChat) ===
	"gemini": {
		Mode:          ModeChat,
		AuthHeader:    AuthXGoogAPIKey,
		BaseURL:       "https://generativelanguage.googleapis.com/v1beta/models",
		DefaultModel:  "gemini-2.5-flash",
		SearchTypes:   []string{"web"},
	},
	"openai": {
		Mode:         ModeChat,
		AuthHeader:   AuthBearer,
		BaseURL:      "https://api.openai.com/v1/chat/completions",
		DefaultModel: "gpt-4o-mini",
		SearchTypes:  []string{"web"},
	},
	"xai": {
		Mode:         ModeChat,
		AuthHeader:   AuthBearer,
		BaseURL:      "https://api.x.ai/v1/responses",
		DefaultModel: "grok-4.20-reasoning",
		SearchTypes:  []string{"web"},
	},
	"kimi": {
		Mode:         ModeChat,
		AuthHeader:   AuthBearer,
		BaseURL:      "https://api.moonshot.cn/v1/chat/completions",
		DefaultModel: "kimi-k2.5",
		SearchTypes:  []string{"web"},
	},
	"minimax": {
		Mode:         ModeChat,
		AuthHeader:   AuthBearer,
		BaseURL:      "https://api.minimaxi.com/v1/text/chatcompletion_v2",
		DefaultModel: "MiniMax-M2.7",
		SearchTypes:  []string{"web"},
	},
	"perplexity-agent": {
		Mode:         ModeChat,
		AuthHeader:   AuthBearer,
		BaseURL:      "https://api.perplexity.ai/v1/responses",
		DefaultModel: "perplexity/sonar",
		SearchTypes:  []string{"web"},
	},
}

// KnownProviders is the static set of provider ids with a search config, used
// for the bare-model fallback scan and tests. It mirrors the configs map.
var KnownProviders = []string{
	"serper", "brave-search", "perplexity", "exa", "tavily", "google-pse",
	"linkup", "searchapi", "youcom", "searxng",
	"gemini", "openai", "xai", "kimi", "minimax", "perplexity-agent",
}

// ChatFallbackFor returns the chat-based search config to fall back to when a
// dedicated search call fails with a retriable error. Only perplexity has both
// a dedicated and a chat mode in this registry; the JS build also allows any
// provider with searchViaChat. Returns (zero, false) when no chat fallback.
func ChatFallbackFor(providerID string) (Config, bool) {
	if providerID == "perplexity" {
		// Perplexity chat uses chat/completions with sonar and top-level
		// citations. Reuse the agent config shape but point at chat/completions.
		return Config{
			Mode:         ModeChat,
			AuthHeader:   AuthBearer,
			BaseURL:      "https://api.perplexity.ai/chat/completions",
			DefaultModel: "sonar",
			SearchTypes:  []string{"web"},
		}, true
	}
	return Config{}, false
}