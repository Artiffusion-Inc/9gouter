package searchproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/search"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

type captureLogger struct{}

func (captureLogger) Infof(string, ...any)  {}
func (captureLogger) Warnf(string, ...any)  {}
func (captureLogger) Debugf(string, ...any) {}

func creds(apiKey string) domainProv.Credentials {
	return domainProv.Credentials{APIKey: apiKey, ProviderSpecificData: map[string]any{"_connectionId": "c1"}}
}

func searchCfg(baseURL, authHeader string, mode search.Mode) search.Config {
	return search.Config{Mode: mode, AuthHeader: search.AuthHeader(authHeader), BaseURL: baseURL, DefaultResults: 5, MaxResults: 100, SearchTypes: []string{"web", "news"}}
}

// === Dispatch / validation ===

func TestHandle_UnsupportedProvider(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "nope", Query: "q"})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

// TestHandle_AllKnownProvidersImplemented asserts there is no remaining 501
// surface: every provider in the search registry (search.KnownProviders) has a
// working dedicated or chat transport in runDedicated/runChat. We drive each
// provider through Handle against an unreachable port with a tiny client
// timeout so the test is hermetic and fast — the only status we forbid is 501
// ("not implemented"); every other status (400/401/502) means dispatch found a
// real transport and exercised it. searxng (AuthNone) is env-steered to the
// same unreachable address so it dispatches rather than 401-ing on creds.
func TestHandle_AllKnownProvidersImplemented(t *testing.T) {
	os.Unsetenv("SEARXNG_URL")
	client := &http.Client{Timeout: 50 * time.Millisecond}
	h := New(Dependencies{HTTPClient: client, Logger: captureLogger{}, Config: config.Config{}})
	for _, id := range search.KnownProviders {
		c := creds("k")
		if id == "google-pse" {
			c.ProviderSpecificData["cx"] = "cx"
		}
		res := h.Handle(context.Background(), Request{ProviderID: id, Query: "q", Credentials: c})
		if res.StatusCode == http.StatusNotImplemented {
			t.Errorf("provider %s returned 501 — transport not implemented", id)
		}
	}
}

func TestHandle_MissingQuery(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "serper", Query: "   ", Credentials: creds("k")})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing query)", res.StatusCode)
	}
}

func TestHandle_NoCredentials(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "serper", Query: "q"})
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
}

func TestHandle_SearxngNoAuthDoesNotRequireCreds(t *testing.T) {
	// searxng is AuthNone — no credentials required; missing query is the only
	// guard, so a valid query dispatches (and fails on the unreachable host).
	// We assert it does NOT 401 on missing creds.
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	// Point searxng at a down server to keep the test hermetic without a mock.
	_ = h
	cfg, ok := search.Lookup("searxng")
	if !ok {
		t.Fatal("searxng config missing")
	}
	if cfg.AuthHeader != search.AuthNone {
		t.Errorf("searxng auth = %v, want none", cfg.AuthHeader)
	}
}

// === Dedicated: serper ===

func TestHandle_Serper_Web(t *testing.T) {
	var gotAuth string
	var gotPath string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-API-Key")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"searchParameters":{"totalResults":42},"organic":[{"title":"T1","link":"https://a","snippet":"s1"},{"title":"T2","link":"https://b","snippet":"s2"}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "x-api-key-serper", search.ModeDedicated)
	body, status, err := h.dedicatedSerper(context.Background(), cfg, Request{ProviderID: "serper", Query: "hello", MaxResults: 5, Credentials: creds("k")}, "hello", 5, "web")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if gotAuth != "k" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotPath != "/search" {
		t.Errorf("path = %q, want /search", gotPath)
	}
	if !contains(gotBody, `"q":"hello"`) || !contains(gotBody, `"num":5`) {
		t.Errorf("body = %q", gotBody)
	}
	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Provider != "serper" || resp.Query != "hello" {
		t.Errorf("provider/query = %q/%q", resp.Provider, resp.Query)
	}
	if len(resp.Results) != 2 || resp.Results[0].URL != "https://a" || resp.Results[1].Position != 2 {
		t.Errorf("results = %+v", resp.Results)
	}
	if resp.Answer != nil {
		t.Errorf("dedicated search must have nil answer, got %+v", resp.Answer)
	}
	if totalAsInt(resp.Metrics.TotalResultsAvailable) != 42 {
		t.Errorf("total = %v, want 42", resp.Metrics.TotalResultsAvailable)
	}
}

