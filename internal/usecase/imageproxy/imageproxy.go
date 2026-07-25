// Package imageproxy implements the /v1/images/generations pipeline for the Go
// rewrite. It ports src/sse/handlers/imageGeneration.js (handleImageGeneration)
// + open-sse/handlers/imageGenerationCore.js (adapter pattern) + the
// per-provider imageProviders adapters: generate images via the provider's
// static image config, normalize the upstream response into the OpenAI
// {created, data:[{url|b64_json}]} shape (or raw binary when
// response_format=binary).
//
// Supported in this slice:
//   - OpenAI-compatible (openai, minimax, openrouter, recraft, xai with
//     bodyFields whitelist, vercel-ai-gateway, venice) — passthrough OpenAI
//     shape.
//   - Gemini — generateContent with responseModalities ["TEXT","IMAGE"] →
//     candidates[].content.parts[].inlineData.data → {b64_json}.
//   - Codex — Responses API with tools:[{type:"image_generation",…}], SSE
//     parse → {created, data:[{b64_json}]}.
//   - Sync image providers (step 5): sdwebui (noAuth local /sdapi/v1/txt2img),
//     comfyui (noAuth local passthrough), huggingface (raw binary → b64_json),
//     stability-ai (core/ultra/sd3 segment, image b64_json).
//
// Deferred (501): fal-ai / black-forest-labs / runwayml / nanobanana (async
// polling), cloudflare-ai (JSON/multipart), antigravity (executor). The handler
// resolves the provider from body.model (provider/model prefix or bare → openai
// fallback).
//
// NOT in this slice (separate slices): combo expansion, account-fallback
// rotation, on-401 token refresh, usage persistence, x-9gouter-connection-id
// forwarding (the JS handler does echo it for pinning; deferred here).
package imageproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/image"
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

// HTTPExecutor is the outbound HTTP boundary for image generation. The usecase
// never imports the DB, proxy, or connection packages — it hands each upstream
// request to an executor with the transport metadata (provider, credentials,
// connection, lifecycle phase) attached to the request context. The production
// executor (wired in app/wire.go) resolves connection proxy settings, honours a
// proxy.ValidatedTarget for untrusted image URLs, and calls proxy.ProxyAwareFetch.
// Tests substitute a recording executor built over httptest.Server.
type HTTPExecutor interface {
	Do(req *http.Request) (*http.Response, error)
}

// TransportMetadata describes one outbound image lifecycle HTTP call. It is
// attached to the request context by the usecase before Executor.Do and read by
// the production executor (wire.go). It carries no DB/proxy types — only the
// primitive identifiers and the validated target the policy-aware proxy
// transport (step 1) needs.
type TransportMetadata struct {
	ProviderID    string
	ConnectionID  string
	Credentials   domainProv.Credentials
	Phase         string // "submit" | "poll" | "result" | "input" | "output"
	ValidatedHost ValidatedHost
}

// ValidatedHost is the untrusted-image egress contract handed to the
// policy-aware proxy transport. It mirrors proxy.ValidatedTarget but lives in
// the usecase package so imageproxy never imports the proxy package; the wire
// adapter translates it to proxy.ValidatedTarget and attaches it to the
// request context. Zero value (nil IP / empty port) means "no pinned target —
// use the standard proxy pipeline" (provider lifecycle requests that are
// operator-trusted hostnames).
type ValidatedHost struct {
	Scheme   string
	Hostname string
	Port     string
	IP       net.IP
}

// IsPinned reports whether the validated host carries a resolved IP to pin.
func (v ValidatedHost) IsPinned() bool { return v.IP != nil && v.Port != "" }

type transportMetaCtxKey struct{}

// WithTransportMetadata attaches the transport metadata to a request context so
// the production executor can read it. Tests use it to assert what the usecase
// passed to the executor.
func WithTransportMetadata(ctx context.Context, meta TransportMetadata) context.Context {
	return context.WithValue(ctx, transportMetaCtxKey{}, meta)
}

