// Package proxyfetch implements the /v1/web/fetch upstream pipeline for the Go
// rewrite. It ports open-sse/handlers/fetch/index.js + src/sse/handlers/fetch.js:
// resolve the provider adapter (provider IS the model), build the upstream
// request, fetch, normalize into the JS buildData shape, and write the JSON
// body. No usage rows are persisted — the legacy JS fetch path does not call
// saveRequestUsage (it only returns fetch_cost_usd in-band).
//
// NOT in this package (separate slices, mirroring the embeddings port scope):
// account fallback (the JS loop over excludeConnectionIds), on-401 token
// refresh, and combo expansion. SSRF guarding of the target URL is the
// handler's responsibility (it validates + assertsPublicUrl before dispatch),
// matching the JS route boundary in src/sse/handlers/fetch.js.
package proxyfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/webfetch"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Request is the input to Handle.
type Request struct {
	Ctx          context.Context
	ProviderID   string
	Credentials  domainProv.Credentials
	APIKey       string
	ConnectionID string
	Endpoint     string
	UserAgent    string
	// Params carries the parsed /v1/web/fetch body fields.
	Params webfetch.Params
}

// Result is the output of Handle.
type Result struct {
	StatusCode int
	Err        error
	// Body is the normalized JSON body to write to the client when 2xx.
	Body []byte
}

// Dependencies collects the collaborators consumed by the usecase.
type Dependencies struct {
	// LookupAdapter resolves the per-provider web-fetch adapter. Defaults to
	// webfetch.Lookup when nil.
	LookupAdapter func(providerID string) (webfetch.Adapter, bool)
	// HTTPClient is the upstream HTTP client. Defaults to a standard client
	// with the per-call timeout (15s, matching JS DEFAULT_TIMEOUT_MS) when nil.
	HTTPClient *http.Client
	// Logger is a minimal log sink. May be nil (no-op).
	Logger Logger
	// Config carries the timeout settings (currently unused — fetch uses the
	// JS DEFAULT_TIMEOUT_MS, not the chat body timeout).
	Config config.Config
}

// Logger is a minimal log sink matching proxychat's shape.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

// Handler is the compiled web-fetch usecase.
type Handler struct {
	deps Dependencies
}

// New creates a Handler. Nil collaborators select sensible defaults.
func New(deps Dependencies) *Handler {
	if deps.LookupAdapter == nil {
		deps.LookupAdapter = webfetch.Lookup
	}
	if deps.HTTPClient == nil {
		deps.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if deps.Logger == nil {
		deps.Logger = noopLogger{}
	}
	return &Handler{deps: deps}
}

type noopLogger struct{}

func (noopLogger) Infof(string, ...any)  {}
func (noopLogger) Warnf(string, ...any)  {}
func (noopLogger) Debugf(string, ...any) {}

// Handle runs the /v1/web/fetch pipeline.
func (h *Handler) Handle(ctx context.Context, req Request) Result {
	start := time.Now()

	adapter, ok := h.deps.LookupAdapter(req.ProviderID)
	if !ok {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("provider %q does not support web fetch", req.ProviderID)}
	}

	// Per-call timeout mirroring JS DEFAULT_TIMEOUT_MS / providerConfig.timeoutMs.
	callTimeout := 15 * time.Second
	if req.Params.ProviderConfig != nil {
		if v, ok := req.Params.ProviderConfig["timeoutMs"].(float64); ok && v > 0 {
			callTimeout = time.Duration(v) * time.Millisecond
		}
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	client := h.deps.HTTPClient
	// Clone with the per-call timeout so a slow upstream cannot clamp the
	// shared client's longer timeout.
	perCall := *client
	perCall.Timeout = callTimeout

	res, err := adapter.Fetch(cctx, &perCall, req.Credentials, req.Params, slogAdapter{h.deps.Logger})
	upstreamMs := time.Since(start).Milliseconds()
	responseMs := upstreamMs
	if err != nil {
		status := http.StatusBadGateway
		if cctx.Err() != nil {
			status = http.StatusGatewayTimeout
		}
		return Result{StatusCode: status, Err: fmt.Errorf("upstream error: %w", err)}
	}

	out := buildResponseJSON(res, responseMs, upstreamMs)
	body, err := json.Marshal(out)
	if err != nil {
		return Result{StatusCode: http.StatusInternalServerError, Err: fmt.Errorf("encode response: %w", err)}
	}
	return Result{StatusCode: http.StatusOK, Body: body}
}

// buildResponseJSON mirrors the JS buildData({ provider, url, title, format,
// text, costUsd, responseMs, upstreamMs }) output shape.
func buildResponseJSON(res *webfetch.Result, responseMs, upstreamMs int64) map[string]any {
	text := res.Text
	return map[string]any{
		"provider": res.Provider,
		"url":      res.URL,
		"title":    nullableString(res.Title),
		"content": map[string]any{
			"format": res.Format,
			"text":   text,
			"length": len(text),
		},
		"metadata": map[string]any{
			"author":       nil,
			"published_at": nil,
			"language":     nil,
		},
		"usage": map[string]any{
			"fetch_cost_usd": nullableString(res.CostUSD),
		},
		"metrics": map[string]any{
			"response_time_ms":   responseMs,
			"upstream_latency_ms": upstreamMs,
		},
	}
}

// nullableString returns nil for an empty string so the JSON encodes null
// (matching the JS `title || null` / `costUsd ?? null` idioms).
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// slogAdapter bridges the usecase Logger to the webfetch.Logger (Warnf/Debugf).
type slogAdapter struct{ log Logger }

func (a slogAdapter) Warnf(format string, args ...any)  { a.log.Warnf(format, args...) }
func (a slogAdapter) Debugf(format string, args ...any) { a.log.Debugf(format, args...) }