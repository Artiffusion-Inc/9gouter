// Package webfetch ports the per-provider web-fetch adapters from
// open-sse/handlers/fetch/index.js (runFirecrawl/runJina/runTavily/runExa).
// Each adapter builds the upstream URL, headers, and request body for a
// /v1/web/fetch call against a target URL, and normalizes the upstream
// response into the JS handleFetchCore buildData shape:
//
//	{
//	  "provider": "...",
//	  "url": "...",
//	  "title": null,
//	  "content": { "format":"markdown", "text":"...", "length":N },
//	  "metadata": { "author":null, "published_at":null, "language":null },
//	  "usage": { "fetch_cost_usd": null },
//	  "metrics": { "response_time_ms":N, "upstream_latency_ms":N }
//	}
//
// Auth refresh-on-401, account fallback, and combo expansion are NOT in this
// package — they are separate slices, mirroring the embeddings port scope.
package webfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Params carries the parsed /v1/web/fetch request fields an adapter needs.
// Mirrors the JS handleFetchCore({ url, format, maxCharacters, ... }).
type Params struct {
	URL            string
	Format         string // "markdown" | "html" | "text"; empty => "markdown"
	MaxCharacters  int    // <=0 means no truncation
	ProviderConfig map[string]any
}

// Result is the normalized JS buildData shape returned to the client.
type Result struct {
	Provider string
	URL      string
	Title    string
	Format   string
	Text     string
	// CostUSD is the provider's per-query cost (providerConfig.costPerQuery),
	// or empty string when unknown. Mirrors JS usage.fetch_cost_usd (null).
	CostUSD string
}

// Adapter is the per-provider web-fetch port, mirroring the JS
// runFirecrawl/runJina/runTavily/runExa shape.
type Adapter interface {
	// Fetch performs the upstream call and returns the normalized Result.
	// It is given the resolved credentials (apiKey/token) and a logger.
	Fetch(ctx context.Context, client *http.Client, creds domainProv.Credentials, p Params, log Logger) (*Result, error)
}

