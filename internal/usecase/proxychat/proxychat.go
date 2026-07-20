// Package proxychat implements the /v1 chat pipeline for the Go rewrite.
// It ports the handleChatCore control flow from open-sse/handlers/chatCore.js
// with the Artiffusion fork patches: env timeouts, error SSE on stall/abort,
// JSON→SSE synthesis for non-streaming upstreams, and adaptive stream-readiness.
package proxychat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	reg "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider"
	httpstream "github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/http"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/usage"
)

// TokenSaverConfig holds the runtime token-saver switches. All stages are gated
// by the X-9Gouter-Token-Saver request header (config.TOKEN_SAVER_HEADER).
type TokenSaverConfig struct {
	RtkEnabled              bool
	HeadroomEnabled         bool
	HeadroomURL             string
	HeadroomCompressUser    bool
	CavemanEnabled          bool
	CavemanLevel            string
	PonytailEnabled         bool
	PonytailLevel           string
	PxpipeEnabled           bool
	PxpipeMinChars          int
	PxpipeTimeoutMs         int
	PxpipeTransform         func([]byte, string, int) ([]byte, error)
}

// Request is the input to Handle.
type Request struct {
	Ctx            context.Context
	Body           json.RawMessage
	Endpoint       string // e.g. "/v1/chat/completions"
	Headers        http.Header
	ProviderID     string
	Model          string
	Credentials    domainProv.Credentials
	Stream         bool
	APIKey         string
	ConnectionID   string
	UserAgent      string
	TokenSavers    TokenSaverConfig
	ResponseWriter http.ResponseWriter
	SSEWriter      *httpstream.Writer
}

// Result is the output of Handle.
type Result struct {
	StatusCode int
	Streamed   bool
	Err        error
}

// Dependencies collects the collaborators consumed by the usecase.
type Dependencies struct {
	Registry       func(id string) (DomainProvider, error)
	UsageRepo      usage.Repo
	StreamPipe     StreamPiper
	JSONToSSE      JSONToSSETranslator
	Logger         Logger
	Config         config.Config
}

// DomainProvider narrows provider.Provider to the fields we need.
type DomainProvider interface {
	ID() string
	Executor() domainProv.Executor
}

// StreamPiper abstracts httpstream.Pipe.
type StreamPiper interface {
	Pipe(ctx context.Context, upstream io.Reader, w *httpstream.Writer, opts httpstream.PipeOpts) error
}

// JSONToSSETranslator abstracts translator.Synthesize.
type JSONToSSETranslator interface {
	Synthesize(body []byte) (string, error)
}

// Logger is a minimal log sink.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

// Handler is the compiled proxychat usecase.
type Handler struct {
	deps Dependencies
}

// New creates a Handler. Passing nil for any collaborator selects a default:
// real registry lookup, real stream pipe, real JSON→SSE synthesis, and a no-op
// logger. This keeps tests lightweight while still exercising the pipeline.
func New(deps Dependencies) *Handler {
	if deps.Registry == nil {
		deps.Registry = func(id string) (DomainProvider, error) { return reg.Lookup(id) }
	}
	if deps.StreamPipe == nil {
		deps.StreamPipe = pipeAdapter{}
	}
	if deps.JSONToSSE == nil {
		deps.JSONToSSE = synthesizerFunc(translator.Synthesize)
	}
	if deps.Logger == nil {
		deps.Logger = noopLogger{}
	}
	return &Handler{deps: deps}
}

// synthesizerFunc adapts the package-level translator.Synthesize function to the
// JSONToSSETranslator interface.
type synthesizerFunc func([]byte) (string, error)

func (f synthesizerFunc) Synthesize(body []byte) (string, error) { return f(body) }