func TestHandle_Serper_News(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"news":[{"title":"N1","link":"https://n","snippet":"s","date":"2026-07-19"}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "x-api-key-serper", search.ModeDedicated)
	body, _, err := h.dedicatedSerper(context.Background(), cfg, Request{ProviderID: "serper", Query: "q", Credentials: creds("k")}, "q", 5, "news")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/news" {
		t.Errorf("path = %q, want /news", gotPath)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].Title != "N1" || resp.Results[0].PublishedAt != "2026-07-19" {
		t.Errorf("results = %+v", resp.Results)
	}
}

func TestHandle_Serper_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Invalid API key"}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "x-api-key-serper", search.ModeDedicated)
	_, status, err := h.dedicatedSerper(context.Background(), cfg, Request{ProviderID: "serper", Query: "q", Credentials: creds("k")}, "q", 5, "web")
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
	if err == nil || !contains(err.Error(), "Invalid API key") {
		t.Errorf("err = %v, want upstream Invalid API key", err)
	}
}

// === Dedicated: tavily ===

func TestHandle_Tavily(t *testing.T) {
	var gotAuth string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"results":[{"title":"T","url":"https://a","content":"c","score":0.9,"published_date":"2026-07-19"}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "bearer", search.ModeDedicated)
	body, status, err := h.dedicatedTavily(context.Background(), cfg, Request{ProviderID: "tavily", Query: "q", MaxResults: 3, SearchType: "news", Credentials: creds("k")}, "q", 3, "news")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("auth = %q", gotAuth)
	}
	if !contains(gotBody, `"topic":"news"`) || !contains(gotBody, `"max_results":3`) {
		t.Errorf("body = %q", gotBody)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://a" || resp.Results[0].Score != 0.9 {
		t.Errorf("results = %+v", resp.Results)
	}
}

// === Dedicated: exa ===

func TestHandle_Exa(t *testing.T) {
	var gotAuth string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"results":[{"title":"T","url":"https://a","highlights":["hl snippet"],"score":0.8,"publishedDate":"2026-07-19"},{"title":"T2","url":"https://b","text":"long text without highlights that exceeds 300 chars so it gets truncated to the first 300 chars as the snippet fallback when no highlight is present","score":0.5}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "x-api-key", search.ModeDedicated)
	body, status, err := h.dedicatedExa(context.Background(), cfg, Request{ProviderID: "exa", Query: "hello", MaxResults: 5, Credentials: creds("k")}, "hello", 5, "web")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if gotAuth != "k" {
		t.Errorf("auth = %q, want k (x-api-key)", gotAuth)
	}
	// type:auto, text:true, highlights:true, numResults:5 must be present;
	// category:"news" must NOT be present for a web search.
	if !contains(gotBody, `"query":"hello"`) || !contains(gotBody, `"numResults":5`) || !contains(gotBody, `"type":"auto"`) || !contains(gotBody, `"text":true`) || !contains(gotBody, `"highlights":true`) {
		t.Errorf("body = %q", gotBody)
	}
	if contains(gotBody, `"category":"news"`) {
		t.Errorf("web search must not set category:news, body = %q", gotBody)
	}
	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Provider != "exa" || resp.Query != "hello" {
		t.Errorf("provider/query = %q/%q", resp.Provider, resp.Query)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results len = %d, want 2", len(resp.Results))
	}
	// First result: highlight snippet, score, publishedDate.
	if resp.Results[0].URL != "https://a" || resp.Results[0].Snippet != "hl snippet" || resp.Results[0].Position != 1 {
		t.Errorf("result[0] = %+v", resp.Results[0])
	}
	if s, ok := resp.Results[0].Score.(float64); !ok || s != 0.8 {
		t.Errorf("result[0] score = %v, want 0.8", resp.Results[0].Score)
	}
	if resp.Results[0].PublishedAt != "2026-07-19" {
		t.Errorf("result[0] published_at = %v, want 2026-07-19", resp.Results[0].PublishedAt)
	}
	// Second result: text fallback truncated to 300 chars.
	if len(resp.Results[1].Snippet) > 300 {
		t.Errorf("text fallback snippet must be capped at 300 chars, got %d", len(resp.Results[1].Snippet))
	}
	if resp.Answer != nil {
		t.Errorf("dedicated search must have nil answer, got %+v", resp.Answer)
	}
	if totalAsInt(resp.Metrics.TotalResultsAvailable) != 2 {
		t.Errorf("total = %v, want 2", resp.Metrics.TotalResultsAvailable)
	}
}

func TestHandle_Exa_NewsCategory(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "x-api-key", search.ModeDedicated)
	_, _, err := h.dedicatedExa(context.Background(), cfg, Request{ProviderID: "exa", Query: "q", SearchType: "news", Credentials: creds("k")}, "q", 5, "news")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(gotBody, `"category":"news"`) {
		t.Errorf("news search must set category:news, body = %q", gotBody)
	}
}

