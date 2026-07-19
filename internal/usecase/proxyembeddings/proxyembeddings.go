// Package proxyembeddings implements the /v1/embeddings upstream pipeline for
// the Go rewrite. It ports open-sse/handlers/embeddingsCore.js: build the
// upstream URL/headers/body via a per-provider embedding adapter, POST through
// the proxy-aware fetch stack, classify the upstream status, normalize the
// response into the OpenAI embeddings shape, write the JSON body, and record
// usage (prompt tokens only — embeddings have no completion tokens).
//
// NOT in this package (separate slices): account fallback (the JS handler
// loop over excludeConnectionIds) and on-401 token refresh. These will reuse
// the same proxy fetch path once the shared account-fallback service lands.
package proxyembeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/embedding"
	"github.com/Artiffusion-Inc/9router/internal/adapter/transport/proxy"
	domainProv "github.com/Artiffusion-Inc/9router/internal/domain/provider"
	"github.com/Artiffusion-Inc/9router/internal/domain/usage"
)

// Request is the input to Handle.
type Request struct {
	Ctx         context.Context
	Body        json.RawMessage
	Endpoint    string
	Headers     http.Header
	ProviderID  string
	Model       string
	Credentials domainProv.Credentials
	APIKey      string
	ConnectionID string
	UserAgent   string
}

// Result is the output of Handle.
type Result struct {
	StatusCode int
	Err        error
	// Body is the normalized OpenAI-shaped response body to write to the
	// client when StatusCode is 2xx. Empty on error.
	Body []byte
}

// Dependencies collects the collaborators consumed by the usecase.
type Dependencies struct {
	// LookupAdapter resolves the per-provider embedding adapter. Defaults to
	// embedding.Lookup when nil.
	LookupAdapter func(providerID string) (embedding.Adapter, bool)
	// Fetch executes the upstream request through the proxy-aware stack.
	// Defaults to proxy.ProxyAwareFetch when nil.
	Fetch func(ctx context.Context, client *http.Client, req *http.Request, opts proxy.Options, proxyOpts proxy.ProxyFetchOptions, fallback *proxy.Fallback) (*http.Response, error)
	// HTTPClient is the upstream HTTP client. Defaults to a standard client
	// with the configured body timeout when nil.
	HTTPClient *http.Client
	// ProxyOpts is the application-level proxy options resolved from config.
	ProxyOpts proxy.Options
	// Fallback is the proxy fallback (account pool) helper. May be nil —
	// ProxyAwareFetch tolerates a nil fallback for non-proxy routes.
	Fallback *proxy.Fallback
	// UsageRepo records usage. May be nil (no-op).
	UsageRepo usage.Repo
	// Logger is a minimal log sink. May be nil (no-op).
	Logger Logger
	// Config carries the timeout settings.
	Config config.Config
}

// Logger is a minimal log sink matching proxychat's shape so wire.go can
// reuse its slog adapter.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

// Handler is the compiled embeddings usecase.
type Handler struct {
	deps Dependencies
}

