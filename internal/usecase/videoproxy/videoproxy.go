// Package videoproxy implements the /v1/videos/* surface for the Go rewrite.
// It ports open-sse/handlers/videoCore.js (handleVideoProxyCore) +
// src/sse/handlers/videoGeneration.js. The Go MVP keeps the JS shape: a raw
// byte passthrough to the xAI video upstream (the only provider with a
// videoConfig on legacy), with submit (POST /videos/{generations|edits|extensions})
// and poll (GET /videos/{id}) actions, transparent Content-Type forwarding,
// an Idempotency-Key pass-through on POST, and a single on-401/403 token
// refresh retry.
//
// NOT in this MVP slice (separate slices, mirroring the embeddings/fetch scope):
// the multi-account fallback rotation loop (markAccountUnavailable +
// shouldFallback + excludeConnectionIds), usage persistence (the legacy video
// path never persisted usage), and combo expansion. SSRF is N/A — the target is
// the fixed xAI upstream, not a client-supplied URL.
package videoproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Action is the video operation kind.
type Action string

const (
	ActionGenerations Action = "generations"
	ActionEdits       Action = "edits"
	ActionExtensions  Action = "extensions"
)

// Valid reports whether a is one of the supported actions.
func (a Action) Valid() bool {
	switch a {
	case ActionGenerations, ActionEdits, ActionExtensions:
		return true
	}
	return false
}

// Request is the input to Handle (a raw passthrough call).
type Request struct {
	Ctx            context.Context
	Action         Action // empty for GET poll
	RequestID      string // set for GET poll; empty for POST submit
	Body           []byte // raw upstream body bytes (POST only)
	ContentType    string // client Content-Type (POST only)
	IdempotencyKey string // Idempotency-Key header (POST only, optional)
	Credentials    domainProv.Credentials
	ProviderID     string
	Model          string
	ConnectionID   string
	UserAgent      string
}

// Result is the output of Handle.
type Result struct {
	StatusCode   int
	Err          error
	Body         []byte
	ContentType  string
	ConnectionID string
}

// Dependencies collects the collaborators.
type Dependencies struct {
	// VideoBaseURL returns the upstream base for the provider's videoConfig.
	// Defaults to the xAI URL when nil.
	VideoBaseURL func(providerID string) string
	// HTTPClient is the upstream client. Defaults to a client with the
	// configured round-trip timeout when nil.
	HTTPClient *http.Client
	// RefreshCredentials is called once on a 401/403 upstream to refresh the
	// OAuth token (for token-based providers). Returns the refreshed
	// credentials and whether a refresh actually happened. May be nil → no
	// refresh retry (mirrors apikey accounts where refresh is impossible).
	RefreshCredentials func(ctx context.Context, providerID string, creds domainProv.Credentials) (domainProv.Credentials, bool, error)
	// Logger is a minimal log sink. May be nil (no-op).
	Logger Logger
	// Config carries the timeout settings.
	Config config.Config
}

// Logger is a minimal log sink matching proxychat's shape.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

// Handler is the compiled video-proxy usecase.
type Handler struct {
	deps Dependencies
}

// New creates a Handler. Nil collaborators select sensible defaults.
func New(deps Dependencies) *Handler {
	if deps.VideoBaseURL == nil {
		deps.VideoBaseURL = defaultVideoBaseURL
	}
	if deps.HTTPClient == nil {
		deps.HTTPClient = &http.Client{Timeout: 120 * time.Second}
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

// defaultVideoBaseURL mirrors the JS DEFAULT_VIDEO_PROVIDER + xAI videoConfig.
// Only xAI has a videoConfig on legacy; other providers 400 at the handler.
func defaultVideoBaseURL(providerID string) string {
	if providerID == "xai" {
		return "https://api.x.ai/v1/videos"
	}
	return ""
}

// Handle runs a single raw passthrough to the video upstream.
func (h *Handler) Handle(ctx context.Context, req Request) Result {
	base := h.deps.VideoBaseURL(req.ProviderID)
	if base == "" {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("provider %q does not support video generation", req.ProviderID)}
	}
	base = strings.TrimSuffix(base, "/")

	var upstreamURL string
	method := http.MethodPost
	if req.RequestID != "" {
		upstreamURL = base + "/" + req.RequestID
		method = http.MethodGet
	} else if req.Action.Valid() {
		upstreamURL = base + "/" + string(req.Action)
	} else {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("invalid video action: %q", req.Action)}
	}

	res, err := h.doUpstream(ctx, method, upstreamURL, req, req.Credentials)
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: fmt.Errorf("upstream error: %w", err)}
	}
	// On 401/403, try a single credential refresh and retry once.
	if (res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden) && h.deps.RefreshCredentials != nil {
		refreshed, ok, rerr := h.deps.RefreshCredentials(ctx, req.ProviderID, req.Credentials)
		if rerr == nil && ok {
			// Drain and discard the first response body before retrying.
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			r2, rerr := h.doUpstream(ctx, method, upstreamURL, req, refreshed)
			if rerr != nil {
				return Result{StatusCode: http.StatusBadGateway, Err: fmt.Errorf("upstream retry error: %w", rerr)}
			}
			res = r2
		}
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: fmt.Errorf("read upstream: %w", err)}
	}
	ct := res.Header.Get("Content-Type")
	return Result{StatusCode: res.StatusCode, Body: b, ContentType: ct, ConnectionID: req.ConnectionID}
}

func (h *Handler) doUpstream(ctx context.Context, method, url string, req Request, creds domainProv.Credentials) (*http.Response, error) {
	var bodyReader io.Reader
	if method == http.MethodPost && len(req.Body) > 0 {
		bodyReader = strings.NewReader(string(req.Body))
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	if method == http.MethodPost {
		if req.ContentType != "" {
			httpReq.Header.Set("Content-Type", req.ContentType)
		}
		if req.IdempotencyKey != "" {
			httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)
		}
	}
	httpReq.Header.Set("Accept", "application/json")
	if tok := credentialToken(creds); tok != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	return h.deps.HTTPClient.Do(httpReq)
}

// credentialToken extracts the bearer token (accessToken or apiKey), mirroring
// the JS `accessToken || apiKey` precedence.
func credentialToken(creds domainProv.Credentials) string {
	if creds.AccessToken != "" {
		return creds.AccessToken
	}
	return creds.APIKey
}