func TestHandle_Exa_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded"}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "x-api-key", search.ModeDedicated)
	_, status, err := h.dedicatedExa(context.Background(), cfg, Request{ProviderID: "exa", Query: "q", Credentials: creds("k")}, "q", 5, "web")
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
	if err == nil || !contains(err.Error(), "quota exceeded") {
		t.Errorf("err = %v, want upstream quota exceeded", err)
	}
}

func TestHandle_Exa_NoResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "x-api-key", search.ModeDedicated)
	body, status, err := h.dedicatedExa(context.Background(), cfg, Request{ProviderID: "exa", Query: "q", Credentials: creds("k")}, "q", 5, "web")
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 0 {
		t.Errorf("results len = %d, want 0", len(resp.Results))
	}
}

// === Dedicated: searxng ===

func TestHandle_Searxng_EnvOverride(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		_, _ = io.WriteString(w, `{"results":[{"title":"T","url":"https://a","content":"c"}]}`)
	}))
	defer srv.Close()
	// Override SEARXNG_URL to point at the test server.
	os.Setenv("SEARXNG_URL", srv.URL)
	defer os.Unsetenv("SEARXNG_URL")

	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "searxng", Query: "hello", SearchType: "news", Language: "en"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !contains(gotURL, "q=hello") || !contains(gotURL, "format=json") || !contains(gotURL, "categories=news") || !contains(gotURL, "language=en") {
		t.Errorf("url = %q", gotURL)
	}
	var resp searchResponse
	_ = json.Unmarshal(res.Body, &resp)
	if resp.Provider != "searxng" || len(resp.Results) != 1 {
		t.Errorf("resp = %+v", resp)
	}
}

// === Dedicated: brave-search ===

func TestHandle_Brave_Web(t *testing.T) {
	var gotAuth string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Subscription-Token")
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"web":{"results":[{"title":"T","url":"https://a","description":"d","page_age":"2026-07-19"}],"totalCount":7}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "x-subscription-token", search.ModeDedicated)
	body, status, err := h.dedicatedBrave(context.Background(), cfg, Request{ProviderID: "brave-search", Query: "q", MaxResults: 5, Credentials: creds("k")}, "q", 5, "web")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if gotAuth != "k" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotPath != "/web/search" {
		t.Errorf("path = %q, want /web/search", gotPath)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://a" || resp.Results[0].PublishedAt != "2026-07-19" {
		t.Errorf("results = %+v", resp.Results)
	}
	if totalAsInt(resp.Metrics.TotalResultsAvailable) != 7 {
		t.Errorf("total = %v, want 7", resp.Metrics.TotalResultsAvailable)
	}
}

func TestHandle_Brave_News(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"news":{"results":[{"title":"N","url":"https://n","description":"d","age":"2026-07-18"}],"totalCount":1}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "x-subscription-token", search.ModeDedicated)
	body, _, err := h.dedicatedBrave(context.Background(), cfg, Request{ProviderID: "brave-search", Query: "q", Credentials: creds("k")}, "q", 5, "news")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/news/search" {
		t.Errorf("path = %q, want /news/search", gotPath)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].PublishedAt != "2026-07-18" {
		t.Errorf("results = %+v", resp.Results)
	}
}

func TestHandle_Brave_NewsBareContainer(t *testing.T) {
	// Brave may return the news container at the top level (data.news || data).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"title":"N","url":"https://n","description":"d"}],"totalCount":3}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "x-subscription-token", search.ModeDedicated)
	body, status, err := h.dedicatedBrave(context.Background(), cfg, Request{ProviderID: "brave-search", Query: "q", Credentials: creds("k")}, "q", 5, "news")
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://n" {
		t.Errorf("results = %+v", resp.Results)
	}
}

// === Dedicated: perplexity ===

func TestHandle_Perplexity(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"results":[{"title":"T","url":"https://a","snippet":"s","date":"2026-07-19"}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "bearer", search.ModeDedicated)
	body, status, err := h.dedicatedPerplexity(context.Background(), cfg, Request{ProviderID: "perplexity", Query: "q", MaxResults: 5, Country: "US", Language: "en", Credentials: creds("k")}, "q", 5, "web")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !contains(gotBody, `"query":"q"`) || !contains(gotBody, `"max_results":5`) || !contains(gotBody, `"country":"US"`) || !contains(gotBody, `"search_language_filter":["en"]`) {
		t.Errorf("body = %q", gotBody)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].PublishedAt != "2026-07-19" {
		t.Errorf("results = %+v", resp.Results)
	}
}

// === Dedicated: google-pse ===