// TransportMetadataFromContext returns the metadata attached to the context, if
// any. Used by the production executor in wire.go.
func TransportMetadataFromContext(ctx context.Context) (TransportMetadata, bool) {
	m, ok := ctx.Value(transportMetaCtxKey{}).(TransportMetadata)
	return m, ok
}

// fallbackExecutor wraps an *http.Client as an HTTPExecutor. New uses it when
// no executor is injected (e.g. tests that pass an httptest.Server client). It
// never sees transport metadata — production wiring always injects a real
// executor.
type fallbackExecutor struct {
	client *http.Client
}

func (e *fallbackExecutor) Do(req *http.Request) (*http.Response, error) {
	return e.client.Do(req)
}

// Dependencies wires the imageproxy Handler.
type Dependencies struct {
	// Executor is the outbound HTTP boundary. If nil, New creates a fallback
	// executor over a plain *http.Client (300s body timeout); production wiring
	// injects the policy-aware proxy executor from app/wire.go.
	Executor HTTPExecutor
	Logger   Logger
	Config   config.Config
	// PollInterval is the delay between poll attempts. New sets the production
	// default 1500ms when zero; tests pass a short value to avoid sleeping.
	PollInterval time.Duration
	// PollTimeout is the overall polling deadline. New sets the production
	// default 120s when zero; tests pass a short value.
	PollTimeout time.Duration
	// Resolver resolves a hostname to IPs for the SSRF guard and ValidatedHost
	// construction (untrusted image input / binary download URLs). If nil, New
	// uses a no-op resolver that fails closed — the production wiring in
	// wire.go substitutes a net.LookupIP-based resolver. imageproxy never
	// performs real DNS itself.
	Resolver HostResolver
	// SSRFPolicy is the default-deny egress policy for untrusted image URLs.
	// If nil, New uses the production default-deny policy (rejects loopback,
	// private, link-local, CGNAT, multicast, metadata, .internal). Tests
	// inject a permissive policy so an httptest loopback endpoint can exercise
	// the download/redirect path — the production policy is never weakened.
	SSRFPolicy SSRFPolicy
}

// Handler runs the image-generation pipeline.
type Handler struct {
	deps Dependencies
}

// New constructs a Handler with sane defaults (300s body timeout fallback
// executor — image gen can be slow, especially Codex streaming). PollInterval
// defaults to 1500ms and PollTimeout to 120s (production parity values from
// the spec); tests pass shorter values.
func New(deps Dependencies) *Handler {
	if deps.Executor == nil {
		deps.Executor = &fallbackExecutor{client: &http.Client{Timeout: 300 * time.Second}}
	}
	if deps.Logger == nil {
		deps.Logger = noopLogger{}
	}
	if deps.PollInterval <= 0 {
		deps.PollInterval = 1500 * time.Millisecond
	}
	if deps.PollTimeout <= 0 {
		deps.PollTimeout = 120 * time.Second
	}
	if deps.Resolver == nil {
		deps.Resolver = noopResolver{}
	}
	if deps.SSRFPolicy == nil {
		deps.SSRFPolicy = defaultSSRFPolicy{}
	}
	return &Handler{deps: deps}
}

// Request is the input to Handle.
type Request struct {
	Ctx        context.Context
	ProviderID string
	Model      string
	Prompt     string
	N          int
	// NSupplied is true when the request body carried an explicit `n` key
	// (including n:0). The SDWebUI legacy decision table (step 5) distinguishes
	// absent n (→ batch_size:1) from explicit n:0 (→ batch_size:0); other
	// adapters ignore it.
	NSupplied             bool
	Size                  string
	Quality               string
	Style                 string
	ResponseFormat        string // "url" (default) | "b64_json" | "binary" (raw image bytes)
	OutputFormat          string // "png" (default) | "jpeg" | "webp" — used by codex + binary
	Background            string // codex
	Credentials           domainProv.Credentials
	UserAgent             string
	PreferredConnectionID string // x-9gouter-connection-id hint; "" → auto-resolve
	// Options carries provider-specific optional fields with presence semantics
	// (json.RawMessage so missing key ≠ supplied null/""/0). The HTTP handler
	// populates it after capability decision; the usecase does not enforce
	// provider policy (capability table lives in the handler, step 3).
	Options RequestOptions
}