// New creates a Handler. Nil collaborators select sensible defaults.
func New(deps Dependencies) *Handler {
	if deps.LookupAdapter == nil {
		deps.LookupAdapter = embedding.Lookup
	}
	if deps.Fetch == nil {
		deps.Fetch = proxy.ProxyAwareFetch
	}
	if deps.HTTPClient == nil {
		timeout := deps.Config.FetchBodyTimeout.Duration()
		if timeout <= 0 {
			timeout = 10 * time.Minute
		}
		deps.HTTPClient = &http.Client{Timeout: timeout}
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

// Handle runs the /v1/embeddings pipeline.
func (h *Handler) Handle(ctx context.Context, req Request) Result {
	start := time.Now()

	adapter, ok := h.deps.LookupAdapter(req.ProviderID)
	if !ok {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("provider %q does not support embeddings", req.ProviderID)}
	}

	var parsed struct {
		Input          any `json:"input"`
		EncodingFormat any `json:"encoding_format"`
		Dimensions     any `json:"dimensions"`
	}
	if err := json.Unmarshal(req.Body, &parsed); err != nil {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("invalid request body: %w", err)}
	}
	if parsed.Input == nil {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("missing required field: input")}
	}
	params := embedding.Params{
		Input:          parsed.Input,
		EncodingFormat: toString(parsed.EncodingFormat),
		Dimensions:     parsed.Dimensions,
	}

	upURL := adapter.BuildURL(req.Model, req.Credentials, params)
	upBody, err := adapter.BuildBody(req.Model, params)
	if err != nil {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("build upstream body: %w", err)}
	}
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upURL, bytes.NewReader(upBody))
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: fmt.Errorf("build upstream request: %w", err)}
	}
	for k, vv := range adapter.BuildHeaders(req.Credentials, params) {
		for _, v := range vv {
			upReq.Header.Add(k, v)
		}
	}
	if req.UserAgent != "" && upReq.Header.Get("User-Agent") == "" {
		upReq.Header.Set("User-Agent", req.UserAgent)
	}

	proxyOpts := base.ProxyFetchOptsFromCreds(req.Credentials, proxy.ProxyFetchOptions{})
	resp, err := h.deps.Fetch(ctx, h.deps.HTTPClient, upReq, h.deps.ProxyOpts, proxyOpts, h.deps.Fallback)
	if err != nil {
		h.saveUsage(ctx, req, start, 0, "error")
		status := http.StatusBadGateway
		if ctx.Err() != nil {
			status = 499
		}
		return Result{StatusCode: status, Err: fmt.Errorf("upstream error: %w", err)}
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		h.saveUsage(ctx, req, start, 0, "error")
		return Result{StatusCode: http.StatusBadGateway, Err: fmt.Errorf("read upstream body: %w", err)}
	}

	if resp.StatusCode/100 != 2 {
		h.saveUsage(ctx, req, start, 0, fmt.Sprintf("failed %d", resp.StatusCode))
		return Result{
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("upstream returned %d: %s", resp.StatusCode, truncate(bodyBytes, 512)),
		}
	}

	normalized, err := adapter.Normalize(bodyBytes, req.Model)
	if err != nil {
		h.saveUsage(ctx, req, start, 0, "error")
		return Result{StatusCode: http.StatusBadGateway, Err: fmt.Errorf("normalize upstream response: %w", err)}
	}

	promptTokens := extractPromptTokens(normalized)
	h.saveUsage(ctx, req, start, promptTokens, "success")
	return Result{StatusCode: http.StatusOK, Body: normalized}
}

func (h *Handler) saveUsage(ctx context.Context, req Request, start time.Time, promptTokens int, status string) {
	if h.deps.UsageRepo == nil {
		return
	}
	rec := usage.UsageRecord{
		Timestamp:    start,
		Provider:     req.ProviderID,
		Model:        req.Model,
		ConnectionID: req.ConnectionID,
		APIKey:       req.APIKey,
		Endpoint:     req.Endpoint,
		PromptTokens: promptTokens,
		Status:       status,
	}
	_ = h.deps.UsageRepo.Save(ctx, rec)
}

// extractPromptTokens reads usage.prompt_tokens (or total_tokens) from the
// normalized OpenAI-shaped response body.
func extractPromptTokens(body []byte) int {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return 0
	}
	u, ok := m["usage"].(map[string]any)
	if !ok {
		return 0
	}
	for _, k := range []string{"prompt_tokens", "total_tokens", "input_tokens"} {
		if v, ok := u[k].(float64); ok {
			return int(v)
		}
	}
	return 0
}

// toString coerces a JSON-decoded value to its string form, returning "" for
// nil. JSON numbers arrive as float64; the encoding_format field is a string
// in the spec, but be defensive about numeric/string forms.
func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return fmt.Sprintf("%v", x)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}