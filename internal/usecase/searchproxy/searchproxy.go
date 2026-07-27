// Package searchproxy implements the /v1/search pipeline for the Go rewrite.
// It ports the legacy JS web-search handlers:
//   - src/sse/handlers/search.js (parse body, resolve provider, api-key gate),
//   - open-sse/handlers/search/index.js (handleSearchCore: sanitize query,
//     routing dedicated → chat fallback, global timeout),
//   - open-sse/handlers/search/callers.js (per-provider dedicated upstream
//     request building),
//   - open-sse/handlers/search/normalizers.js (per-provider response reshape
//     into the unified SearchResult),
//   - open-sse/handlers/search/chatSearch.js (chat-based search: build the LLM
//     call with a search tool, extract answer text + citations).
//
// Unified response shape (successResult), mirroring JS:
//
//	{
//	  "provider": "<id>",
//	  "query": "...",
//	  "results": [ {title,url,snippet,position,...} ],
//	  "answer": null | { "source": "<id>", "text": "...", "model": "..." },
//	  "usage": { "queries_used": 1, "search_cost_usd": 0, "llm_tokens": 0 },
//	  "metrics": { "response_time_ms": N, "upstream_latency_ms": N,
//	               "total_results_available": N | null },
//	  "errors": []
//	}
//
// Supported in this MVP slice:
//   - Dedicated: serper, tavily, searxng, exa, brave-search, perplexity,
//     google-pse, linkup, searchapi, youcom (full request build + normalize).
//   - Chat: gemini (generateContent + google_search tool, grounding chunks),
//     openai (chat/completions + web_search tool, annotations citations),
//     perplexity-chat fallback (chat/completions + top-level citations),
//     xai (/v1/responses + web_search, output[]/annotations citations),
//     kimi (chat/completions + $web_search builtin, tool_calls citations),
//     minimax (chatcompletion_v2 + web_search, web_search_results citations),
//     perplexity-agent (/v1/responses + web_search, output[]/results citations).
//
// Every provider in the search registry now has a working transport; there is
// no remaining 501 surface. Combo expansion, account-fallback rotation,
// on-401 token refresh, and usage persistence are separate slices.
package searchproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/search"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Logger is a minimal log sink.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

type noopLogger struct{}

func (noopLogger) Infof(string, ...any)  {}
func (noopLogger) Warnf(string, ...any)  {}
func (noopLogger) Debugf(string, ...any) {}

// Dependencies wires the searchproxy Handler.
type Dependencies struct {
	HTTPClient *http.Client
	Logger     Logger
	Config     config.Config
}

// Handler runs the web-search pipeline.
type Handler struct {
	deps Dependencies
}

// New constructs a Handler with sane defaults (15s timeout — mirrors the JS
// GLOBAL_TIMEOUT_MS).
func New(deps Dependencies) *Handler {
	if deps.HTTPClient == nil {
		deps.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if deps.Logger == nil {
		deps.Logger = noopLogger{}
	}
	return &Handler{deps: deps}
}

// Request is the input to Handle.
type Request struct {
	Ctx         context.Context
	ProviderID  string
	Query       string
	Model       string // optional override (chat-based search)
	MaxResults  int
	SearchType  string // "web" (default) | "news"
	Country     string
	Language    string
	TimeRange   string
	Offset      int
	Credentials domainProv.Credentials
	UserAgent   string
}

// Result is the output of Handle.
type Result struct {
	StatusCode  int
	Err         error
	Body        []byte
	ContentType string
}

// SearchResult is the unified result item, mirroring JS makeResult. Only the
// fields populated by the MVP providers are non-zero; the rest are omitted via
// omitempty so the JSON stays compact.
type SearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Snippet     string `json:"snippet,omitempty"`
	Position    int    `json:"position,omitempty"`
	Score       any    `json:"score,omitempty"`
	PublishedAt any    `json:"published_at,omitempty"`
}

// searchResponse is the unified response body.
type searchResponse struct {
	Provider string         `json:"provider"`
	Query    string         `json:"query"`
	Results  []SearchResult `json:"results"`
	Answer   *answerPayload `json:"answer"`
	Usage    usagePayload   `json:"usage"`
	Metrics  metricsPayload `json:"metrics"`
	Errors   []string       `json:"errors"`
}

type answerPayload struct {
	Source string `json:"source"`
	Text   string `json:"text"`
	Model  string `json:"model,omitempty"`
}

type usagePayload struct {
	QueriesUsed   int `json:"queries_used"`
	SearchCostUSD any `json:"search_cost_usd"`
	LLMTokens     int `json:"llm_tokens"`
}

type metricsPayload struct {
	ResponseTimeMS        int `json:"response_time_ms"`
	UpstreamLatencyMS     int `json:"upstream_latency_ms"`
	TotalResultsAvailable any `json:"total_results_available"`
}

// Handle dispatches the web-search upstream call by the provider's static
// config. Dedicated providers build a direct search-API request; chat providers
// build an LLM call with a search tool. On a retriable dedicated failure, a
// provider with a chat fallback retries via chat.
func (h *Handler) Handle(ctx context.Context, req Request) Result {
	start := nowMillis()
	cfg, ok := search.Lookup(req.ProviderID)
	if !ok {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("provider '%s' does not support web search", req.ProviderID)}
	}
	if cfg.Unsupported {
		return Result{StatusCode: http.StatusNotImplemented, Err: fmt.Errorf("provider '%s' search transport not implemented in Go build", req.ProviderID)}
	}
	query := sanitizeQuery(req.Query)
	if query == "" {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("missing required field: query")}
	}
	if cfg.AuthHeader != search.AuthNone && credentialToken(req.Credentials) == "" {
		return Result{StatusCode: http.StatusUnauthorized, Err: fmt.Errorf("no credentials for provider: %s", req.ProviderID)}
	}

	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = cfg.DefaultResults
	}
	if cfg.MaxResults > 0 && maxResults > cfg.MaxResults {
		maxResults = cfg.MaxResults
	}
	searchType := req.SearchType
	if searchType == "" {
		if len(cfg.SearchTypes) > 0 {
			searchType = cfg.SearchTypes[0]
		} else {
			searchType = "web"
		}
	}
	// Resolve the searxng BaseURL override from env at call time.
	if req.ProviderID == "searxng" {
		if env := strings.TrimSpace(os.Getenv("SEARXNG_URL")); env != "" {
			cfg.BaseURL = strings.TrimRight(env, "/") + "/search"
		}
	}

	body, status, err := h.runSearch(ctx, cfg, req, query, maxResults, searchType)
	if err != nil {
		// Retriable dedicated failure → chat fallback (perplexity).
		if cfg.Mode == search.ModeDedicated && isRetriable(status) {
			if chatCfg, ok := search.ChatFallbackFor(req.ProviderID); ok {
				body, status, err = h.runChat(ctx, chatCfg, req, query, maxResults, searchType)
			}
		}
		if err != nil {
			return Result{StatusCode: status, Err: err}
		}
	}

	// Stamp metrics into the unified response (the run* helpers already built
	// the body; inject response_time_ms / upstream_latency_ms).
	body = stampMetrics(body, start)
	return Result{StatusCode: status, Body: body, ContentType: "application/json"}
}