// Logger is a minimal log sink.
type Logger interface {
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

// Lookup returns the web-fetch adapter for a provider id. Returns
// (nil, false) for providers with no fetch support so the caller can 400
// cleanly, mirroring the JS "Unsupported provider" branch.
func Lookup(providerID string) (Adapter, bool) {
	a, ok := adapters[providerID]
	return a, ok
}

var adapters = map[string]Adapter{
	"firecrawl":   firecrawlAdapter{},
	"jina-reader": jinaAdapter{},
	"tavily":      tavilyAdapter{},
	"exa":         exaAdapter{},
}

// credentialKey extracts the apiKey/token from credentials, mirroring the JS
// credentials?.apiKey || credentials?.key || credentials?.token.
func credentialKey(creds domainProv.Credentials) string {
	if creds.APIKey != "" {
		return creds.APIKey
	}
	if v, ok := creds.ProviderSpecificData["apiKey"].(string); ok && v != "" {
		return v
	}
	if v, ok := creds.ProviderSpecificData["key"].(string); ok && v != "" {
		return v
	}
	if creds.AccessToken != "" {
		return creds.AccessToken
	}
	return ""
}

// fetchTimeoutMs mirrors the JS DEFAULT_TIMEOUT_MS / providerConfig.timeoutMs.
func fetchTimeoutMs(p Params) int {
	if p.ProviderConfig != nil {
		if v, ok := p.ProviderConfig["timeoutMs"].(float64); ok && v > 0 {
			return int(v)
		}
	}
	return 15000
}

// costPerQuery mirrors the JS providerConfig?.costPerQuery.
func costPerQuery(p Params) string {
	if p.ProviderConfig != nil {
		if v, ok := p.ProviderConfig["costPerQuery"].(string); ok {
			return v
		}
		if v, ok := p.ProviderConfig["costPerQuery"].(float64); ok {
			return fmt.Sprintf("%v", v)
		}
	}
	return ""
}

// truncateText mirrors the JS truncate(text, max).
func truncateText(text string, max int) string {
	if max <= 0 || len(text) <= max {
		return text
	}
	return text[:max]
}

// --- firecrawl ---

type firecrawlAdapter struct{}

func (firecrawlAdapter) Fetch(ctx context.Context, client *http.Client, creds domainProv.Credentials, p Params, log Logger) (*Result, error) {
	body, _ := json.Marshal(map[string]any{"url": p.URL, "formats": []string{normalizeFormat(p.Format)}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.firecrawl.dev/v1/scrape", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if k := credentialKey(creds); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("firecrawl %d: %s", resp.StatusCode, truncate(string(b), 300))
	}
	var parsed struct {
		Data struct {
			Markdown  string            `json:"markdown"`
			HTML      string            `json:"html"`
			Text      string            `json:"text"`
			Metadata  map[string]any    `json:"metadata"`
		} `json:"data"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, fmt.Errorf("firecrawl: decode: %w", err)
	}
	d := parsed.Data
	text := d.Markdown
	if text == "" {
		text = d.HTML
	}
	if text == "" {
		text = d.Text
	}
	text = truncateText(text, p.MaxCharacters)
	title, _ := d.Metadata["title"].(string)
	return &Result{
		Provider: "firecrawl", URL: p.URL, Title: title,
		Format: normalizeFormat(p.Format), Text: text, CostUSD: costPerQuery(p),
	}, nil
}

// --- jina-reader ---

type jinaAdapter struct{}

func (jinaAdapter) Fetch(ctx context.Context, client *http.Client, creds domainProv.Credentials, p Params, log Logger) (*Result, error) {
	target := "https://r.jina.ai/" + url.PathEscape(p.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if k := credentialKey(creds); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("jina-reader %d: %s", resp.StatusCode, truncate(string(b), 300))
	}
	text := truncateText(string(b), p.MaxCharacters)
	return &Result{
		Provider: "jina-reader", URL: p.URL, Title: parseJinaTitle(string(b)),
		Format: normalizeFormat(p.Format), Text: text, CostUSD: costPerQuery(p),
	}, nil
}

// parseJinaTitle mirrors the JS parseJinaTitle: first "# Title" line.
func parseJinaTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(t, "# "))
		}
	}
	return ""
}

// --- tavily ---

type tavilyAdapter struct{}

func (tavilyAdapter) Fetch(ctx context.Context, client *http.Client, creds domainProv.Credentials, p Params, log Logger) (*Result, error) {
	body, _ := json.Marshal(map[string]any{"urls": []string{p.URL}, "extract_depth": "basic"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/extract", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if k := credentialKey(creds); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("tavily %d: %s", resp.StatusCode, truncate(string(b), 300))
	}
	var parsed struct {
		Results []struct {
			RawContent string `json:"raw_content"`
		} `json:"results"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, fmt.Errorf("tavily: decode: %w", err)
	}
	text := ""
	if len(parsed.Results) > 0 {
		text = parsed.Results[0].RawContent
	}
	text = truncateText(text, p.MaxCharacters)
	return &Result{
		Provider: "tavily", URL: p.URL, Title: "",
		Format: normalizeFormat(p.Format), Text: text, CostUSD: costPerQuery(p),
	}, nil
}

// --- exa ---

type exaAdapter struct{}

func (exaAdapter) Fetch(ctx context.Context, client *http.Client, creds domainProv.Credentials, p Params, log Logger) (*Result, error) {
	body, _ := json.Marshal(map[string]any{"ids": []string{p.URL}, "text": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.exa.ai/contents", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if k := credentialKey(creds); k != "" {
		req.Header.Set("x-api-key", k)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("exa %d: %s", resp.StatusCode, truncate(string(b), 300))
	}
	var parsed struct {
		Results []struct {
			Text  string `json:"text"`
			Title string `json:"title"`
		} `json:"results"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, fmt.Errorf("exa: decode: %w", err)
	}
	text, title := "", ""
	if len(parsed.Results) > 0 {
		text = parsed.Results[0].Text
		title = parsed.Results[0].Title
	}
	text = truncateText(text, p.MaxCharacters)
	return &Result{
		Provider: "exa", URL: p.URL, Title: title,
		Format: normalizeFormat(p.Format), Text: text, CostUSD: costPerQuery(p),
	}, nil
}

// normalizeFormat mirrors the JS DEFAULT_FORMAT fallback.
func normalizeFormat(f string) string {
	if f == "" {
		return "markdown"
	}
	return f
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}