// Handle runs the full /v1 pipeline.
func (h *Handler) Handle(ctx context.Context, req Request) (Result, error) {
	start := time.Now()

	sourceFormat := detectSourceFormat(req.Endpoint, req.Body)

	prov, err := h.deps.Registry(req.ProviderID)
	if err != nil {
		return h.errorResult(http.StatusBadRequest, fmt.Sprintf("unknown provider %q", req.ProviderID), start)
	}
	providerID := prov.ID()
	exec := prov.Executor()

	targetFormat := resolveTargetFormat(providerID, sourceFormat)

	var translatedBody json.RawMessage
	if translator.NeedsTranslation(sourceFormat, targetFormat) {
		translatedBody, err = translator.TranslateRequest(sourceFormat, targetFormat, req.Model, req.Body, req.Stream, providerID)
		if err != nil {
			return h.errorResult(http.StatusBadRequest, fmt.Sprintf("translate request: %v", err), start)
		}
	} else {
		translatedBody = req.Body
	}

	bodyMap, err := rawToMap(translatedBody)
	if err != nil {
		return h.errorResult(http.StatusBadRequest, "invalid request body", start)
	}

	// Mirror open-sse/handlers/chatCore.js:151 — after request translation the
	// upstream body's "model" field is forced to the bare upstream model name
	// (req.Model is the part after the "provider/" routing prefix; it never
	// carries the namespace). Without this, a passthrough/no-op translation
	// (e.g. OpenAI→Ollama has no request translator) leaves the client's
	// "ollama/gemma3:4b" in the body and the upstream 404s with "model not found".
	bodyMap["model"] = req.Model

	// Token-saver pipeline, gated by X-9Gouter-Token-Saver header.
	tokenSaverEnabled := isTokenSaverEnabled(req.Headers, h.deps.Config)
	var headroomStats *headroomResult
	var pxpipeSummary string
	var xf []string

	if tokenSaverEnabled {
		rtkStats := runRtk(bodyMap, req.TokenSavers)
		if line := formatRtkLog(rtkStats); line != "" {
			h.deps.Logger.Infof(line)
		}

		headroomStats = runHeadroom(bodyMap, req.TokenSavers, providerID, targetFormat, h.deps.Logger)
		if headroomStats != nil {
			xf = append(xf, fmt.Sprintf("HEADROOM −%dtok", headroomStats.saved))
			h.deps.Logger.Infof("HEADROOM %s", headroomStats.log)
			h.deps.Logger.Infof("HEADROOM body=%s", headroomStats.sizeLog)
			h.deps.Logger.Infof("HEADROOM messages=%s", headroomStats.messagesLog)
			if headroomStats.phantom {
				h.deps.Logger.Warnf("HEADROOM reported token delta, but outbound JSON shrank <5%%; provider may bill near-original payload | %s", headroomStats.sizeLog)
			}
		} else if req.TokenSavers.HeadroomEnabled {
			reason := "compression unavailable"
			if headroomStats != nil && headroomStats.skippedReason != "" {
				reason = headroomStats.skippedReason
			}
			h.deps.Logger.Warnf("HEADROOM skipped: %s", reason)
		}

		if req.TokenSavers.CavemanEnabled && req.TokenSavers.CavemanLevel != "" {
			injectCaveman(bodyMap, targetFormat, req.TokenSavers.CavemanLevel)
			xf = append(xf, fmt.Sprintf("CAVEMAN:%s", req.TokenSavers.CavemanLevel))
		}

		if req.TokenSavers.PonytailEnabled && req.TokenSavers.PonytailLevel != "" {
			injectPonytail(bodyMap, targetFormat, req.TokenSavers.PonytailLevel)
			xf = append(xf, fmt.Sprintf("PONYTAIL:%s", req.TokenSavers.PonytailLevel))
		}

		if req.TokenSavers.PxpipeEnabled {
			pxRes := runPxpipe(bodyMap, targetFormat, req.Model, req.TokenSavers)
			if pxRes.summary != nil && pxRes.summary.Applied {
				bodyMap = pxRes.body
				pxpipeSummary = fmt.Sprintf("PXPIPE:%dimg", pxRes.summary.ImageCount)
				xf = append(xf, pxpipeSummary)
			}
		}
	}

	if len(xf) > 0 {
		h.deps.Logger.Infof("TOKEN-SAVERS %s", strings.Join(xf, " · "))
	}

	finalBody, err := mapToRaw(bodyMap)
	if err != nil {
		return h.errorResult(http.StatusBadRequest, "marshal final body", start)
	}

	// Resolve reasoning detection and adaptive readiness timeout.
	isReasoning := isThinkingEnabled(req.Body, req.Headers, req.Model)
	baseStall := h.deps.Config.StreamStallTimeout.Duration()
	if isReasoning {
		baseStall = h.deps.Config.StreamStallTimeoutReasoning.Duration()
	}
	readiness := ResolveStreamReadinessTimeout(
		baseStall,
		h.deps.Config.StreamReadinessMaxTimeout.Duration(),
		providerID,
		req.Model,
		req.Body,
	)
	h.deps.Logger.Debugf("STALL provider=%s model=%s reasoning=%v timeout=%v reasons=%v", providerID, req.Model, isReasoning, readiness.TimeoutMs, readiness.Reasons)

	execReq := domainProv.ExecRequest{
		Model:       req.Model,
		Body:        finalBody,
		Stream:      req.Stream,
		Credentials: req.Credentials,
	}
	resp, err := exec.Execute(ctx, execReq)
	if err != nil {
		status := http.StatusBadGateway
		if ctx.Err() != nil {
			status = 499
		}
		h.saveUsage(ctx, req, providerID, start, 0, 0, "error", nil, nil)
		return h.errorResult(status, fmt.Sprintf("upstream error: %v", err), start)
	}
	defer resp.Response.Body.Close()

	if resp.Response.StatusCode/100 != 2 {
		// provider returned non-2xx
		msg := readShortResponse(resp.Response)
		h.saveUsage(ctx, req, providerID, start, 0, 0, fmt.Sprintf("failed %d", resp.Response.StatusCode), nil, nil)
		return h.errorResult(resp.Response.StatusCode, msg, start)
	}

	// For streaming requests, pipe to the client (with JSON→SSE synthesis if needed).
	if req.Stream {
		// JSON→SSE synthesis is for non-streaming upstreams that ignore
		// stream:true and reply with a single application/json chat-completion
		// body (ported from OmniRoute #3089). It must NOT fire for upstreams
		// that genuinely stream a non-SSE format — notably Ollama/llama.cpp,
		// whose /api/chat streams NDJSON but labels it "application/json".
		// Reading such a body with io.ReadAll blocks forever waiting for an
		// EOF that arrives only when the stream finishes. De-framing +
		// translation for those formats is handled by the Pipe (FrameMode +
		// TranslateResponse), so skip synthesis whenever the target format
		// has its own framing strategy.
		ndjsonUpstream := frameModeFor(targetFormat) == "ndjson"
		if !ndjsonUpstream {
			contentType := strings.ToLower(resp.Response.Header.Get("content-type"))
			if strings.Contains(contentType, "application/json") && isOpenAIFormatted(sourceFormat) {
				bodyText, _ := io.ReadAll(resp.Response.Body)
				synthesized, _ := h.deps.JSONToSSE.Synthesize(bodyText)
				if synthesized != "" {
					h.saveUsage(ctx, req, providerID, start, 0, 0, "success", nil, nil)
					return h.serveSynthesizedSSE(ctx, req, synthesized)
				}
			}
		}

		w := req.SSEWriter
		if w == nil && req.ResponseWriter != nil {
			w = httpstream.New(req.ResponseWriter, ctx)
		}
		if w == nil {
			return Result{}, fmt.Errorf("no SSE writer available")
		}

		opts := httpstream.PipeOpts{
			StallTimeout:          readiness.TimeoutMs,
			StallTimeoutReasoning: h.deps.Config.StreamStallTimeoutReasoning.Duration(),
			IsThinkingModel:     isReasoning,
			Reason:              "stream_stall_timeout",
			Provider:            providerID,
			Model:               req.Model,
			FrameMode:           frameModeFor(targetFormat),
			TranslateResponse:   translateStreamChunk(sourceFormat, targetFormat),
		}
		err = h.deps.StreamPipe.Pipe(ctx, resp.Response.Body, w, opts)

		var promptTokens, completionTokens int
		if headroomStats != nil {
			promptTokens = headroomStats.tokensBefore
			completionTokens = headroomStats.tokensAfter - headroomStats.tokensBefore
			if completionTokens < 0 {
				completionTokens = 0
			}
		}
		h.saveUsage(ctx, req, providerID, start, promptTokens, completionTokens, "success", nil, nil)
		return Result{StatusCode: http.StatusOK, Streamed: true, Err: err}, nil
	}

	// Non-streaming path: read body, optionally translate response, write JSON.
	bodyBytes, err := io.ReadAll(resp.Response.Body)
	if err != nil {
		h.saveUsage(ctx, req, providerID, start, 0, 0, "error", nil, nil)
		return h.errorResult(http.StatusBadGateway, "read upstream body", start)
	}
	var upstreamBody map[string]any
	_ = json.Unmarshal(bodyBytes, &upstreamBody)

	clientBody := translateNonStreamingResponse(upstreamBody, sourceFormat, targetFormat)
	clientBytes, _ := json.Marshal(clientBody)

	if req.ResponseWriter != nil {
		req.ResponseWriter.Header().Set("Content-Type", "application/json")
		req.ResponseWriter.WriteHeader(http.StatusOK)
		_, _ = req.ResponseWriter.Write(clientBytes)
	}

	h.saveUsage(ctx, req, providerID, start, tokenCount(clientBody, "prompt_tokens", "input_tokens"), tokenCount(clientBody, "completion_tokens", "output_tokens"), "success", nil, nil)
	return Result{StatusCode: http.StatusOK}, nil
}