// runSearch dispatches by mode.
func (h *Handler) runSearch(ctx context.Context, cfg search.Config, req Request, query string, maxResults int, searchType string) ([]byte, int, error) {
	if cfg.Mode == search.ModeChat {
		return h.runChat(ctx, cfg, req, query, maxResults, searchType)
	}
	return h.runDedicated(ctx, cfg, req, query, maxResults, searchType)
}

// runDedicated builds and sends a dedicated search-API request, then normalizes
// the upstream response into the unified shape.
func (h *Handler) runDedicated(ctx context.Context, cfg search.Config, req Request, query string, maxResults int, searchType string) ([]byte, int, error) {
	switch req.ProviderID {
	case "serper":
		return h.dedicatedSerper(ctx, cfg, req, query, maxResults, searchType)
	case "tavily":
		return h.dedicatedTavily(ctx, cfg, req, query, maxResults, searchType)
	case "searxng":
		return h.dedicatedSearxng(ctx, cfg, req, query, maxResults, searchType)
	case "exa":
		return h.dedicatedExa(ctx, cfg, req, query, maxResults, searchType)
	case "brave-search":
		return h.dedicatedBrave(ctx, cfg, req, query, maxResults, searchType)
	case "perplexity":
		return h.dedicatedPerplexity(ctx, cfg, req, query, maxResults, searchType)
	case "google-pse":
		return h.dedicatedGooglePSE(ctx, cfg, req, query, maxResults, searchType)
	case "linkup":
		return h.dedicatedLinkup(ctx, cfg, req, query, maxResults, searchType)
	case "searchapi":
		return h.dedicatedSearchApi(ctx, cfg, req, query, maxResults, searchType)
	case "youcom":
		return h.dedicatedYouCom(ctx, cfg, req, query, maxResults, searchType)
	default:
		return nil, http.StatusNotImplemented, fmt.Errorf("dedicated search for '%s' not implemented", req.ProviderID)
	}
}

// runChat builds and sends a chat-based search call (LLM + search tool), then
// extracts the answer text + citations into the unified shape.
func (h *Handler) runChat(ctx context.Context, cfg search.Config, req Request, query string, maxResults int, searchType string) ([]byte, int, error) {
	switch req.ProviderID {
	case "gemini":
		return h.chatGemini(ctx, cfg, req, query)
	case "openai":
		return h.chatOpenAI(ctx, cfg, req, query)
	case "perplexity":
		return h.chatPerplexity(ctx, cfg, req, query)
	case "xai":
		return h.chatXai(ctx, cfg, req, query)
	case "kimi":
		return h.chatKimi(ctx, cfg, req, query)
	case "minimax":
		return h.chatMinimax(ctx, cfg, req, query)
	case "perplexity-agent":
		return h.chatPerplexityAgent(ctx, cfg, req, query)
	default:
		return nil, http.StatusNotImplemented, fmt.Errorf("chat-based search for '%s' not implemented", req.ProviderID)
	}
}

// === Dedicated providers ===

// dedicatedSerper POSTs {q,num,gl,hl} to /search or /news with X-API-Key.
func (h *Handler) dedicatedSerper(ctx context.Context, cfg search.Config, req Request, query string, maxResults int, searchType string) ([]byte, int, error) {
	path := "/search"
	if searchType == "news" {
		path = "/news"
	}
	payload := map[string]any{"q": query, "num": maxResults}
	if req.Country != "" {
		payload["gl"] = req.Country
	}
	if req.Language != "" {
		payload["hl"] = req.Language
	}
	raw, _ := json.Marshal(payload)
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if tok := credentialToken(req.Credentials); tok != "" {
		httpReq.Header.Set("X-API-Key", tok)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	// Normalize: news → data.news[]; web → data.organic[].
	var parsed struct {
		SearchParameters struct {
			TotalResults any `json:"totalResults"`
		} `json:"searchParameters"`
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
			Date    string `json:"date"`
		} `json:"organic"`
		News []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
			Date    string `json:"date"`
		} `json:"news"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("serper: failed to parse response: %w", err)
	}
	results := []SearchResult{}
	if searchType == "news" {
		for i, n := range parsed.News {
			results = append(results, SearchResult{Title: n.Title, URL: n.Link, Snippet: n.Snippet, Position: i + 1, PublishedAt: orString(n.Date)})
		}
	} else {
		for i, o := range parsed.Organic {
			results = append(results, SearchResult{Title: o.Title, URL: o.Link, Snippet: o.Snippet, Position: i + 1, PublishedAt: orString(o.Date)})
		}
	}
	return h.buildUnified(req.ProviderID, query, results, nil, parsed.SearchParameters.TotalResults, 0), http.StatusOK, nil
}

// dedicatedTavily POSTs {query,max_results,topic,include_domains,exclude_domains,
// country} to /search with Bearer.
func (h *Handler) dedicatedTavily(ctx context.Context, cfg search.Config, req Request, query string, maxResults int, searchType string) ([]byte, int, error) {
	payload := map[string]any{
		"query":       query,
		"max_results": maxResults,
		"topic":       "general",
	}
	if searchType == "news" {
		payload["topic"] = "news"
	}
	if req.Country != "" {
		payload["country"] = req.Country
	}
	raw, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if tok := credentialToken(req.Credentials); tok != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	var parsed struct {
		Results []struct {
			Title         string  `json:"title"`
			URL           string  `json:"url"`
			Content       string  `json:"content"`
			Score         float64 `json:"score"`
			PublishedDate string  `json:"published_date"`
		} `json:"results"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("tavily: failed to parse response: %w", err)
	}
	results := []SearchResult{}
	for i, r := range parsed.Results {
		results = append(results, SearchResult{
			Title: r.Title, URL: r.URL, Snippet: r.Content, Position: i + 1,
			Score: r.Score, PublishedAt: orString(r.PublishedDate),
		})
	}
	return h.buildUnified(req.ProviderID, query, results, nil, len(parsed.Results), 0), http.StatusOK, nil
}

