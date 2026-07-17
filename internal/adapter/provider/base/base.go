// Package base ports the BaseExecutor from open-sse/executors/base.js.
// It holds the generic upstream build/execute pipeline used by all providers.
package base

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	"github.com/Artiffusion-Inc/9router/internal/adapter/transport/proxy"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// Shared defaults from open-sse/providers/shared.js.
const (
	AnthropicAPIVersion = "2023-06-01"
	OpenAICompatBase    = "https://api.openai.com/v1"
	AnthropicCompatBase = "https://api.anthropic.com/v1"
)

// HTTPStatus mirrors open-sse/config/runtimeConfig.js HTTP_STATUS.
const (
	HTTPStatusBadRequest     = 400
	HTTPStatusUnauthorized   = 401
	HTTPStatusPaymentRequired= 402
	HTTPStatusForbidden      = 403
	HTTPStatusNotFound       = 404
	HTTPStatusNotAcceptable  = 406
	HTTPStatusRequestTimeout = 408
	HTTPStatusRateLimited    = 429
	HTTPStatusServerError    = 500
	HTTPStatusBadGateway     = 502
	HTTPStatusServiceUnavailable = 503
	HTTPStatusGatewayTimeout = 504
)

// RetryEntry mirrors a retry config entry.
type RetryEntry struct {
	Attempts int
	DelayMs  int
}

// DefaultRetryConfig mirrors open-sse/config/runtimeConfig.js DEFAULT_RETRY_CONFIG.
var DefaultRetryConfig = map[int]RetryEntry{
	HTTPStatusRateLimited:    {Attempts: 0, DelayMs: 0},
	HTTPStatusBadGateway:     {Attempts: 3, DelayMs: 3000},
	HTTPStatusServiceUnavailable: {Attempts: 3, DelayMs: 2000},
	HTTPStatusGatewayTimeout: {Attempts: 2, DelayMs: 3000},
}

// ResolveRetryEntry normalizes a retry entry, matching the JS helper.
func ResolveRetryEntry(entry any) RetryEntry {
	if entry == nil {
		return RetryEntry{Attempts: 0, DelayMs: 2000}
	}
	switch e := entry.(type) {
	case int:
		return RetryEntry{Attempts: e, DelayMs: 2000}
	case int64:
		return RetryEntry{Attempts: int(e), DelayMs: 2000}
	case float64:
		return RetryEntry{Attempts: int(e), DelayMs: 2000}
	case RetryEntry:
		return e
	case map[string]any:
		var out RetryEntry
		if v, ok := e["attempts"]; ok {
			out.Attempts = int(Number(v))
		}
		if v, ok := e["delayMs"]; ok {
			out.DelayMs = int(Number(v))
		} else {
			out.DelayMs = 2000
		}
		return out
	}
	return RetryEntry{Attempts: 0, DelayMs: 2000}
}

// Number coerces a value to int.
func Number(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		i, _ := x.Int64()
		return int(i)
	case string:
		var i int
		fmt.Sscanf(x, "%d", &i)
		return i
	}
	return 0
}

// Config is the per-provider configuration subset needed by executors.
// It mirrors fields from the JS registry transport object.
type Config struct {
	ID              string
	BaseURL         string
	BaseURLs        []string
	Format          string
	URLSuffix       string
	Headers         map[string]string
	NoAuth          bool
	Auth            AuthDescriptor
	Retry           map[int]RetryEntry
	TimeoutMs       int
	Quirks          Quirks
	ReasoningInject *ReasoningInject
	RuntimeTransports []RuntimeTransport
}

// RuntimeTransport mirrors credentials.runtimeTransport.
type RuntimeTransport struct {
	BaseURL   string
	URLSuffix string
	Headers   map[string]string
	Auth      AuthDescriptor
}

// AuthDescriptor describes how to set auth headers.
type AuthDescriptor struct {
	Combined bool
	Header   string
	Scheme   string
	APIKey   *AuthSpec
	OAuth    *AuthSpec
	Hooks    []string
	AnthropicVersion bool
}

// AuthSpec is one auth branch.
type AuthSpec struct {
	Header string
	Scheme string
}

// ReasoningInject mirrors transport.reasoningInject.
type ReasoningInject struct {
	Scope string
}

// Quirks mirrors transport.quirks.
type Quirks struct {
	DropClientMetadata bool
	PreserveCacheControl bool
	DropOutputConfig   bool
}