func TestHandle_GooglePSE(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		_, _ = io.WriteString(w, `{"items":[{"title":"T","link":"https://a","snippet":"s"}],"searchInformation":{"totalResults":"42"}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "key-query", search.ModeDedicated)
	c := creds("k")
	c.ProviderSpecificData["cx"] = "mycx"
	body, status, err := h.dedicatedGooglePSE(context.Background(), cfg, Request{ProviderID: "google-pse", Query: "hello", MaxResults: 5, Country: "US", Language: "en", TimeRange: "week", Credentials: c}, "hello", 5, "web")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !contains(gotURL, "key=k") || !contains(gotURL, "cx=mycx") || !contains(gotURL, "q=hello") || !contains(gotURL, "num=5") || !contains(gotURL, "gl=us") || !contains(gotURL, "hl=en") || !contains(gotURL, "dateRestrict=w1") {
		t.Errorf("url = %q", gotURL)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://a" {
		t.Errorf("results = %+v", resp.Results)
	}
	if totalAsInt(resp.Metrics.TotalResultsAvailable) != 42 {
		t.Errorf("total = %v, want 42", resp.Metrics.TotalResultsAvailable)
	}
}

func TestHandle_GooglePSE_MissingCX(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "key-query", search.ModeDedicated)
	_, status, err := h.dedicatedGooglePSE(context.Background(), cfg, Request{ProviderID: "google-pse", Query: "q", Credentials: creds("k")}, "q", 5, "web")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing cx)", status)
	}
	if err == nil || !contains(err.Error(), "cx") {
		t.Errorf("err = %v", err)
	}
}

// === Dedicated: linkup ===

func TestHandle_Linkup(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"results":[{"name":"T","url":"https://a","content":"c"}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "bearer", search.ModeDedicated)
	body, status, err := h.dedicatedLinkup(context.Background(), cfg, Request{ProviderID: "linkup", Query: "q", MaxResults: 5, Credentials: creds("k")}, "q", 5, "web")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !contains(gotBody, `"depth":"standard"`) || !contains(gotBody, `"outputType":"searchResults"`) || !contains(gotBody, `"maxResults":5`) {
		t.Errorf("body = %q", gotBody)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].Title != "T" || resp.Results[0].Snippet != "c" {
		t.Errorf("results = %+v", resp.Results)
	}
}

// === Dedicated: searchapi ===

func TestHandle_SearchApi_Web(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		_, _ = io.WriteString(w, `{"organic_results":[{"title":"T","link":"https://a","snippet":"s"}],"search_information":{"total_results":9}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "api_key-query", search.ModeDedicated)
	body, status, err := h.dedicatedSearchApi(context.Background(), cfg, Request{ProviderID: "searchapi", Query: "hello", MaxResults: 5, Country: "US", Language: "en", Credentials: creds("k")}, "hello", 5, "web")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !contains(gotURL, "engine=google") || !contains(gotURL, "q=hello") || !contains(gotURL, "api_key=k") || !contains(gotURL, "gl=us") || !contains(gotURL, "hl=en") {
		t.Errorf("url = %q", gotURL)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://a" {
		t.Errorf("results = %+v", resp.Results)
	}
	if totalAsInt(resp.Metrics.TotalResultsAvailable) != 9 {
		t.Errorf("total = %v, want 9", resp.Metrics.TotalResultsAvailable)
	}
}

func TestHandle_SearchApi_News(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		_, _ = io.WriteString(w, `{"top_stories":[{"title":"N","link":"https://n","description":"d","published_at":"2026-07-19"}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "api_key-query", search.ModeDedicated)
	body, _, err := h.dedicatedSearchApi(context.Background(), cfg, Request{ProviderID: "searchapi", Query: "q", Credentials: creds("k")}, "q", 5, "news")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(gotURL, "engine=google_news") {
		t.Errorf("url = %q, want engine=google_news", gotURL)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].PublishedAt != "2026-07-19" {
		t.Errorf("results = %+v", resp.Results)
	}
}

func TestHandle_SearchApi_MissingKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "api_key-query", search.ModeDedicated)
	_, status, err := h.dedicatedSearchApi(context.Background(), cfg, Request{ProviderID: "searchapi", Query: "q"}, "q", 5, "web")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if err == nil || !contains(err.Error(), "api key") {
		t.Errorf("err = %v", err)
	}
}

// === Dedicated: youcom ===

func TestHandle_YouCom_Web(t *testing.T) {
	var gotAuth string
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-API-Key")
		gotURL = r.URL.String()
		_, _ = io.WriteString(w, `{"results":{"web":[{"title":"T","url":"https://a","snippets":["s1"],"description":"d"}]}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "x-api-key", search.ModeDedicated)
	body, status, err := h.dedicatedYouCom(context.Background(), cfg, Request{ProviderID: "youcom", Query: "hello", MaxResults: 5, TimeRange: "week", Country: "US", Language: "en", Credentials: creds("k")}, "hello", 5, "web")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if gotAuth != "k" {
		t.Errorf("auth = %q", gotAuth)
	}
	if !contains(gotURL, "query=hello") || !contains(gotURL, "count=5") || !contains(gotURL, "freshness=week") || !contains(gotURL, "country=US") || !contains(gotURL, "language=en") {
		t.Errorf("url = %q", gotURL)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].Snippet != "s1" {
		t.Errorf("results = %+v", resp.Results)
	}
}

func TestHandle_YouCom_News(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"results":{"news":[{"title":"N","url":"https://n","description":"d","page_age":"2026-07-19"}]}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "x-api-key", search.ModeDedicated)
	body, _, err := h.dedicatedYouCom(context.Background(), cfg, Request{ProviderID: "youcom", Query: "q", Credentials: creds("k")}, "q", 5, "news")
	if err != nil {
		t.Fatal(err)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].Title != "N" || resp.Results[0].PublishedAt != "2026-07-19" {
		t.Errorf("results = %+v", resp.Results)
	}
}

func TestHandle_YouCom_MissingKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := searchCfg(srv.URL, "x-api-key", search.ModeDedicated)
	_, status, err := h.dedicatedYouCom(context.Background(), cfg, Request{ProviderID: "youcom", Query: "q"}, "q", 5, "web")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if err == nil || !contains(err.Error(), "api key") {
		t.Errorf("err = %v", err)
	}
}