// dedicatedSearxng GETs <SEARXNG_URL>/search?q=&format=json&categories=&language=
// &time_range=&pageno= with no auth.
func (h *Handler) dedicatedSearxng(ctx context.Context, cfg search.Config, req Request, query string, maxResults int, searchType string) ([]byte, int, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "json")
	if searchType == "news" {
		q.Set("categories", "news")
	} else {
		q.Set("categories", "general")
	}
	if req.Language != "" {
		q.Set("language", req.Language)
	}
	if req.TimeRange != "" {
		q.Set("time_range", req.TimeRange)
	}
	page := 1
	if req.Offset > 0 {
		page = req.Offset/maxResults + 1
	}
	q.Set("pageno", fmt.Sprintf("%d", page))
	endpoint := cfg.BaseURL + "?" + q.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	var parsed struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			PublishedDate string `json:"publishedDate"`
		} `json:"results"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("searxng: failed to parse response: %w", err)
	}
	results := []SearchResult{}
	for i, r := range parsed.Results {
		results = append(results, SearchResult{
			Title: r.Title, URL: r.URL, Snippet: r.Content, Position: i + 1,
			PublishedAt: orString(r.PublishedDate),
		})
	}
	return h.buildUnified(req.ProviderID, query, results, nil, len(parsed.Results), 0), http.StatusOK, nil
}

// dedicatedExa POSTs {query,numResults,type:"auto",text:true,highlights:true,
// category:"news"?} to https://api.exa.ai/search with x-api-key. The response
// is normalized from results[] {title,url,highlights[],text,score,publishedDate},
// mirroring open-sse/handlers/search/normalizers.js normalizeExa: the snippet is
// the first highlight, falling back to the first 300 chars of text.
func (h *Handler) dedicatedExa(ctx context.Context, cfg search.Config, req Request, query string, maxResults int, searchType string) ([]byte, int, error) {
	payload := map[string]any{
		"query":      query,
		"numResults": maxResults,
		"type":       "auto",
		"text":       true,
		"highlights": true,
	}
	if searchType == "news" {
		payload["category"] = "news"
	}
	raw, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if tok := credentialToken(req.Credentials); tok != "" {
		httpReq.Header.Set("x-api-key", tok)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	var parsed struct {
		Results []struct {
			Title         string   `json:"title"`
			URL           string   `json:"url"`
			Highlights    []string `json:"highlights"`
			Text          string   `json:"text"`
			Score         float64  `json:"score"`
			PublishedDate string   `json:"publishedDate"`
		} `json:"results"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("exa: failed to parse response: %w", err)
	}
	results := []SearchResult{}
	for i, r := range parsed.Results {
		snippet := ""
		if len(r.Highlights) > 0 {
			snippet = r.Highlights[0]
		} else if r.Text != "" {
			if len(r.Text) > 300 {
				snippet = r.Text[:300]
			} else {
				snippet = r.Text
			}
		}
		results = append(results, SearchResult{
			Title: r.Title, URL: r.URL, Snippet: snippet, Position: i + 1,
			Score: r.Score, PublishedAt: orString(r.PublishedDate),
		})
	}
	return h.buildUnified(req.ProviderID, query, results, nil, len(parsed.Results), 0), http.StatusOK, nil
}

// dedicatedBrave GETs <base>/web/search or /news/search?q=&count=&country=&search_lang=
// with X-Subscription-Token. Mirrors callers.js buildBraveRequest and
// normalizers.js normalizeBrave: container = (news ? data.news||data : data.web),
// items = container.results[] {title,url,description,page_age|age}.
func (h *Handler) dedicatedBrave(ctx context.Context, cfg search.Config, req Request, query string, maxResults int, searchType string) ([]byte, int, error) {
	endpoint := strings.TrimRight(cfg.BaseURL, "/")
	if searchType == "news" {
		endpoint += "/news/search"
	} else {
		endpoint += "/web/search"
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("count", fmt.Sprintf("%d", maxResults))
	if req.Country != "" {
		q.Set("country", req.Country)
	}
	if req.Language != "" {
		q.Set("search_lang", req.Language)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Accept", "application/json")
	if tok := credentialToken(req.Credentials); tok != "" {
		httpReq.Header.Set("X-Subscription-Token", tok)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	// Container is data.news (or data) for news, data.web for web. Each container
	// has results[] {title,url,description,page_age,age}.
	var parsed struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
				PageAge     string `json:"page_age"`
				Age         string `json:"age"`
			} `json:"results"`
			TotalCount any `json:"totalCount"`
		} `json:"web"`
		News *struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
				PageAge     string `json:"page_age"`
				Age         string `json:"age"`
			} `json:"results"`
			TotalCount any `json:"totalCount"`
		} `json:"news"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("brave-search: failed to parse response: %w", err)
	}
	results := []SearchResult{}
	var total any
	if searchType == "news" {
		// news container may be the top-level object when the API omits the news
		// wrapper; normalizeBrave uses data.news || data.
		container := parsed.News
		if container == nil {
			// Fall back: treat top-level as a news container via a second parse.
			var bare struct {
				Results []struct {
					Title       string `json:"title"`
					URL         string `json:"url"`
					Description string `json:"description"`
					PageAge     string `json:"page_age"`
					Age         string `json:"age"`
				} `json:"results"`
				TotalCount any `json:"totalCount"`
			}
			_ = json.Unmarshal(respBody, &bare)
			for i, it := range bare.Results {
				results = append(results, SearchResult{Title: it.Title, URL: it.URL, Snippet: it.Description, Position: i + 1, PublishedAt: orString(firstNonEmpty(it.PageAge, it.Age))})
			}
			total = bare.TotalCount
			return h.buildUnified(req.ProviderID, query, results, nil, total, 0), http.StatusOK, nil
		}
		for i, it := range container.Results {
			results = append(results, SearchResult{Title: it.Title, URL: it.URL, Snippet: it.Description, Position: i + 1, PublishedAt: orString(firstNonEmpty(it.PageAge, it.Age))})
		}
		total = container.TotalCount
	} else {
		for i, it := range parsed.Web.Results {
			results = append(results, SearchResult{Title: it.Title, URL: it.URL, Snippet: it.Description, Position: i + 1, PublishedAt: orString(firstNonEmpty(it.PageAge, it.Age))})
		}
		total = parsed.Web.TotalCount
	}
	return h.buildUnified(req.ProviderID, query, results, nil, total, 0), http.StatusOK, nil
}

// dedicatedPerplexity POSTs {query,max_results,country,search_language_filter,
// search_domain_filter} to https://api.perplexity.ai with Bearer. Mirrors
// callers.js buildPerplexityRequest + normalizers.js normalizePerplexity:
// results[] {title,url,snippet,date|last_updated}.
func (h *Handler) dedicatedPerplexity(ctx context.Context, cfg search.Config, req Request, query string, maxResults int, searchType string) ([]byte, int, error) {
	payload := map[string]any{
		"query":       query,
		"max_results": maxResults,
	}
	if req.Country != "" {
		payload["country"] = req.Country
	}
	if req.Language != "" {
		payload["search_language_filter"] = []string{req.Language}
	}
	raw, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if tok := credentialToken(req.Credentials); tok != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	var parsed struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Snippet     string `json:"snippet"`
			Date        string `json:"date"`
			LastUpdated string `json:"last_updated"`
		} `json:"results"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("perplexity: failed to parse response: %w", err)
	}
	results := []SearchResult{}
	for i, r := range parsed.Results {
		results = append(results, SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Snippet, Position: i + 1, PublishedAt: orString(firstNonEmpty(r.Date, r.LastUpdated))})
	}
	return h.buildUnified(req.ProviderID, query, results, nil, len(parsed.Results), 0), http.StatusOK, nil
}