// Fetcher performs the upstream HTTP request.
type Fetcher func(ctx context.Context, client *http.Client, req *http.Request, opts proxy.Options, proxyOpts proxy.ProxyFetchOptions, fallback *proxy.Fallback) (*http.Response, error)

// BaseExecutor is the generic provider executor.
type BaseExecutor struct {
	Provider string
	Config   Config
	NoAuth   bool
	Fetch    Fetcher
	HTTPClient *http.Client
	ProxyOpts  proxy.Options
	ProxyFetchOpts proxy.ProxyFetchOptions
	Fallback *proxy.Fallback
}

// NewBaseExecutor creates a base executor from config.
func NewBaseExecutor(provider string, cfg Config) *BaseExecutor {
	return &BaseExecutor{
		Provider: provider,
		Config:   cfg,
		NoAuth:   cfg.NoAuth,
		Fetch:    proxy.ProxyAwareFetch,
		HTTPClient: http.DefaultClient,
	}
}

// SetProxyOptions wires proxy options from application config.
func (e *BaseExecutor) SetProxyOptions(opts proxy.Options) {
	e.ProxyOpts = opts
}

// GetProvider returns the provider id.
func (e *BaseExecutor) GetProvider() string { return e.Provider }

// GetBaseUrls returns fallback URLs.
func (e *BaseExecutor) GetBaseUrls() []string {
	if len(e.Config.BaseURLs) > 0 {
		return e.Config.BaseURLs
	}
	if e.Config.BaseURL != "" {
		return []string{e.Config.BaseURL}
	}
	return nil
}

// GetFallbackCount returns the number of fallback attempts.
func (e *BaseExecutor) GetFallbackCount() int {
	if n := len(e.GetBaseUrls()); n > 0 {
		return n
	}
	return 1
}

// BuildURL builds the upstream URL. It is exported so it matches the interface.
func (e *BaseExecutor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	if e.Provider != "" && strings.HasPrefix(e.Provider, "openai-compatible-") {
		baseURL := OpenAICompatBase
		if v, ok := creds.ProviderSpecificData["baseUrl"].(string); ok && v != "" {
			baseURL = v
		}
		normalized := strings.TrimSuffix(baseURL, "/")
		path := "/chat/completions"
		if strings.Contains(e.Provider, "responses") {
			path = "/responses"
		}
		return normalized + path
	}
	if e.Provider != "" && strings.HasPrefix(e.Provider, "anthropic-compatible-") {
		baseURL := AnthropicCompatBase
		if v, ok := creds.ProviderSpecificData["baseUrl"].(string); ok && v != "" {
			baseURL = v
		}
		normalized := strings.TrimSuffix(baseURL, "/")
		return normalized + "/messages"
	}

	// Runtime transport override.
	if rt := e.resolveRuntimeTransport(creds); rt != nil && rt.BaseURL != "" {
		if rt.URLSuffix != "" {
			return rt.BaseURL + rt.URLSuffix
		}
		return rt.BaseURL
	}

	baseURLs := e.GetBaseUrls()
	url := ""
	if urlIndex >= 0 && urlIndex < len(baseURLs) {
		url = baseURLs[urlIndex]
	} else if len(baseURLs) > 0 {
		url = baseURLs[0]
	} else {
		url = e.Config.BaseURL
	}

	if e.Config.URLSuffix != "" {
		return url + e.Config.URLSuffix
	}
	if strings.Contains(url, "{accountId}") {
		accountID, _ := creds.ProviderSpecificData["accountId"].(string)
		if accountID == "" {
			panic(fmt.Sprintf("%s requires accountId in providerSpecificData", e.Provider))
		}
		url = strings.ReplaceAll(url, "{accountId}", accountID)
	}
	return url
}

func (e *BaseExecutor) resolveRuntimeTransport(creds provider.Credentials) *RuntimeTransport {
	v, ok := creds.ProviderSpecificData["runtimeTransport"]
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	rt := &RuntimeTransport{}
	if s, ok := m["baseUrl"].(string); ok {
		rt.BaseURL = s
	}
	if s, ok := m["urlSuffix"].(string); ok {
		rt.URLSuffix = s
	}
	if h, ok := m["headers"].(map[string]any); ok {
		rt.Headers = make(map[string]string, len(h))
		for k, v2 := range h {
			if s2, ok := v2.(string); ok {
				rt.Headers[k] = s2
			}
		}
	}
	if a, ok := m["auth"].(map[string]any); ok {
		rt.Auth = mapAuthDescriptor(a)
	}
	return rt
}

