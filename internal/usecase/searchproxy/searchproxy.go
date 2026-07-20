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
//   - Dedicated: serper, tavily, searxng (full request build + normalize).
//   - Chat: gemini (generateContent + google_search tool, grounding chunks),
//     openai (chat/completions + web_search tool, annotations citations),
//     perplexity-chat fallback (chat/completions + top-level citations).
//
// Deferred (501): brave-search, google-pse, linkup, searchapi, youcom, exa
// (dedicated), xai/kimi/minimax/perplexity-agent (chat). Registered so the
// handler can 501 them honestly. Combo expansion, account-fallback rotation,
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
	Ctx          context.Context
	ProviderID   string
	Query        string
	Model        string // optional override (chat-based search)
	MaxResults   int
	SearchType   string // "web" (default) | "news"
	Country      string
	Language     string
	TimeRange    string
	Offset       int
	Credentials  domainProv.Credentials
	UserAgent    string
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
	Title      string `json:"title"`
	URL        string `json:"url"`
	Snippet    string `json:"snippet,omitempty"`
	Position   int    `json:"position,omitempty"`
	Score      any    `json:"score,omitempty"`
	PublishedAt any   `json:"published_at,omitempty"`
}

// searchResponse is the unified response body.
type searchResponse struct {
	Provider string          `json:"provider"`
	Query    string          `json:"query"`
	Results  []SearchResult  `json:"results"`
	Answer   *answerPayload  `json:"answer"`
	Usage    usagePayload    `json:"usage"`
	Metrics  metricsPayload  `json:"metrics"`
	Errors   []string        `json:"errors"`
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
	ResponseTimeMS       int `json:"response_time_ms"`
	UpstreamLatencyMS    int `json:"upstream_latency_ms"`
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
			Title        string `json:"title"`
			URL          string `json:"url"`
			Content      string `json:"content"`
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