// dedicatedGooglePSE GETs googleapis.com/customsearch/v1?key=&cx=&q=&num=&gl=&hl=
// &dateRestrict=&start= with no auth header (key is in query). Mirrors
// callers.js buildGooglePseRequest + normalizers.js normalizeGooglePse:
// items[] {title,link,snippet}, total from searchInformation.totalResults.
func (h *Handler) dedicatedGooglePSE(ctx context.Context, cfg search.Config, req Request, query string, maxResults int, searchType string) ([]byte, int, error) {
	tok := credentialToken(req.Credentials)
	cx := ""
	if v, ok := req.Credentials.ProviderSpecificData["cx"].(string); ok {
		cx = v
	}
	if tok == "" || cx == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("google-pse: google programmable search requires both apiKey and cx")
	}
	q := url.Values{}
	q.Set("key", tok)
	q.Set("cx", cx)
	q.Set("q", query)
	num := maxResults
	if num > 10 {
		num = 10
	}
	q.Set("num", fmt.Sprintf("%d", num))
	if req.Country != "" {
		q.Set("gl", strings.ToLower(req.Country))
	}
	if req.Language != "" {
		q.Set("hl", req.Language)
	}
	if req.TimeRange != "" && req.TimeRange != "any" {
		dateRestrictMap := map[string]string{"day": "d1", "week": "w1", "month": "m1", "year": "y1"}
		if dr, ok := dateRestrictMap[req.TimeRange]; ok {
			q.Set("dateRestrict", dr)
		}
	}
	if req.Offset > 0 {
		start := req.Offset + 1
		if start > 91 {
			start = 91
		}
		q.Set("start", fmt.Sprintf("%d", start))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Accept", "application/json")
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	var parsed struct {
		Items []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"items"`
		SearchInformation struct {
			TotalResults string `json:"totalResults"`
		} `json:"searchInformation"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("google-pse: failed to parse response: %w", err)
	}
	results := []SearchResult{}
	for i, it := range parsed.Items {
		results = append(results, SearchResult{Title: it.Title, URL: it.Link, Snippet: it.Snippet, Position: i + 1})
	}
	total := any(nil)
	if n, convErr := strconvAtoi(parsed.SearchInformation.TotalResults); convErr == nil {
		total = n
	} else if len(parsed.Items) > 0 {
		total = len(parsed.Items)
	}
	return h.buildUnified(req.ProviderID, query, results, nil, total, 0), http.StatusOK, nil
}