// RequestOptions holds provider-specific optional image-generation inputs with
// presence-bearing types so the capability table (handler) can distinguish
// "absent" from "supplied null/empty/zero". The usecase forwards only the
// permitted, canonical fields to each provider adapter. Raw JSON is kept for
// fields whose provider wire shape is not yet fixed (cloudflare/async adapters,
// steps 6–7); the sync/OpenAI/Gemini/Codex paths ignore it for now.
type RequestOptions struct {
	// ImageInputs are the validated image inputs (data URL or HTTPS URL) for
	// img2img / inpainting. nil when no image was supplied. Populated by the safe
	// input resolver (step 4); until then the handler forwards the raw supplied
	// image values via RawImageInputs.
	ImageInputs []ImageInput
	// Mask is the validated mask input for inpainting. nil when not supplied.
	// Populated by the safe input resolver (step 4); until then the handler
	// forwards the canonical raw mask via RawMask.
	Mask *ImageInput
	// RawImageInputs carries the raw, presence-bearing image inputs (the
	// `image` and `images` JSON values) as supplied by the client, BEFORE the
	// safe input resolver (step 4) converts them into typed ImageInputs. The
	// HTTP handler populates this in step 3 so the capability table and the
	// adapter probe can observe presence; step 4 will replace it with resolved
	// ImageInputs and clear it.
	RawImageInputs []json.RawMessage
	// RawMask carries the canonical raw mask value (one of mask_image/maskImage/
	// mask after alias canonicalization) before the safe input resolver (step 4)
	// converts it into a typed Mask.
	RawMask json.RawMessage
	// Width/Height override Size when set (Cloudflare JSON / some providers).
	Width  json.RawMessage
	Height json.RawMessage
	// NegativePrompt, Guidance, Seed, NumSteps, Steps, Strength are the six
	// named Cloudflare-ish optional fields. json.RawMessage preserves presence
	// and the numeric-zero-vs-null distinction.
	NegativePrompt json.RawMessage
	Guidance       json.RawMessage
	Seed           json.RawMessage
	NumSteps       json.RawMessage
	Steps          json.RawMessage
	Strength       json.RawMessage
}

// ImageInput is one validated image input (data URL or HTTPS URL) produced by
// the safe input resolver (step 4). The adapter never re-validates.
type ImageInput struct {
	// Kind is "data" (inline base64) or "url" (remote HTTPS).
	Kind string
	// B64 is the decoded base64 bytes for a data: URL (Kind=="data").
	B64 string
	// URL is the HTTPS URL for a remote input (Kind=="url").
	URL string
	// MIME is the authoritative MIME sniffed from bytes (PNG/JPEG/WebP).
	MIME string
	// Host is the SSRF-validated, IP-pinned target for a URL input. The adapter
	// passes it to h.do so the production executor (wire.go) pins the dial to
	// the validated IP:port. Zero value for data inputs.
	Host ValidatedHost
}

// Result is the output of Handle.
type Result struct {
	StatusCode  int
	Err         error
	Body        []byte
	ContentType string
}