func (h *Handler) saveUsage(ctx context.Context, req Request, providerID string, start time.Time, prompt, completion int, status string, streamMs *int, tps *float64) {
	rec := usage.UsageRecord{
		Timestamp:        start,
		Provider:         providerID,
		Model:            req.Model,
		ConnectionID:     req.ConnectionID,
		APIKey:           req.APIKey,
		Endpoint:         req.Endpoint,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		Status:           status,
		StreamMs:         streamMs,
		TPS:              tps,
	}
	_ = h.deps.UsageRepo.Save(ctx, rec)
}

func (h *Handler) errorResult(status int, msg string, start time.Time) (Result, error) {
	return Result{StatusCode: status, Err: fmt.Errorf("%s", msg)}, nil
}

func (h *Handler) serveSynthesizedSSE(ctx context.Context, req Request, synthesized string) (Result, error) {
	w := req.SSEWriter
	if w == nil && req.ResponseWriter != nil {
		w = httpstream.New(req.ResponseWriter, ctx)
	}
	if w == nil {
		return Result{}, fmt.Errorf("no SSE writer available")
	}
	_ = w.WriteRaw([]byte(synthesized))
	return Result{StatusCode: http.StatusOK, Streamed: true}, nil
}

func detectSourceFormat(endpoint string, body json.RawMessage) format.Format {
	if f := format.DetectByEndpoint(endpoint, body); f != format.FormatUnknown {
		return f
	}
	// Fallback body heuristics: responses API has an input array, messages API
	// has a messages array, otherwise default to OpenAI.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err == nil {
		if _, ok := m["messages"]; ok {
			return format.Openai
		}
		if _, ok := m["input"]; ok {
			return format.OpenaiResponses
		}
	}
	return format.Openai
}