// dedicatedLinkup POSTs {q,depth,outputType:"searchResults",maxResults,
// includeDomains,excludeDomains,fromDate,toDate} to /search with Bearer.
// Mirrors callers.js buildLinkupRequest + normalizers.js normalizeLinkup:
// results[] {name|title,url,content|snippet}.
func (h *Handler) dedicatedLinkup(ctx context.Context, cfg search.Config, req Request, query string, maxResults int, searchType string) ([]byte, int, error) {
	depth := "standard"
	if d, ok := req.Credentials.ProviderSpecificData["depth"].(string); ok {
		switch d {
		case "fast", "standard", "deep":
			depth = d
		}
	}
	payload := map[string]any{
		"q":          query,
		"depth":      depth,
		"outputType": "searchResults",
		"maxResults": maxResults,
	}
	raw, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if tok := credentialToken(req.Credentials); tok != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	var parsed struct {
		Results []struct {
			Name    string `json:"name"`
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("linkup: failed to parse response: %w", err)
	}
	results := []SearchResult{}
	for i, r := range parsed.Results {
		title := r.Name
		if title == "" {
			title = r.Title
		}
		snippet := r.Content
		if snippet == "" {
			snippet = r.Snippet
		}
		results = append(results, SearchResult{Title: title, URL: r.URL, Snippet: snippet, Position: i + 1})
	}
	return h.buildUnified(req.ProviderID, query, results, nil, len(parsed.Results), 0), http.StatusOK, nil
}

// dedicatedSearchApi GETs <base>?engine=google|google_news&q=&api_key=&gl=&hl=&page=
// with no auth header. Mirrors callers.js buildSearchApiRequest + normalizers.js
// normalizeSearchApi: organic_results[] or top_stories[] {title,link,snippet|description,
// date|published_at}, total from search_information.total_results.
func (h *Handler) dedicatedSearchApi(ctx context.Context, cfg search.Config, req Request, query string, maxResults int, searchType string) ([]byte, int, error) {
	tok := credentialToken(req.Credentials)
	if tok == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("searchapi: searchapi requires an api key")
	}
	q := url.Values{}
	if searchType == "news" {
		q.Set("engine", "google_news")
	} else {
		q.Set("engine", "google")
	}
	q.Set("q", query)
	q.Set("api_key", tok)
	if req.Country != "" {
		q.Set("gl", strings.ToLower(req.Country))
	}
	if req.Language != "" {
		q.Set("hl", req.Language)
	}
	if req.Offset > 0 && maxResults > 0 {
		q.Set("page", fmt.Sprintf("%d", req.Offset/maxResults+1))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Accept", "application/json")
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	// organic_results (web) or top_stories (news); search_information.total_results.
	var parsed struct {
		OrganicResults []struct {
			Title       string `json:"title"`
			Link        string `json:"link"`
			Snippet     string `json:"snippet"`
			Description string `json:"description"`
			Date        string `json:"date"`
			PublishedAt string `json:"published_at"`
		} `json:"organic_results"`
		TopStories []struct {
			Title       string `json:"title"`
			Link        string `json:"link"`
			Snippet     string `json:"snippet"`
			Description string `json:"description"`
			Date        string `json:"date"`
			PublishedAt string `json:"published_at"`
		} `json:"top_stories"`
		SearchInformation struct {
			TotalResults any `json:"total_results"`
		} `json:"search_information"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("searchapi: failed to parse response: %w", err)
	}
	results := []SearchResult{}
	items := parsed.OrganicResults
	if searchType == "news" {
		items = parsed.TopStories
	}
	for i, r := range items {
		snippet := r.Snippet
		if snippet == "" {
			snippet = r.Description
		}
		results = append(results, SearchResult{Title: r.Title, URL: r.Link, Snippet: snippet, Position: i + 1, PublishedAt: orString(firstNonEmpty(r.Date, r.PublishedAt))})
	}
	total := parsed.SearchInformation.TotalResults
	if total == nil {
		total = len(results)
	}
	return h.buildUnified(req.ProviderID, query, results, nil, total, 0), http.StatusOK, nil
}

// dedicatedYouCom GETs <base>?query=&count=&freshness=&offset=&country=&language=
// &include_domains=&exclude_domains= with X-API-Key. Mirrors callers.js
// buildYouComRequest + normalizers.js normalizeYouCom: container = data.results,
// section = container.news (news) or container.web (web) {title,url,snippets[],
// description,page_age}.
func (h *Handler) dedicatedYouCom(ctx context.Context, cfg search.Config, req Request, query string, maxResults int, searchType string) ([]byte, int, error) {
	tok := credentialToken(req.Credentials)
	if tok == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("youcom: you.com search requires an api key")
	}
	count := maxResults
	if count > 100 {
		count = 100
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("count", fmt.Sprintf("%d", count))
	if req.TimeRange != "" && req.TimeRange != "any" {
		q.Set("freshness", req.TimeRange)
	}
	if req.Offset > 0 && maxResults > 0 {
		off := req.Offset / maxResults
		if off > 9 {
			off = 9
		}
		q.Set("offset", fmt.Sprintf("%d", off))
	}
	if req.Country != "" {
		q.Set("country", req.Country)
	}
	if req.Language != "" {
		q.Set("language", req.Language)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-API-Key", tok)
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	// data.results is an object {news:[], web:[]}.
	var parsed struct {
		Results struct {
			News []struct {
				Title       string   `json:"title"`
				URL         string   `json:"url"`
				Snippets    []string `json:"snippets"`
				Description string   `json:"description"`
				PageAge     string   `json:"page_age"`
			} `json:"news"`
			Web []struct {
				Title       string   `json:"title"`
				URL         string   `json:"url"`
				Snippets    []string `json:"snippets"`
				Description string   `json:"description"`
				PageAge     string   `json:"page_age"`
			} `json:"web"`
		} `json:"results"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("youcom: failed to parse response: %w", err)
	}
	results := []SearchResult{}
	items := parsed.Results.Web
	if searchType == "news" {
		items = parsed.Results.News
	}
	for i, it := range items {
		snippet := ""
		for _, s := range it.Snippets {
			if s != "" {
				snippet = s
				break
			}
		}
		if snippet == "" {
			snippet = it.Description
		}
		results = append(results, SearchResult{Title: it.Title, URL: it.URL, Snippet: snippet, Position: i + 1, PublishedAt: orString(it.PageAge)})
	}
	return h.buildUnified(req.ProviderID, query, results, nil, len(results), 0), http.StatusOK, nil
}

// === Chat-based providers ===

// chatGemini calls generateContent with tools:[{google_search:{}}], extracts
// the answer text from candidates[0].content.parts[].text and citations from
// groundingMetadata.groundingChunks[].web.
func (h *Handler) chatGemini(ctx context.Context, cfg search.Config, req Request, query string) ([]byte, int, error) {
	model := orDefault(req.Model, cfg.DefaultModel)
	model = strings.TrimPrefix(model, "models/")
	endpoint := fmt.Sprintf("%s/%s:generateContent", strings.TrimRight(cfg.BaseURL, "/"), model)
	if tok := credentialToken(req.Credentials); tok != "" {
		endpoint += "?key=" + tok
	}
	payload := map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": query}}},
		},
		"tools": []any{map[string]any{"google_search": map[string]any{}}},
	}
	raw, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			GroundingMetadata struct {
				GroundingChunks []struct {
					Web struct {
						URI   string `json:"uri"`
						URL   string `json:"url"`
						Title string `json:"title"`
					} `json:"web"`
				} `json:"groundingChunks"`
			} `json:"groundingMetadata"`
		} `json:"candidates"`
		UsageMetadata struct {
			TotalTokenCount int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("gemini: failed to parse response: %w", err)
	}
	if len(parsed.Candidates) == 0 {
		return nil, http.StatusBadGateway, fmt.Errorf("gemini: no candidates in response")
	}
	var textParts []string
	for _, p := range parsed.Candidates[0].Content.Parts {
		if p.Text != "" {
			textParts = append(textParts, p.Text)
		}
	}
	answerText := strings.Join(textParts, "")
	results := []SearchResult{}
	for i, ch := range parsed.Candidates[0].GroundingMetadata.GroundingChunks {
		u := ch.Web.URI
		if u == "" {
			u = ch.Web.URL
		}
		if u == "" {
			continue
		}
		results = append(results, SearchResult{Title: ch.Web.Title, URL: u, Position: i + 1})
	}
	ans := &answerPayload{Source: "gemini", Text: answerText, Model: model}
	return h.buildUnified(req.ProviderID, query, results, ans, len(results), parsed.UsageMetadata.TotalTokenCount), http.StatusOK, nil
}