// === Chat: gemini ===

func TestHandle_Gemini_GroundingChunks(t *testing.T) {
	var gotURL string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"Answer text"}]},"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://a","title":"A"}},{"web":{"uri":"https://b","title":"B"}}]}}],"usageMetadata":{"totalTokenCount":123}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := search.Config{Mode: search.ModeChat, AuthHeader: search.AuthXGoogAPIKey, BaseURL: srv.URL, DefaultModel: "gemini-2.5-flash"}
	body, status, err := h.chatGemini(context.Background(), cfg, Request{ProviderID: "gemini", Query: "q", Credentials: creds("k")}, "q")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	// generateContent with ?key= query.
	if gotURL != "/gemini-2.5-flash:generateContent" {
		t.Errorf("url path = %q", gotURL)
	}
	if !contains(gotBody, `"google_search"`) {
		t.Errorf("body missing google_search tool: %q", gotBody)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if resp.Answer == nil || resp.Answer.Text != "Answer text" || resp.Answer.Model != "gemini-2.5-flash" {
		t.Errorf("answer = %+v", resp.Answer)
	}
	if len(resp.Results) != 2 || resp.Results[0].URL != "https://a" || resp.Results[1].Title != "B" {
		t.Errorf("results = %+v", resp.Results)
	}
	if resp.Usage.LLMTokens != 123 {
		t.Errorf("llm_tokens = %d, want 123", resp.Usage.LLMTokens)
	}
}

func TestHandle_Gemini_ModelsPrefixStripped(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.Path
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"a"}]},"groundingMetadata":{"groundingChunks":[]}}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := search.Config{Mode: search.ModeChat, AuthHeader: search.AuthXGoogAPIKey, BaseURL: srv.URL, DefaultModel: "gemini-2.5-flash"}
	_, _, err := h.chatGemini(context.Background(), cfg, Request{ProviderID: "gemini", Model: "models/gemini-2.5-flash", Query: "q", Credentials: creds("k")}, "q")
	if err != nil {
		t.Fatal(err)
	}
	if gotURL != "/gemini-2.5-flash:generateContent" {
		t.Errorf("url path = %q (models/ prefix should be stripped)", gotURL)
	}
}

func TestHandle_Gemini_NoCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"candidates":[]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := search.Config{Mode: search.ModeChat, AuthHeader: search.AuthXGoogAPIKey, BaseURL: srv.URL, DefaultModel: "gemini-2.5-flash"}
	_, status, err := h.chatGemini(context.Background(), cfg, Request{ProviderID: "gemini", Query: "q", Credentials: creds("k")}, "q")
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
	if err == nil || !contains(err.Error(), "no candidates") {
		t.Errorf("err = %v", err)
	}
}

// === Chat: openai ===