func resolveTargetFormat(providerID string, sourceFormat format.Format) format.Format {
	// In the JS pipeline modelTargetFormat/runtimeTransport would be resolved from
	// providerModels.js. For the Go port we keep the mapping minimal: providers
	// with a known format use it, otherwise fall back to the source format.
	switch providerID {
	case "claude", "anthropic", "glm", "kimi", "kimi-coding", "minimax", "minimax-cn":
		return format.Claude
	case "codex", "grok-cli", "perplexity-agent":
		return format.OpenaiResponses
	case "gemini", "gemini-cli", "antigravity", "vertex":
		return format.Gemini
	case "cursor":
		return format.Cursor
	case "ollama", "ollama-local":
		return format.Ollama
	case "commandcode":
		return format.Commandcode
	case "kiro":
		return format.Kiro
	}
	return sourceFormat
}

// frameModeFor maps the upstream target format to a de-framing strategy.
// Ollama/llama.cpp emit raw JSON lines (NDJSON); everything else is SSE.
func frameModeFor(targetFormat format.Format) string {
	if targetFormat == format.Ollama {
		return "ndjson"
	}
	return "sse"
}

// translateStreamChunk returns a Pipe TranslateResponse callback that
// converts a de-framed upstream chunk into source-format SSE events. When
// source and target match (native OpenAI providers, raw passthrough) it
// returns nil so the pipe re-terminates the frame byte-for-byte — the
// historical behaviour and what the streaming-passthrough tests assert.
func translateStreamChunk(sourceFormat, targetFormat format.Format) func([]byte, map[string]any) ([][]byte, error) {
	if !translator.NeedsTranslation(sourceFormat, targetFormat) {
		return nil
	}
	return func(frame []byte, state map[string]any) ([][]byte, error) {
		chunks, err := translator.TranslateResponse(targetFormat, sourceFormat, frame, state)
		if err != nil {
			return nil, err
		}
		out := make([][]byte, 0, len(chunks))
		for _, c := range chunks {
			if len(bytes.TrimSpace(c)) == 0 {
				continue
			}
			out = append(out, c)
		}
		return out, nil
	}
}