func mapAuthDescriptor(a map[string]any) AuthDescriptor {
	var d AuthDescriptor
	if v, ok := a["combined"].(bool); ok {
		d.Combined = v
	}
	if v, ok := a["header"].(string); ok {
		d.Header = v
	}
	if v, ok := a["scheme"].(string); ok {
		d.Scheme = v
	}
	if v, ok := a["anthropicVersion"].(bool); ok {
		d.AnthropicVersion = v
	}
	return d
}

// BuildHeaders builds upstream headers.
func (e *BaseExecutor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")

	// Config headers first (lowercased keys are canonicalized by http.Header).
	for k, v := range e.Config.Headers {
		h.Set(k, v)
	}

	desc := e.Config.Auth
	if desc.Header == "" && desc.APIKey == nil && desc.OAuth == nil {
		desc = e.resolveAuthDescriptor()
	}

	for _, hook := range desc.Hooks {
		if fn := e.HeaderHook(hook); fn != nil {
			fn(h, creds)
		}
	}

	e.applyAuth(h, desc, creds)

	if stream {
		h.Set("Accept", "text/event-stream")
	}
	return h
}

// ResolveAuthDescriptor returns a fallback auth descriptor.
func (e *BaseExecutor) resolveAuthDescriptor() AuthDescriptor {
	if e.Provider != "" && strings.HasPrefix(e.Provider, "anthropic-compatible-") {
		return AuthDescriptor{
			APIKey: &AuthSpec{Header: "x-api-key", Scheme: "raw"},
			OAuth:  &AuthSpec{Header: "Authorization", Scheme: "bearer"},
			AnthropicVersion: true,
		}
	}
	if e.Config.Format == "claude" {
		return AuthDescriptor{
			Combined: true,
			Header:   "x-api-key",
			Scheme:   "raw",
			AnthropicVersion: true,
		}
	}
	return AuthDescriptor{Combined: true, Header: "Authorization", Scheme: "bearer"}
}

func (e *BaseExecutor) applyAuth(h http.Header, desc AuthDescriptor, creds provider.Credentials) {
	if desc.Combined {
		token := creds.APIKey
		if token == "" {
			token = creds.AccessToken
		}
		header := desc.Header
		scheme := desc.Scheme
		if header == "" {
			header = "Authorization"
			scheme = "bearer"
		}
		e.setAuthHeader(h, header, scheme, token)
		if desc.AnthropicVersion && h.Get("anthropic-version") == "" && h.Get("Anthropic-Version") == "" {
			h.Set("anthropic-version", AnthropicAPIVersion)
		}
		return
	}
	if creds.APIKey != "" && desc.APIKey != nil {
		e.setAuthHeader(h, desc.APIKey.Header, desc.APIKey.Scheme, creds.APIKey)
	} else if creds.AccessToken != "" && desc.OAuth != nil {
		e.setAuthHeader(h, desc.OAuth.Header, desc.OAuth.Scheme, creds.AccessToken)
	}
	if desc.AnthropicVersion && h.Get("anthropic-version") == "" && h.Get("Anthropic-Version") == "" {
		h.Set("anthropic-version", AnthropicAPIVersion)
	}
}

func (e *BaseExecutor) setAuthHeader(h http.Header, header, scheme, token string) {
	if scheme == "bearer" {
		h.Set(header, "Bearer "+token)
	} else {
		h.Set(header, token)
	}
}

// HeaderHook returns a hook function by name, or nil.
func (e *BaseExecutor) HeaderHook(name string) func(http.Header, provider.Credentials) {
	return nil
}

// TransformRequest is the generic passthrough; default executor overrides it.
func (e *BaseExecutor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	return body, nil
}

// ShouldRetry decides whether to fall back to the next URL.
func (e *BaseExecutor) ShouldRetry(status, urlIndex int) bool {
	return status == HTTPStatusRateLimited && urlIndex+1 < e.GetFallbackCount()
}

// ComputeRetryDelay hook for subclass-derived dynamic delays.
type ComputeRetryDelayFunc func(response *http.Response, attempt int, delayMs int) (int, bool, error)

var _ ComputeRetryDelayFunc = nil