// chatOpenAI calls chat/completions with tools:[{type:"web_search"}] (when the
// model name does not already contain "search"), extracts the answer text from
// choices[0].message.content and citations from message.annotations[].url_citation
// (fallback data.citations[]).
func (h *Handler) chatOpenAI(ctx context.Context, cfg search.Config, req Request, query string) ([]byte, int, error) {
	model := orDefault(req.Model, cfg.DefaultModel)
	tools := []any{}
	if !strings.Contains(strings.ToLower(model), "search") {
		tools = append(tools, map[string]any{"type": "web_search"})
	}
	payload := map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": query}},
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	raw, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if tok := credentialToken(req.Credentials); tok != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content     string `json:"content"`
				Annotations []struct {
					URLCitation struct {
						URL   string `json:"url"`
						Title string `json:"title"`
					} `json:"url_citation"`
				} `json:"annotations"`
			} `json:"message"`
		} `json:"choices"`
		Citations []struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"citations"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("openai: failed to parse response: %w", err)
	}
	answerText := ""
	if len(parsed.Choices) > 0 {
		answerText = parsed.Choices[0].Message.Content
	}
	results := []SearchResult{}
	seen := map[string]bool{}
	addCitation := func(u, title string) {
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		results = append(results, SearchResult{Title: title, URL: u, Position: len(results) + 1})
	}
	if len(parsed.Choices) > 0 {
		for _, a := range parsed.Choices[0].Message.Annotations {
			addCitation(a.URLCitation.URL, a.URLCitation.Title)
		}
	}
	for _, c := range parsed.Citations {
		addCitation(c.URL, c.Title)
	}
	ans := &answerPayload{Source: "openai", Text: answerText, Model: model}
	return h.buildUnified(req.ProviderID, query, results, ans, len(results), parsed.Usage.TotalTokens), http.StatusOK, nil
}

// chatPerplexity calls chat/completions with sonar (no tools — sonar searches
// natively), extracts the answer text from choices[0].message.content and
// citations from the top-level citations[] array.
func (h *Handler) chatPerplexity(ctx context.Context, cfg search.Config, req Request, query string) ([]byte, int, error) {
	model := orDefault(req.Model, cfg.DefaultModel)
	payload := map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": query}},
	}
	raw, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if tok := credentialToken(req.Credentials); tok != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Citations []struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"citations"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("perplexity: failed to parse response: %w", err)
	}
	answerText := ""
	if len(parsed.Choices) > 0 {
		answerText = parsed.Choices[0].Message.Content
	}
	results := []SearchResult{}
	for i, c := range parsed.Citations {
		if c.URL == "" {
			continue
		}
		results = append(results, SearchResult{Title: c.Title, URL: c.URL, Position: i + 1})
	}
	ans := &answerPayload{Source: "perplexity", Text: answerText, Model: model}
	return h.buildUnified(req.ProviderID, query, results, ans, len(results), parsed.Usage.TotalTokens), http.StatusOK, nil
}

// chatXai calls /v1/responses with {model,input,tools:[{type:"web_search"}]},
// extracting the answer text from output[].content[].text and citations from
// output[].content[].annotations[] ({url|url_citation}) plus a top-level
// citations[] fallback. Mirrors chatSearch.js xai.
func (h *Handler) chatXai(ctx context.Context, cfg search.Config, req Request, query string) ([]byte, int, error) {
	model := orDefault(req.Model, cfg.DefaultModel)
	payload := map[string]any{
		"model": model,
		"input": []any{map[string]any{"role": "user", "content": query}},
		"tools": []any{map[string]any{"type": "web_search"}},
	}
	raw, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if tok := credentialToken(req.Credentials); tok != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	var parsed struct {
		Output []struct {
			Content []struct {
				Text        string `json:"text"`
				Annotations []struct {
					URL         string `json:"url"`
					URLCitation struct {
						URL   string `json:"url"`
						Title string `json:"title"`
					} `json:"url_citation"`
				} `json:"annotations"`
			} `json:"content"`
		} `json:"output"`
		Citations []json.RawMessage `json:"citations"`
		Usage     struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("xai: failed to parse response: %w", err)
	}
	var textParts []string
	type citation struct{ url, title string }
	cits := []citation{}
	seen := map[string]bool{}
	addCit := func(u, title string) {
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		cits = append(cits, citation{u, title})
	}
	for _, item := range parsed.Output {
		for _, p := range item.Content {
			if p.Text != "" {
				textParts = append(textParts, p.Text)
			}
			for _, a := range p.Annotations {
				if a.URL != "" {
					addCit(a.URL, "")
				}
				addCit(a.URLCitation.URL, a.URLCitation.Title)
			}
		}
	}
	if len(cits) == 0 {
		for _, raw := range parsed.Citations {
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				addCit(s, "")
				continue
			}
			var obj struct {
				URL   string `json:"url"`
				Title string `json:"title"`
			}
			if json.Unmarshal(raw, &obj) == nil {
				addCit(obj.URL, obj.Title)
			}
		}
	}
	answerText := strings.Join(textParts, "")
	results := []SearchResult{}
	for i, c := range cits {
		results = append(results, SearchResult{Title: c.title, URL: c.url, Position: i + 1})
	}
	ans := &answerPayload{Source: "xai", Text: answerText, Model: model}
	return h.buildUnified(req.ProviderID, query, results, ans, len(results), parsed.Usage.TotalTokens), http.StatusOK, nil
}