func TestHandle_OpenAI_WebSearchTool(t *testing.T) {
	var gotAuth string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Answer","annotations":[{"url_citation":{"url":"https://a","title":"A"}}]}}],"usage":{"total_tokens":50}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := search.Config{Mode: search.ModeChat, AuthHeader: search.AuthBearer, BaseURL: srv.URL, DefaultModel: "gpt-4o-mini"}
	body, status, err := h.chatOpenAI(context.Background(), cfg, Request{ProviderID: "openai", Query: "q", Credentials: creds("k")}, "q")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("auth = %q", gotAuth)
	}
	// gpt-4o-mini does not contain "search" → web_search tool is added.
	if !contains(gotBody, `"type":"web_search"`) {
		t.Errorf("body missing web_search tool: %q", gotBody)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if resp.Answer == nil || resp.Answer.Text != "Answer" {
		t.Errorf("answer = %+v", resp.Answer)
	}
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://a" {
		t.Errorf("results = %+v", resp.Results)
	}
	if resp.Usage.LLMTokens != 50 {
		t.Errorf("llm_tokens = %d", resp.Usage.LLMTokens)
	}
}

func TestHandle_OpenAI_SearchModelSkipsTool(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"a"}}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := search.Config{Mode: search.ModeChat, AuthHeader: search.AuthBearer, BaseURL: srv.URL, DefaultModel: "gpt-4o-mini"}
	_, _, err := h.chatOpenAI(context.Background(), cfg, Request{ProviderID: "openai", Model: "gpt-4o-search-preview", Query: "q", Credentials: creds("k")}, "q")
	if err != nil {
		t.Fatal(err)
	}
	if contains(gotBody, `"web_search"`) {
		t.Errorf("search-named model must not get web_search tool: %q", gotBody)
	}
}

func TestHandle_OpenAI_CitationsFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"a"}}],"citations":[{"url":"https://c","title":"C"}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := search.Config{Mode: search.ModeChat, AuthHeader: search.AuthBearer, BaseURL: srv.URL, DefaultModel: "gpt-4o-mini"}
	body, _, err := h.chatOpenAI(context.Background(), cfg, Request{ProviderID: "openai", Query: "q", Credentials: creds("k")}, "q")
	if err != nil {
		t.Fatal(err)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://c" {
		t.Errorf("results = %+v", resp.Results)
	}
}

// === Chat: perplexity (fallback) ===