// Execute performs the upstream request with retry + fallback.
func (e *BaseExecutor) Execute(ctx context.Context, req provider.ExecRequest) (provider.Resp, error) {
	fallbackCount := e.GetFallbackCount()
	var lastError error
	retryAttemptsByURL := make(map[int]int)

	retryConfig := make(map[int]RetryEntry)
	for k, v := range DefaultRetryConfig {
		retryConfig[k] = v
	}
	for k, v := range e.Config.Retry {
		retryConfig[k] = v
	}

	tryRetry := func(urlIndex int, statusKey int, reason string, response *http.Response) (bool, error) {
		entry := ResolveRetryEntry(retryConfig[statusKey])
		if entry.Attempts <= 0 {
			return false, nil
		}
		if retryAttemptsByURL[urlIndex] >= entry.Attempts {
			return false, nil
		}
		waitMs := entry.DelayMs
		if response != nil {
			waitMs = e.computeDynamicRetryDelay(response, retryAttemptsByURL[urlIndex]+1, entry.DelayMs)
		}
		retryAttemptsByURL[urlIndex]++
		select {
		case <-time.After(time.Duration(waitMs) * time.Millisecond):
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}

	for urlIndex := 0; urlIndex < fallbackCount; urlIndex++ {
		url := e.BuildURL(req.Model, req.Stream, urlIndex, req.Credentials)
		transformedBody, err := e.TransformRequest(req.Model, req.Body, req.Stream, req.Credentials)
		if err != nil {
			return provider.Resp{}, err
		}
		headers := e.BuildHeaders(req.Credentials, req.Stream)

		if _, ok := retryAttemptsByURL[urlIndex]; !ok {
			retryAttemptsByURL[urlIndex] = 0
		}

		timeoutMs := e.Config.TimeoutMs
		if timeoutMs <= 0 {
			timeoutMs = int(config.Config{}.FetchConnectTimeout.Duration().Milliseconds())
		}

		bodyStr := string(transformedBody)
		if transformedBody == nil {
			bodyStr = ""
		}

		upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(bodyStr))
		if err != nil {
			return provider.Resp{}, err
		}
		for k, vv := range headers {
			for _, v := range vv {
				upReq.Header.Add(k, v)
			}
		}

		resp, err := e.doFetch(ctx, upReq)
		if err != nil {
			lastError = err
			// Map network/fetch exceptions to 502 retry config.
			if shouldRetry, rerr := tryRetry(urlIndex, HTTPStatusBadGateway, fmt.Sprintf("network %q", err.Error()), nil); shouldRetry {
				if rerr != nil {
					return provider.Resp{}, rerr
				}
				urlIndex--
				continue
			}
			if urlIndex+1 < fallbackCount {
				continue
			}
			return provider.Resp{}, err
		}

		if shouldRetry, rerr := tryRetry(urlIndex, resp.StatusCode, fmt.Sprintf("status %d", resp.StatusCode), resp); shouldRetry {
			if rerr != nil {
				resp.Body.Close()
				return provider.Resp{}, rerr
			}
			resp.Body.Close()
			urlIndex--
			continue
		}

		if e.ShouldRetry(resp.StatusCode, urlIndex) {
			resp.Body.Close()
			continue
		}

		return provider.Resp{
			Response:        resp,
			URL:             url,
			Headers:         headers,
			TransformedBody: transformedBody,
		}, nil
	}

	return provider.Resp{}, lastError
}

func (e *BaseExecutor) doFetch(ctx context.Context, req *http.Request) (*http.Response, error) {
	// Use a context with timeout for connect timeout if possible.
	timeoutMs := e.Config.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 120000
	}
	fetchCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	req = req.Clone(fetchCtx)
	return e.Fetch(ctx, e.HTTPClient, req, e.ProxyOpts, e.ProxyFetchOpts, e.Fallback)
}

func (e *BaseExecutor) computeDynamicRetryDelay(response *http.Response, attempt, delayMs int) int {
	// Base does not implement dynamic delay; default returns configured delay.
	return delayMs
}

// DrainAndClose discards and closes a response body.
func DrainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// ReadBody reads the full response body and closes it.
func ReadBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, err := io.Copy(&buf, resp.Body)
	return buf.Bytes(), err
}

// Ensure BaseExecutor implements the interface.
var _ provider.Executor = (*BaseExecutor)(nil)