// chatKimi calls chat/completions with tools:[{type:"builtin_function",function:
// {name:"$web_search"}}], extracting the answer text from choices[0].message.
// content and citations from message.tool_calls[].function.arguments
// (JSON {search_results|results|references:[{url|link,title,snippet|summary}]}).
// Mirrors chatSearch.js kimi.
func (h *Handler) chatKimi(ctx context.Context, cfg search.Config, req Request, query string) ([]byte, int, error) {
	model := orDefault(req.Model, cfg.DefaultModel)
	payload := map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": query}},
		"tools": []any{map[string]any{
			"type":     "builtin_function",
			"function": map[string]any{"name": "$web_search"},
		}},
	}
	raw, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if tok := credentialToken(req.Credentials); tok != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("kimi: failed to parse response: %w", err)
	}
	answerText := ""
	if len(parsed.Choices) > 0 {
		answerText = parsed.Choices[0].Message.Content
	}
	type citation struct{ url, title, snippet string }
	cits := []citation{}
	seen := map[string]bool{}
	for _, ch := range parsed.Choices {
		for _, call := range ch.Message.ToolCalls {
			argsBytes, ok := unmarshalToolCallArguments(call.Function.Arguments)
			if !ok {
				continue
			}
			var args struct {
				SearchResults []struct {
					URL     string `json:"url"`
					Link    string `json:"link"`
					Title   string `json:"title"`
					Snippet string `json:"snippet"`
					Summary string `json:"summary"`
				} `json:"search_results"`
				Results []struct {
					URL     string `json:"url"`
					Link    string `json:"link"`
					Title   string `json:"title"`
					Snippet string `json:"snippet"`
					Summary string `json:"summary"`
				} `json:"results"`
				References []struct {
					URL     string `json:"url"`
					Link    string `json:"link"`
					Title   string `json:"title"`
					Snippet string `json:"snippet"`
				} `json:"references"`
			}
			if json.Unmarshal(argsBytes, &args) != nil {
				continue
			}
			add := func(u, title, snippet string) {
				if u == "" || seen[u] {
					return
				}
				seen[u] = true
				cits = append(cits, citation{u, title, snippet})
			}
			for _, it := range args.SearchResults {
				add(firstNonEmpty(it.URL, it.Link), it.Title, firstNonEmpty(it.Snippet, it.Summary))
			}
			for _, it := range args.Results {
				add(firstNonEmpty(it.URL, it.Link), it.Title, firstNonEmpty(it.Snippet, it.Summary))
			}
			for _, it := range args.References {
				add(firstNonEmpty(it.URL, it.Link), it.Title, it.Snippet)
			}
		}
	}
	results := []SearchResult{}
	for i, c := range cits {
		results = append(results, SearchResult{Title: c.title, URL: c.url, Snippet: c.snippet, Position: i + 1})
	}
	ans := &answerPayload{Source: "kimi", Text: answerText, Model: model}
	return h.buildUnified(req.ProviderID, query, results, ans, len(results), parsed.Usage.TotalTokens), http.StatusOK, nil
}