// Handle dispatches the image-generation upstream call by the provider's
// static config.
func (h *Handler) Handle(ctx context.Context, req Request) Result {
	cfg, ok := image.Lookup(req.ProviderID)
	if !ok {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("provider '%s' does not support image generation", req.ProviderID)}
	}
	if cfg.Unsupported {
		return Result{StatusCode: http.StatusNotImplemented, Err: fmt.Errorf("provider '%s' image transport not implemented in Go build", req.ProviderID)}
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("missing required field: prompt")}
	}
	if cfg.AuthType != image.AuthTypeNone && credentialToken(req.Credentials) == "" {
		return Result{StatusCode: http.StatusUnauthorized, Err: fmt.Errorf("no credentials for provider: %s", req.ProviderID)}
	}

	body, contentType, status, err := h.synthesize(ctx, cfg, req)
	if err != nil {
		return Result{StatusCode: status, Err: err}
	}
	return Result{StatusCode: status, Body: body, ContentType: contentType}
}

// synthesize dispatches by the provider's static Format.
func (h *Handler) synthesize(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	switch cfg.Format {
	case image.FormatOpenAI:
		return h.synthOpenAICompatible(ctx, cfg, req)
	case image.FormatGemini:
		return h.synthGemini(ctx, cfg, req)
	case image.FormatCodex:
		return h.synthCodex(ctx, cfg, req)
	case image.FormatSDWebUI:
		return h.synthSDWebUI(ctx, cfg, req)
	case image.FormatComfyUI:
		return h.synthComfyUI(ctx, cfg, req)
	case image.FormatHuggingFace:
		return h.synthHuggingFace(ctx, cfg, req)
	case image.FormatStability:
		return h.synthStability(ctx, cfg, req)
	default:
		return nil, "", http.StatusNotImplemented, fmt.Errorf("image format %q not implemented", cfg.Format)
	}
}

// do is the single outbound HTTP entry point. It attaches transport metadata
// (provider, connection, credentials, lifecycle phase, validated host) to the
// request context, hands the request to the injected HTTPExecutor, and logs only
// provider/model/phase/status/redacted URL — never prompt, credentials, or
// image bytes. Connection-backed requests carry the connection ID so the
// production executor (wire.go) can resolve proxy settings and forward a
// proxy.ValidatedTarget for untrusted image URLs.
func (h *Handler) do(ctx context.Context, req *http.Request, provider, phase string, creds domainProv.Credentials, connID string, host ValidatedHost) (*http.Response, error) {
	// If the request already carries a ValidatedHost (e.g. the poll factory
	// pre-resolved the poll URL, or the adapter pinned an input image host),
	// preserve it — the caller knows the validated target, not h.do. Only
	// fall back to the `host` argument when no validated host is present.
	if existing, ok := TransportMetadataFromContext(req.Context()); ok && existing.ValidatedHost.IsPinned() {
		host = existing.ValidatedHost
		// Preserve the connection ID and provider the factory set too.
		if connID == "" {
			connID = existing.ConnectionID
		}
		if provider == "" {
			provider = existing.ProviderID
		}
	}
	meta := TransportMetadata{
		ProviderID:    provider,
		ConnectionID:  connID,
		Credentials:   creds,
		Phase:         phase,
		ValidatedHost: host,
	}
	// Attach to the request context too so the production executor can read it
	// without a separate context argument (it only sees *http.Request).
	req = req.WithContext(WithTransportMetadata(req.Context(), meta))
	resp, err := h.deps.Executor.Do(req)
	if err != nil {
		h.deps.Logger.Warnf("image %s %s phase=%s err=%v url=%s", provider, req.URL.Hostname(), phase, err, redactedURL(req.URL))
		return nil, err
	}
	h.deps.Logger.Debugf("image %s %s phase=%s status=%d url=%s", provider, req.URL.Hostname(), phase, resp.StatusCode, redactedURL(req.URL))
	return resp, nil
}

// redactedURL returns scheme://host/path with query/fragment stripped. It is
// the only URL shape ever logged for image lifecycle calls — credentials live
// in headers/query and must never reach logs.
func redactedURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// logLifecycle emits one structured lifecycle log line carrying only the
// spec-approved fields: provider, model, phase, status and a redacted URL.
// It MUST NOT receive prompt, credentials, or image bytes. h.do already logs
// per-call; this helper exists for adapter-level lifecycle boundaries (e.g.
// "submit complete, polling started") that are not tied to a single HTTP call.
func (h *Handler) logLifecycle(provider, model, phase, status string, u *url.URL) {
	h.deps.Logger.Debugf("image lifecycle %s %s model=%s phase=%s status=%s url=%s", provider, redactedURL(u), model, phase, status, redactedURL(u))
}