func isOpenAIFormatted(f format.Format) bool {
	return f == format.Openai || f == format.OpenaiResponses
}

func isTokenSaverEnabled(headers http.Header, cfg config.Config) bool {
	if headers == nil {
		return true
	}
	headerName := config.TOKEN_SAVER_HEADER
	if headerName == "" {
		headerName = "x-9gouter-token-saver"
	}
	v := strings.ToLower(headers.Get(headerName))
	return v != "off"
}

func isThinkingEnabled(body json.RawMessage, headers http.Header, model string) bool {
	if headers != nil {
		if beta := headers.Get("Anthropic-Beta"); strings.Contains(strings.ToLower(beta), "thinking") {
			return true
		}
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		if thinking, ok := m["thinking"].(map[string]any); ok {
			if t, _ := thinking["type"].(string); strings.ToLower(t) == "enabled" {
				return true
			}
		}
		if effort, ok := m["reasoning_effort"].(string); ok && effort != "" {
			return true
		}
	}
	mod := strings.ToLower(model)
	return strings.Contains(mod, "thinking") || strings.Contains(mod, "-reason")
}

func rawToMap(raw json.RawMessage) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func mapToRaw(m map[string]any) (json.RawMessage, error) {
	return json.Marshal(m)
}

func readShortResponse(resp *http.Response) string {
	var buf bytes.Buffer
	io.CopyN(&buf, resp.Body, 1024)
	return fmt.Sprintf("upstream returned %d: %s", resp.StatusCode, strings.TrimSpace(buf.String()))
}

func tokenCount(body map[string]any, keys ...string) int {
	usage, ok := body["usage"].(map[string]any)
	if !ok {
		return 0
	}
	for _, k := range keys {
		if v, ok := usage[k].(float64); ok {
			return int(v)
		}
	}
	return 0
}

func translateNonStreamingResponse(body map[string]any, sourceFormat, targetFormat format.Format) map[string]any {
	// Minimal port of nonStreamingHandler.js translateNonStreamingResponse.
	// For the OpenAI→OpenAI passthrough common case return as-is. Additional
	// format-specific translations are out of scope for this slice and will be
	// extended in Task 14/15 wiring.
	if sourceFormat == targetFormat {
		return body
	}
	if targetFormat == format.Openai {
		return body
	}
	return body
}

// pipeAdapter adapts httpstream.Pipe to the StreamPiper interface.
type pipeAdapter struct{}

func (pipeAdapter) Pipe(ctx context.Context, upstream io.Reader, w *httpstream.Writer, opts httpstream.PipeOpts) error {
	return httpstream.Pipe(ctx, upstream, w, opts)
}

type noopLogger struct{}

func (noopLogger) Infof(format string, args ...any)  {}
func (noopLogger) Warnf(format string, args ...any)  {}
func (noopLogger) Debugf(format string, args ...any) {}

// headroomResult carries the result of the headroom stage.
type headroomResult struct {
	saved          int
	log            string
	sizeLog        string
	messagesLog    string
	phantom        bool
	skippedReason  string
	tokensBefore   int
	tokensAfter    int
}