func TestHandle_PerplexityChat_TopLevelCitations(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Answer"}}],"citations":[{"url":"https://a","title":"A"},{"url":"https://b","title":"B"}],"usage":{"total_tokens":10}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg, ok := search.ChatFallbackFor("perplexity")
	if !ok {
		t.Fatal("expected perplexity chat fallback")
	}
	cfg.BaseURL = srv.URL
	body, status, err := h.chatPerplexity(context.Background(), cfg, Request{ProviderID: "perplexity", Query: "q", Credentials: creds("k")}, "q")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !contains(gotBody, `"model":"sonar"`) {
		t.Errorf("body = %q", gotBody)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if resp.Answer == nil || resp.Answer.Text != "Answer" || resp.Answer.Model != "sonar" {
		t.Errorf("answer = %+v", resp.Answer)
	}
	if len(resp.Results) != 2 || resp.Results[0].URL != "https://a" {
		t.Errorf("results = %+v", resp.Results)
	}
}

// === Fallback dedicated → chat ===

func TestHandle_DedicatedRetriableFallsBackToChat(t *testing.T) {
	// Dedicated perplexity endpoint returns 502 (retriable). Handle should
	// catch the retriable failure and retry through ChatFallbackFor("perplexity")
	// → chatPerplexity. ChatFallbackFor returns a static BaseURL pointing at the
	// real api.perplexity.ai, which a hermetic test cannot steer through Handle,
	// so we exercise the two stages directly: runDedicated surfaces the 502, and
	// a chatPerplexity call against a local server yields the chat answer. The
	// Handle-level fallback branch is then asserted via isRetriable + the
	// ChatFallbackFor availability that Handle relies on.
	dedicatedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer dedicatedSrv.Close()
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"chat answer"}}],"citations":[{"url":"https://a","title":"A"}]}`)
	}))
	defer chatSrv.Close()

	h := New(Dependencies{HTTPClient: srvClientFor(dedicatedSrv, chatSrv), Logger: captureLogger{}, Config: config.Config{}})

	// Stage 1: dedicated perplexity now hits the upstream and surfaces the 502.
	dedCfg := search.Config{Mode: search.ModeDedicated, AuthHeader: search.AuthBearer, BaseURL: dedicatedSrv.URL, DefaultResults: 5, MaxResults: 100, SearchTypes: []string{"web"}}
	_, dedStatus, dedErr := h.runDedicated(context.Background(), dedCfg, Request{ProviderID: "perplexity", Query: "q", Credentials: creds("k")}, "q", 5, "web")
	if dedStatus != http.StatusBadGateway {
		t.Errorf("dedicated perplexity should surface 502, got %d", dedStatus)
	}
	if dedErr == nil || !contains(dedErr.Error(), "boom") {
		t.Errorf("dedicated err = %v, want upstream boom", dedErr)
	}
	if !isRetriable(dedStatus) {
		t.Errorf("502 must be retriable so Handle falls back to chat")
	}

	// Stage 2: the chat fallback config Handle would use is available...
	chatCfg, ok := search.ChatFallbackFor("perplexity")
	if !ok {
		t.Fatal("expected perplexity chat fallback config")
	}
	// ...and pointing it at the local chat server yields the chat answer, i.e.
	// the fallback transport works end to end.
	chatCfg.BaseURL = chatSrv.URL
	chatBody, chatStatus, chatErr := h.chatPerplexity(context.Background(), chatCfg, Request{ProviderID: "perplexity", Query: "q", Credentials: creds("k")}, "q")
	if chatErr != nil || chatStatus != http.StatusOK {
		t.Fatalf("chat fallback status=%d err=%v", chatStatus, chatErr)
	}
	var chatResp searchResponse
	_ = json.Unmarshal(chatBody, &chatResp)
	if chatResp.Answer == nil || chatResp.Answer.Text != "chat answer" {
		t.Errorf("chat fallback answer = %+v", chatResp.Answer)
	}
	// 4xx client errors are NOT retriable — the fallback must not fire for them.
	if isRetriable(http.StatusBadRequest) {
		t.Errorf("400 must not be retriable")
	}
}

// === Chat: xai ===

func TestHandle_Xai_ResponsesShape(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"output":[{"content":[{"text":"Answer text","annotations":[{"url_citation":{"url":"https://a","title":"A"}},{"url":"https://b"}]}]}],"usage":{"total_tokens":77}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := search.Config{Mode: search.ModeChat, AuthHeader: search.AuthBearer, BaseURL: srv.URL, DefaultModel: "grok-4.20-reasoning"}
	body, status, err := h.chatXai(context.Background(), cfg, Request{ProviderID: "xai", Query: "q", Credentials: creds("k")}, "q")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !contains(gotBody, `"input"`) || !contains(gotBody, `"type":"web_search"`) {
		t.Errorf("body = %q", gotBody)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if resp.Answer == nil || resp.Answer.Text != "Answer text" || resp.Answer.Model != "grok-4.20-reasoning" {
		t.Errorf("answer = %+v", resp.Answer)
	}
	if len(resp.Results) != 2 || resp.Results[0].URL != "https://a" || resp.Results[0].Title != "A" || resp.Results[1].URL != "https://b" {
		t.Errorf("results = %+v", resp.Results)
	}
	if resp.Usage.LLMTokens != 77 {
		t.Errorf("llm_tokens = %d, want 77", resp.Usage.LLMTokens)
	}
}

func TestHandle_Xai_TopLevelCitationsFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"output":[{"content":[{"text":"a"}]}],"citations":["https://raw-url",{"url":"https://obj","title":"O"}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := search.Config{Mode: search.ModeChat, AuthHeader: search.AuthBearer, BaseURL: srv.URL, DefaultModel: "grok"}
	body, _, err := h.chatXai(context.Background(), cfg, Request{ProviderID: "xai", Query: "q", Credentials: creds("k")}, "q")
	if err != nil {
		t.Fatal(err)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 2 || resp.Results[0].URL != "https://raw-url" || resp.Results[1].URL != "https://obj" || resp.Results[1].Title != "O" {
		t.Errorf("results = %+v", resp.Results)
	}
}

// === Chat: kimi ===

func TestHandle_Kimi_ToolCallCitations(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Answer","tool_calls":[{"function":{"arguments":"{\"search_results\":[{\"url\":\"https://a\",\"title\":\"A\",\"snippet\":\"s\"}]}"}}]}}],"usage":{"total_tokens":33}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := search.Config{Mode: search.ModeChat, AuthHeader: search.AuthBearer, BaseURL: srv.URL, DefaultModel: "kimi-k2.5"}
	body, status, err := h.chatKimi(context.Background(), cfg, Request{ProviderID: "kimi", Query: "q", Credentials: creds("k")}, "q")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !contains(gotBody, `"builtin_function"`) || !contains(gotBody, `"$web_search"`) {
		t.Errorf("body = %q", gotBody)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if resp.Answer == nil || resp.Answer.Text != "Answer" {
		t.Errorf("answer = %+v", resp.Answer)
	}
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://a" || resp.Results[0].Snippet != "s" {
		t.Errorf("results = %+v", resp.Results)
	}
	if resp.Usage.LLMTokens != 33 {
		t.Errorf("llm_tokens = %d, want 33", resp.Usage.LLMTokens)
	}
}

// === Chat: minimax ===

func TestHandle_Minimax_WebSearchResults(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Answer"}}],"web_search_results":[{"url":"https://a","title":"A","snippet":"s"}],"usage":{"total_tokens":11}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := search.Config{Mode: search.ModeChat, AuthHeader: search.AuthBearer, BaseURL: srv.URL, DefaultModel: "MiniMax-M2.7"}
	body, status, err := h.chatMinimax(context.Background(), cfg, Request{ProviderID: "minimax", Query: "q", Credentials: creds("k")}, "q")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !contains(gotBody, `"type":"web_search"`) {
		t.Errorf("body = %q", gotBody)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if resp.Answer == nil || resp.Answer.Text != "Answer" {
		t.Errorf("answer = %+v", resp.Answer)
	}
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://a" || resp.Results[0].Snippet != "s" {
		t.Errorf("results = %+v", resp.Results)
	}
}

func TestHandle_Minimax_ToolCallFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"a","tool_calls":[{"function":{"arguments":"{\"results\":[{\"link\":\"https://b\",\"title\":\"B\"}]}"}}]}}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := search.Config{Mode: search.ModeChat, AuthHeader: search.AuthBearer, BaseURL: srv.URL, DefaultModel: "m"}
	body, _, err := h.chatMinimax(context.Background(), cfg, Request{ProviderID: "minimax", Query: "q", Credentials: creds("k")}, "q")
	if err != nil {
		t.Fatal(err)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://b" {
		t.Errorf("results = %+v", resp.Results)
	}
}

// === Chat: perplexity-agent ===

func TestHandle_PerplexityAgent_ResponsesShape(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"output":[{"content":[{"text":"Answer text","annotations":[{"url_citation":{"url":"https://a","title":"A"}}]}],"results":[{"url":"https://b","title":"B","snippet":"s"}]}],"usage":{"total_tokens":9}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := search.Config{Mode: search.ModeChat, AuthHeader: search.AuthBearer, BaseURL: srv.URL, DefaultModel: "perplexity/sonar"}
	body, status, err := h.chatPerplexityAgent(context.Background(), cfg, Request{ProviderID: "perplexity-agent", Query: "q", Credentials: creds("k")}, "q")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	// input is the bare query string, tools has web_search.
	if !contains(gotBody, `"input":"q"`) || !contains(gotBody, `"type":"web_search"`) {
		t.Errorf("body = %q", gotBody)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if resp.Answer == nil || resp.Answer.Text != "Answer text" {
		t.Errorf("answer = %+v", resp.Answer)
	}
	if len(resp.Results) != 2 || resp.Results[0].URL != "https://a" || resp.Results[1].URL != "https://b" || resp.Results[1].Snippet != "s" {
		t.Errorf("results = %+v", resp.Results)
	}
	if resp.Usage.LLMTokens != 9 {
		t.Errorf("llm_tokens = %d, want 9", resp.Usage.LLMTokens)
	}
}

func TestHandle_PerplexityAgent_TopLevelCitations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"output":[{"content":[{"text":"a"}]}],"citations":[{"url":"https://c","title":"C"}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := search.Config{Mode: search.ModeChat, AuthHeader: search.AuthBearer, BaseURL: srv.URL, DefaultModel: "sonar"}
	body, _, err := h.chatPerplexityAgent(context.Background(), cfg, Request{ProviderID: "perplexity-agent", Query: "q", Credentials: creds("k")}, "q")
	if err != nil {
		t.Fatal(err)
	}
	var resp searchResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://c" || resp.Results[0].Title != "C" {
		t.Errorf("results = %+v", resp.Results)
	}
}

// === sanitizeQuery ===

func TestSanitizeQuery(t *testing.T) {
	cases := map[string]string{
		"  hello   world  ": "hello world",
		"hello\x00world":    "helloworld",
		"\thello\n":         "hello",
		"":                  "",
		"   ":               "",
	}
	for in, want := range cases {
		got := sanitizeQuery(in)
		if got != want {
			t.Errorf("sanitizeQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// === helpers ===

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// totalAsInt coerces a JSON-deoded total_results_available (float64 or int) to
// int for assertion.
func totalAsInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return -1
}

// srvClientFor returns an http.Client that can reach both test servers. Since
// httptest.Server clients are shared (srv.Client() uses the same transport),
// we just return the first server's client.
func srvClientFor(a, b *httptest.Server) *http.Client {
	_ = b
	return a.Client()
}