// chatMinimax calls chatcompletion_v2 with tools:[{type:"web_search"}],
// extracting the answer text from choices[0].message.content and citations
// from the top-level web_search_results[] (or tool_calls[].function.arguments
// .results[] fallback). Mirrors chatSearch.js minimax.
func (h *Handler) chatMinimax(ctx context.Context, cfg search.Config, req Request, query string) ([]byte, int, error) {
	model := orDefault(req.Model, cfg.DefaultModel)
	payload := map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": query}},
		"tools":    []any{map[string]any{"type": "web_search"}},
	}
	raw, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if tok := credentialToken(req.Credentials); tok != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		WebSearchResults []struct {
			URL     string `json:"url"`
			Link    string `json:"link"`
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
			Summary string `json:"summary"`
		} `json:"web_search_results"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("minimax: failed to parse response: %w", err)
	}
	answerText := ""
	if len(parsed.Choices) > 0 {
		answerText = parsed.Choices[0].Message.Content
	}
	type citation struct{ url, title, snippet string }
	cits := []citation{}
	seen := map[string]bool{}
	for _, it := range parsed.WebSearchResults {
		u := firstNonEmpty(it.URL, it.Link)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		cits = append(cits, citation{u, it.Title, firstNonEmpty(it.Snippet, it.Summary)})
	}
	if len(cits) == 0 {
		for _, ch := range parsed.Choices {
			for _, call := range ch.Message.ToolCalls {
				argsBytes, ok := unmarshalToolCallArguments(call.Function.Arguments)
				if !ok {
					continue
				}
				var args struct {
					Results []struct {
						URL     string `json:"url"`
						Link    string `json:"link"`
						Title   string `json:"title"`
						Snippet string `json:"snippet"`
					} `json:"results"`
					SearchResults []struct {
						URL     string `json:"url"`
						Link    string `json:"link"`
						Title   string `json:"title"`
						Snippet string `json:"snippet"`
					} `json:"search_results"`
				}
				if json.Unmarshal(argsBytes, &args) != nil {
					continue
				}
				for _, it := range args.Results {
					u := firstNonEmpty(it.URL, it.Link)
					if u == "" || seen[u] {
						continue
					}
					seen[u] = true
					cits = append(cits, citation{u, it.Title, it.Snippet})
				}
				for _, it := range args.SearchResults {
					u := firstNonEmpty(it.URL, it.Link)
					if u == "" || seen[u] {
						continue
					}
					seen[u] = true
					cits = append(cits, citation{u, it.Title, it.Snippet})
				}
			}
		}
	}
	results := []SearchResult{}
	for i, c := range cits {
		results = append(results, SearchResult{Title: c.title, URL: c.url, Snippet: c.snippet, Position: i + 1})
	}
	ans := &answerPayload{Source: "minimax", Text: answerText, Model: model}
	return h.buildUnified(req.ProviderID, query, results, ans, len(results), parsed.Usage.TotalTokens), http.StatusOK, nil
}

// chatPerplexityAgent calls /v1/responses with {model,input,tools:[{type:
// "web_search"}]}, extracting the answer text from output[].content[].text and
// citations from output[].content[].annotations[] and output[].results[]
// ({url|link,title,snippet}) plus a top-level citations[] fallback. Mirrors
// chatSearch.js perplexity-agent.
func (h *Handler) chatPerplexityAgent(ctx context.Context, cfg search.Config, req Request, query string) ([]byte, int, error) {
	model := orDefault(req.Model, cfg.DefaultModel)
	payload := map[string]any{
		"model": model,
		"input": query,
		"tools": []any{map[string]any{"type": "web_search"}},
	}
	raw, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if tok := credentialToken(req.Credentials); tok != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	respBody, status, err := h.doUpstream(ctx, httpReq)
	if err != nil {
		return nil, status, err
	}
	if status >= 400 {
		return nil, status, upstreamError(respBody)
	}
	var parsed struct {
		Output []struct {
			Content []struct {
				Text        string `json:"text"`
				Annotations []struct {
					URL         string `json:"url"`
					URLCitation struct {
						URL   string `json:"url"`
						Title string `json:"title"`
					} `json:"url_citation"`
				} `json:"annotations"`
			} `json:"content"`
			Results []struct {
				URL     string `json:"url"`
				Link    string `json:"link"`
				Title   string `json:"title"`
				Snippet string `json:"snippet"`
			} `json:"results"`
		} `json:"output"`
		Citations []json.RawMessage `json:"citations"`
		Usage     struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("perplexity-agent: failed to parse response: %w", err)
	}
	var textParts []string
	type citation struct{ url, title, snippet string }
	cits := []citation{}
	seen := map[string]bool{}
	addCit := func(u, title, snippet string) {
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		cits = append(cits, citation{u, title, snippet})
	}
	for _, item := range parsed.Output {
		for _, p := range item.Content {
			if p.Text != "" {
				textParts = append(textParts, p.Text)
			}
			for _, a := range p.Annotations {
				if a.URL != "" {
					addCit(a.URL, "", "")
				}
				addCit(a.URLCitation.URL, a.URLCitation.Title, "")
			}
		}
		for _, r := range item.Results {
			addCit(firstNonEmpty(r.URL, r.Link), r.Title, r.Snippet)
		}
	}
	if len(cits) == 0 {
		for _, raw := range parsed.Citations {
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				addCit(s, "", "")
				continue
			}
			var obj struct {
				URL   string `json:"url"`
				Title string `json:"title"`
			}
			if json.Unmarshal(raw, &obj) == nil {
				addCit(obj.URL, obj.Title, "")
			}
		}
	}
	answerText := strings.Join(textParts, "")
	results := []SearchResult{}
	for i, c := range cits {
		results = append(results, SearchResult{Title: c.title, URL: c.url, Snippet: c.snippet, Position: i + 1})
	}
	ans := &answerPayload{Source: "perplexity-agent", Text: answerText, Model: model}
	return h.buildUnified(req.ProviderID, query, results, ans, len(results), parsed.Usage.TotalTokens), http.StatusOK, nil
}

// === shared helpers ===

// buildUnified assembles the unified response payload.
func (h *Handler) buildUnified(providerID, query string, results []SearchResult, answer *answerPayload, totalAvailable any, llmTokens int) []byte {
	resp := searchResponse{
		Provider: providerID,
		Query:    query,
		Results:  results,
		Answer:   answer,
		Usage: usagePayload{
			QueriesUsed:   1,
			SearchCostUSD: 0.0,
			LLMTokens:     llmTokens,
		},
		Metrics: metricsPayload{
			TotalResultsAvailable: totalAvailable,
		},
		Errors: []string{},
	}
	out, _ := json.Marshal(resp)
	return out
}

// doUpstream sends the request and returns the raw body + status. On transport
// error it returns a 502 status.
func (h *Handler) doUpstream(ctx context.Context, httpReq *http.Request) ([]byte, int, error) {
	resp, err := h.deps.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

// stampMetrics decodes the unified body, injects response_time_ms and
// upstream_latency_ms (approximated by the full elapsed time), and re-encodes.
func stampMetrics(body []byte, startMillis int) []byte {
	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return body
	}
	elapsed := nowMillis() - startMillis
	if elapsed < 0 {
		elapsed = 0
	}
	resp.Metrics.ResponseTimeMS = elapsed
	resp.Metrics.UpstreamLatencyMS = elapsed
	out, err := json.Marshal(resp)
	if err != nil {
		return body
	}
	return out
}

// sanitizeQuery removes control characters, applies NFKC-like cleanup, trims,
// and collapses whitespace — mirroring the JS sanitizeSearchQuery.
func sanitizeQuery(q string) string {
	var b strings.Builder
	for _, r := range q {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	s := strings.TrimSpace(b.String())
	// Collapse runs of whitespace.
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// isRetriable reports whether a dedicated-search failure status is retriable
// (i.e. should fall back to chat). 4xx client errors are not retriable.
func isRetriable(status int) bool {
	return status >= 500 || status == http.StatusBadGateway || status == http.StatusGatewayTimeout
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func orString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// firstNonEmpty returns the first non-empty argument, used for fields where
// providers offer aliases (e.g. brave page_age|age, perplexity date|last_updated).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// strconvAtoi is a thin wrapper over strconv.Atoi so google-pse / searchapi can
// coerce a string totalResults into an int without importing strconv at every
// call site (the rest of the file avoids strconv).
func strconvAtoi(s string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(s))
}

// unmarshalToolCallArguments normalizes a chat tool_call's function.arguments
// field into raw JSON bytes suitable for unmarshalling into a struct. Providers
// (kimi, minimax) encode arguments either as a JSON-encoded string —
// "arguments":"{\"search_results\":[...]}" — or as an inline object —
// "arguments":{"results":[...]}. This mirrors the legacy JS
// `typeof argStr === "string" ? JSON.parse(argStr) : argStr`. Returns (nil,
// false) when the field is empty or neither shape parses.
func unmarshalToolCallArguments(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	// Try inline object first (raw already starts with '{').
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		return raw, true
	}
	// Otherwise it's a JSON string: unescape it to get the inner JSON object.
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" {
		return nil, false
	}
	inner := strings.TrimSpace(s)
	if !strings.HasPrefix(inner, "{") {
		return nil, false
	}
	return json.RawMessage(inner), true
}

func credentialToken(c domainProv.Credentials) string {
	if c.AccessToken != "" {
		return c.AccessToken
	}
	return c.APIKey
}

func upstreamError(body []byte) error {
	// OpenAI-shape error: {"error":{"message":...}}.
	var wrapped struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &wrapped) == nil && len(wrapped.Error) > 0 {
		var nested struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(wrapped.Error, &nested) == nil && nested.Message != "" {
			return fmt.Errorf("upstream: %s", nested.Message)
		}
	}
	var bare struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &bare) == nil && bare.Message != "" {
		return fmt.Errorf("upstream: %s", bare.Message)
	}
	return fmt.Errorf("upstream error")
}

// nowMillis returns the current time in milliseconds. Wrapped so tests can
// monkey-patch if needed; the JS path used Date.now().
func nowMillis() int {
	return int(time.Now().UnixMilli())
}