// synthOpenAICompatible builds the OpenAI {model,prompt,n,size,quality,style,
// response_format} body (optionally whitelisted via cfg.BodyFields), POSTs to
// cfg.BaseURL, and returns the upstream response verbatim (OpenAI shape
// {created, data:[…]}). For response_format=binary it extracts the first
// image (b64_json or downloads url) and returns raw image bytes.
func (h *Handler) synthOpenAICompatible(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	body := buildOpenAIBody(req, cfg.BodyFields)
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setAuthHeader(httpReq, cfg, req.Credentials)
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	resp, err := h.do(ctx, httpReq, req.ProviderID, "submit", req.Credentials, connectionID(req.Credentials), ValidatedHost{})
	if err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, "", resp.StatusCode, upstreamError(respBody)
	}
	// Binary output: extract first image and return raw bytes.
	if req.ResponseFormat == "binary" {
		return h.toBinary(respBody, req.OutputFormat)
	}
	return respBody, "application/json", resp.StatusCode, nil
}

// synthGemini calls generateContent with responseModalities ["TEXT","IMAGE"]
// and reshapes candidates[].content.parts[].inlineData.data into the OpenAI
// {created, data:[{b64_json}]} shape. Binary output returns the first image
// raw bytes.
func (h *Handler) synthGemini(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	modelID := strings.TrimPrefix(req.Model, "models/")
	url := fmt.Sprintf("%s/%s:generateContent", cfg.BaseURL, modelID)
	// Gemini uses ?key=<tok> — append it.
	tok := credentialToken(req.Credentials)
	if tok != "" {
		url += "?key=" + tok
	}
	payload := map[string]any{
		"contents": []any{
			map[string]any{"parts": []any{map[string]any{"text": req.Prompt}}},
		},
		"generationConfig": map[string]any{
			"responseModalities": []string{"TEXT", "IMAGE"},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	resp, err := h.do(ctx, httpReq, req.ProviderID, "submit", req.Credentials, connectionID(req.Credentials), ValidatedHost{})
	if err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, "", resp.StatusCode, upstreamError(respBody)
	}
	// Reshape: collect all inlineData.data base64 blobs.
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *struct {
						Data     string `json:"data"`
						MimeType string `json:"mimeType"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("gemini: failed to parse response: %w", err)
	}
	var images []map[string]any
	for _, c := range parsed.Candidates {
		for _, p := range c.Content.Parts {
			if p.InlineData != nil && p.InlineData.Data != "" {
				images = append(images, map[string]any{"b64_json": p.InlineData.Data})
			}
		}
	}
	if len(images) == 0 {
		return nil, "", http.StatusBadGateway, fmt.Errorf("gemini: no image in response")
	}
	out := map[string]any{
		"created": time.Now().Unix(),
		"data":    images,
	}
	outBody, _ := json.Marshal(out)
	if req.ResponseFormat == "binary" {
		return h.toBinary(outBody, req.OutputFormat)
	}
	return outBody, "application/json", http.StatusOK, nil
}

// synthCodex calls the Codex Responses API with tools:[{type:"image_generation",
// output_format,size,quality,background}], parses the SSE stream for the
// image_generation_call / output_item.done events carrying the base64 result,
// and returns {created, data:[{b64_json}]}. Codex input images / streaming
// passthrough are deferred.
func (h *Handler) synthCodex(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	// Codex uses the model id without the -image suffix (per JS: gpt-5.x-image
	// → upstream model drops -image).
	upstreamModel := strings.TrimSuffix(req.Model, "-image")
	url := strings.TrimRight(cfg.BaseURL, "/") + "/responses"
	payload := map[string]any{
		"model":  upstreamModel,
		"input":  []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": req.Prompt}}}},
		"stream": true,
		"tools": []any{
			map[string]any{
				"type":          "image_generation",
				"output_format": orDefault(req.OutputFormat, "png"),
				"size":          orDefault(req.Size, "1024x1024"),
				"quality":       orDefault(req.Quality, "auto"),
				"background":    orDefault(req.Background, "auto"),
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	// Codex CLI literal headers (legacy codex.js buildHeaders): version pins
	// the Codex CLI build the image_generation tool contract targets.
	httpReq.Header.Set("version", "0.136.0")
	httpReq.Header.Set("originator", "codex_cli_rs")
	httpReq.Header.Set("user-agent", "codex_cli_rs/0.136.0")
	httpReq.Header.Set("session_id", codexSessionID())
	httpReq.Header.Set("x-client-request-id", codexSessionID())
	tok := credentialToken(req.Credentials)
	if tok != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}
	// chatgpt-account-id is carried in the credentials' providerSpecificData.
	if acct, _ := req.Credentials.ProviderSpecificData["chatgptAccountID"].(string); acct != "" {
		httpReq.Header.Set("chatgpt-account-id", acct)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	resp, err := h.do(ctx, httpReq, req.ProviderID, "submit", req.Credentials, connectionID(req.Credentials), ValidatedHost{})
	if err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", resp.StatusCode, upstreamError(body)
	}
	// Parse the SSE stream for image base64 results.
	imgs, err := parseCodexSSE(resp.Body)
	if err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("codex: %w", err)
	}
	if len(imgs) == 0 {
		return nil, "", http.StatusBadGateway, fmt.Errorf("codex: no image in response")
	}
	data := make([]any, 0, len(imgs))
	for _, b64 := range imgs {
		data = append(data, map[string]any{"b64_json": b64})
	}
	out := map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
	}
	outBody, _ := json.Marshal(out)
	if req.ResponseFormat == "binary" {
		return h.toBinary(outBody, req.OutputFormat)
	}
	return outBody, "application/json", http.StatusOK, nil
}

// parseCodexSSE reads the Codex SSE stream and collects image base64 payloads
// from response.image_generation_call.partial_image (b64 field) and
// response.output_item.done item.result (base64 string). The final
// output_item.done carries the complete image.
func parseCodexSSE(r io.Reader) ([]string, error) {
	var images []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20) // images are large
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "response.image_generation_call.partial_image":
			if b64, _ := ev["b64"].(string); b64 != "" {
				images = append(images, b64)
			}
		case "response.output_item.done":
			item, _ := ev["item"].(map[string]any)
			if res, _ := item["result"].(string); res != "" {
				images = append(images, res)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return images, err
	}
	return images, nil
}

// buildOpenAIBody constructs the OpenAI images/generations request body,
// optionally whitelisting fields via bodyFields (xai). n defaults to 1, size
// to "1024x1024" when not set.
func buildOpenAIBody(req Request, bodyFields []string) map[string]any {
	all := map[string]any{
		"model":  req.Model,
		"prompt": req.Prompt,
	}
	if req.N > 0 {
		all["n"] = req.N
	} else {
		all["n"] = 1
	}
	if req.Size != "" {
		all["size"] = req.Size
	} else {
		all["size"] = "1024x1024"
	}
	if req.Quality != "" {
		all["quality"] = req.Quality
	}
	if req.Style != "" {
		all["style"] = req.Style
	}
	if req.ResponseFormat == "url" || req.ResponseFormat == "b64_json" {
		all["response_format"] = req.ResponseFormat
	}
	if len(bodyFields) == 0 {
		return all
	}
	out := make(map[string]any, len(bodyFields))
	for _, f := range bodyFields {
		if v, ok := all[f]; ok {
			out[f] = v
		}
	}
	return out
}

// toBinary extracts the first image from an OpenAI-shape body and returns the
// raw decoded image bytes with the authoritative MIME. The b64_json branch
// decodes + magic-sniffs; the url branch downloads the URL through the same
// executor as submit/poll (h.do) with the SSRF guard, 64 MiB cap, redirect
// contract and magic-byte sniff from image_security.go. Per spec point 11 the
// URL branch no longer returns 501.
func (h *Handler) toBinary(openAIBody []byte, outputFormat string) ([]byte, string, int, error) {
	var parsed struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(openAIBody, &parsed); err != nil || len(parsed.Data) == 0 {
		return nil, "", http.StatusBadGateway, fmt.Errorf("no image data to emit as binary")
	}
	if parsed.Data[0].B64JSON != "" {
		dec, err := base64.StdEncoding.DecodeString(parsed.Data[0].B64JSON)
		if err != nil {
			dec, err = base64.URLEncoding.DecodeString(parsed.Data[0].B64JSON)
			if err != nil {
				dec, err = base64.RawStdEncoding.DecodeString(parsed.Data[0].B64JSON)
				if err != nil {
					return nil, "", http.StatusBadGateway, fmt.Errorf("malformed base64 image: %w", err)
				}
			}
		}
		_, mime, err := decodeAndSniffBytes(dec)
		if err != nil {
			return nil, "", http.StatusBadGateway, err
		}
		return dec, mime, http.StatusOK, nil
	}
	if parsed.Data[0].URL != "" {
		return h.downloadImageURL(context.Background(), parsed.Data[0].URL, "", "", func(u *url.URL) (*http.Request, error) {
			return http.NewRequestWithContext(context.Background(), http.MethodGet, u.String(), nil)
		})
	}
	return nil, "", http.StatusBadGateway, fmt.Errorf("no image data to emit as binary")
}

// === helpers ===

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func credentialToken(c domainProv.Credentials) string {
	if c.AccessToken != "" {
		return c.AccessToken
	}
	return c.APIKey
}

// connectionID extracts the resolved connection id carried in credentials'
// ProviderSpecificData (set by the chat-path credential resolver). It is the
// connection the production executor (wire.go) uses to load proxy settings;
// "" means a no-auth / direct-only request.
func connectionID(c domainProv.Credentials) string {
	if c.ProviderSpecificData == nil {
		return ""
	}
	if id, ok := c.ProviderSpecificData["_connectionId"].(string); ok {
		return id
	}
	return ""
}

func setAuthHeader(r *http.Request, cfg image.Config, c domainProv.Credentials) {
	if cfg.AuthType == image.AuthTypeNone {
		return
	}
	tok := credentialToken(c)
	switch cfg.AuthHeader {
	case image.AuthBearer, image.AuthBearerAccount:
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
	case image.AuthKey:
		// Gemini uses ?key=, handled inline in synthGemini.
	case image.AuthXKey:
		if tok != "" {
			r.Header.Set("x-key", tok)
		}
	case image.AuthFalKey:
		if tok != "" {
			r.Header.Set("Authorization", "Key "+tok)
		}
	}
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
		var s string
		if json.Unmarshal(wrapped.Error, &s) == nil && s != "" {
			return fmt.Errorf("upstream: %s", s)
		}
	}
	// Bare {"message":...} shape.
	var bare struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &bare) == nil && bare.Message != "" {
		return fmt.Errorf("upstream: %s", bare.Message)
	}
	// Raw string body.
	var raw json.RawMessage
	if json.Unmarshal(body, &raw) == nil {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return fmt.Errorf("upstream: %s", s)
		}
	}
	return fmt.Errorf("upstream error")
}

func decodeBase64(s string) []byte {
	// Try standard base64 first, then URL-safe (Gemini/Codex may emit either).
	if out, err := base64.StdEncoding.DecodeString(s); err == nil {
		return out
	}
	if out, err := base64.URLEncoding.DecodeString(s); err == nil {
		return out
	}
	out, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return out
}
