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
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/pricing"
	reg "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider"
	httpstream "github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/http"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/shared"
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
	Registry    func(id string) (DomainProvider, error)
	UsageRepo   usage.Repo
	StreamPipe  StreamPiper
	JSONToSSE   JSONToSSETranslator
	Logger      Logger
	Config      config.Config
	UsageEvents UsageEventPublisher
	// Pricing computes the USD cost of a request from its token breakdown. nil
	// → cost stays 0 (legacy wiring / tests that only check token counts).
	Pricing *pricing.Resolver
}

// UsageEventPublisher is the live real-time analytics surface. proxychat
// publishes Start (before upstream call), Stop (after response/error), and
// Save (after usage repo write) events so the dashboard's SSE stream can push
// active/recent request updates. nil = no-op (tests/legacy wiring).
type UsageEventPublisher interface {
	PublishStart(model, provider, connectionID string)
	PublishStop(model, provider, connectionID string, errored bool)
	PublishSave(model, provider, status string, prompt, completion int, ts time.Time)
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

	// Port upstream 28894096: OpenAI's reasoning_effort enum caps at "xhigh" (no
	// "max"). Claude Code sends "max" (its top level); without a clamp, OpenAI
	// upstreams reject with HTTP 400 "max effort not support". The JS pipeline
	// applies this in thinkingUnified.applyFormat case "openai". Go has no
	// central applyThinking — passthrough bodies reach upstream byte-for-byte —
	// so clamp "max"→"xhigh" here for any OpenAI-native target format, mirroring
	// FORMAT_TO_NATIVE (openai/openai-responses/codex all map to "openai").
	clampReasoningEffortForOpenAINative(bodyMap, targetFormat)

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
	if h.deps.UsageEvents != nil {
		h.deps.UsageEvents.PublishStart(req.Model, providerID, req.ConnectionID)
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
	// closeAndDone closes the upstream body and then releases the fetch
	// context that owns its lifetime (resp.Done). Order matters: the body
	// must be closed before the fetch context is cancelled, otherwise a
	// streaming read races against context cancellation (the ollama NDJSON
	// 90s hang). It is used as a deferred cleanup for every return path.
	closeAndDone := func() {
		resp.Response.Body.Close()
		if resp.Done != nil {
			resp.Done()
		}
	}
	defer closeAndDone()

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
			// The Anthropic streaming format requires an "event: <type>\n" line
			// before each "data: <json>\n\n" frame; the Claude SDK (Claude Code)
			// rejects a stream that has only "data:" as malformed. Set the flag
			// when the CLIENT source format is Claude, mirroring legacy
			// streamHelpers.js formatSSE (sourceFormat === CLAUDE).
			EmitEventPrefix:     sourceFormat == format.Claude,
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

	prompt := tokenCount(clientBody, "prompt_tokens", "input_tokens")
	completion := tokenCount(clientBody, "completion_tokens", "output_tokens")
	tok := extractTokens(clientBody, prompt, completion)
	h.saveUsageWith(ctx, req, providerID, start, prompt, completion, "success", nil, nil, tok)
	return Result{StatusCode: http.StatusOK}, nil
}

func (h *Handler) saveUsage(ctx context.Context, req Request, providerID string, start time.Time, prompt, completion int, status string, streamMs *int, tps *float64) {
	h.saveUsageWith(ctx, req, providerID, start, prompt, completion, status, streamMs, tps, nil)
}

// saveUsageWith persists a usage record, optionally carrying a detailed token
// breakdown (cached/reasoning/cache_creation) used to compute the USD cost.
// When tok is nil, only the flat prompt/completion counts are recorded and cost
// is computed from those alone (cached/reasoning treated as 0). This mirrors
// the legacy saveRequestUsage → calculateCost(provider, model, tokens) path.
func (h *Handler) saveUsageWith(ctx context.Context, req Request, providerID string, start time.Time, prompt, completion int, status string, streamMs *int, tps *float64, tok *pricing.Tokens) {
	tokens := pricing.Tokens{PromptTokens: prompt, CompletionTokens: completion}
	if tok != nil {
		tokens.CachedTokens = tok.CachedTokens
		tokens.ReasoningTokens = tok.ReasoningTokens
		tokens.CacheCreationTokens = tok.CacheCreationTokens
	}
	cost := 0.0
	if h.deps.Pricing != nil {
		cost = h.deps.Pricing.CostFor(providerID, req.Model, tokens)
	}

	var tokensBlob json.RawMessage
	if tok != nil {
		b, err := json.Marshal(map[string]int{
			"prompt_tokens":                tokens.PromptTokens,
			"completion_tokens":             tokens.CompletionTokens,
			"cached_tokens":                 tokens.CachedTokens,
			"reasoning_tokens":              tokens.ReasoningTokens,
			"cache_creation_input_tokens":   tokens.CacheCreationTokens,
			"total_tokens":                  tokens.PromptTokens + tokens.CompletionTokens,
		})
		if err == nil {
			tokensBlob = b
		}
	}

	rec := usage.UsageRecord{
		Timestamp:        start,
		Provider:         providerID,
		Model:             req.Model,
		ConnectionID:      req.ConnectionID,
		APIKey:            req.APIKey,
		Endpoint:          req.Endpoint,
		PromptTokens:      prompt,
		CompletionTokens:  completion,
		Cost:              cost,
		Status:            status,
		StreamMs:          streamMs,
		TPS:               tps,
		Tokens:            tokensBlob,
	}
	_ = h.deps.UsageRepo.Save(ctx, rec)
	// Real-time analytics (#83): every completed request (success or error)
	// decrements the in-flight counter and feeds the recent-requests ring, so
	// the dashboard SSE stream can push an updated frame. errored = any status
	// that is not a plain "success".
	if h.deps.UsageEvents != nil {
		errored := status != "success" && !strings.HasPrefix(status, "failed ")
		h.deps.UsageEvents.PublishStop(req.Model, providerID, req.ConnectionID, errored)
		h.deps.UsageEvents.PublishSave(req.Model, providerID, status, prompt, completion, start)
	}
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

// openAINativeThinkingFormats are the target formats whose provider-native
// thinking representation is the OpenAI reasoning_effort enum, mirroring the
// JS FORMAT_TO_NATIVE entries that map to "openai".
var openAINativeThinkingFormats = map[format.Format]bool{
	format.Openai:          true,
	format.OpenaiResponses: true,
	format.OpenaiResponse:  true,
	format.Codex:           true,
}

// clampReasoningEffortForOpenAINative clamps a top-level reasoning_effort of
// "max" down to "xhigh" for OpenAI-native target formats. OpenAI's
// reasoning_effort enum has no "max" level (caps at "xhigh") and rejects "max"
// with HTTP 400. Other levels pass through unchanged. This ports upstream
// 28894096 (thinkingUnified.applyFormat case "openai"), applied here because Go
// has no central applyThinking and passthrough bodies reach upstream verbatim.
func clampReasoningEffortForOpenAINative(body map[string]any, targetFormat format.Format) {
	if !openAINativeThinkingFormats[targetFormat] {
		return
	}
	re, ok := body["reasoning_effort"].(string)
	if !ok {
		return
	}
	if strings.EqualFold(re, "max") {
		body["reasoning_effort"] = "xhigh"
	}
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
	_, _ = io.CopyN(&buf, resp.Body, 1024)
	return fmt.Sprintf("upstream returned %d: %s", resp.StatusCode, strings.TrimSpace(buf.String()))
}

func tokenCount(body map[string]any, keys ...string) int {
	usage, ok := body["usage"].(map[string]any)
	if !ok {
		return 0
	}
	for _, k := range keys {
		if n, ok := numericInt(usage[k]); ok {
			return n
		}
	}
	return 0
}

// extractTokens pulls the detailed token breakdown (cached/reasoning/cache
// creation) out of a non-streaming response body so saveUsageWith can compute
// cost. prompt/completion are passed in (already resolved by the caller via
// tokenCount, which handles the prompt_tokens/input_tokens aliasing) rather
// than re-reading. Cached and cache_creation come from the OpenAI/Claude usage
// fields; reasoning comes from completion_tokens_details.reasoning_tokens
// (OpenAI o-series) or the top-level reasoning_tokens (translated formats).
// Missing fields are 0 — the cost formula tolerates a partial breakdown.
func extractTokens(body map[string]any, prompt, completion int) *pricing.Tokens {
	usage, ok := body["usage"].(map[string]any)
	if !ok {
		return nil
	}
	t := &pricing.Tokens{PromptTokens: prompt, CompletionTokens: completion}
	// cached_tokens (OpenAI/Gemini canonical) or cache_read_input_tokens (Claude).
	if n, ok := numericInt(usage["cached_tokens"]); ok {
		t.CachedTokens = n
	} else if n, ok := numericInt(usage["cache_read_input_tokens"]); ok {
		t.CachedTokens = n
	}
	// cache_creation_input_tokens (Claude) or nested prompt_tokens_details.
	if n, ok := numericInt(usage["cache_creation_input_tokens"]); ok {
		t.CacheCreationTokens = n
	} else if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if n, ok := numericInt(details["cache_creation_tokens"]); ok {
			t.CacheCreationTokens = n
		}
	}
	// reasoning_tokens: top-level (translated formats) or nested
	// completion_tokens_details.reasoning_tokens (OpenAI o-series).
	if n, ok := numericInt(usage["reasoning_tokens"]); ok {
		t.ReasoningTokens = n
	} else if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		if n, ok := numericInt(details["reasoning_tokens"]); ok {
			t.ReasoningTokens = n
		}
	}
	return t
}

// numericInt coerces the numeric value v to an int, accepting the full set of
// types a usage field may take after json.Unmarshal (float64, json.Number) or
// after an in-process translation that builds the usage map via BuildUsage
// (int, int64). This matters for non-stream responses translated from
// ollama/claude/gemini→openai: BuildUsage emits int values, so a tokenCount
// that only matched float64 (the raw json.Unmarshal type) would record 0 for
// every translated response. Returns ok=false for non-numeric / missing
// values so callers can fall through to the next key.
func numericInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case int64:
		return int(x), true
	case json.Number:
		i, err := x.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}

func translateNonStreamingResponse(body map[string]any, sourceFormat, targetFormat format.Format) map[string]any {
	// Port of nonStreamingHandler.js translateNonStreamingResponse.
	// In the Go pipeline sourceFormat = the CLIENT wire format (what the
	// caller sent, usually OpenAI) and targetFormat = the UPSTREAM wire
	// format (what the provider speaks, e.g. Ollama). The non-stream
	// upstream reply is in targetFormat; we must convert it back to
	// sourceFormat for the client. When the two match, return as-is.
	if sourceFormat == targetFormat {
		return body
	}

	// Ollama upstream → OpenAI client: ollama's non-stream chat reply uses
	// {message:{content,...}, done, done_reason, eval_count} — NOT the
	// OpenAI {choices:[{message}]} shape. Convert so OpenAI clients (and the
	// dashboard model-test probe) see a real choices array. Ports
	// ollamaBodyToOpenAI from open-sse/translator/response/ollama-to-openai.js.
	if targetFormat == format.Ollama && sourceFormat == format.Openai {
		return ollamaBodyToOpenAI(body)
	}

	// Claude upstream → OpenAI client: claude's non-stream reply is
	// {content:[{type:"text"|"thinking"|"tool_use",...}], stop_reason, usage}.
	// Convert to OpenAI {choices:[{message}]} so OpenAI clients (and the
	// dashboard model-test probe) see a real choices array. Ports the
	// claude branch of nonStreamingHandler.js translateNonStreamingResponse.
	if (targetFormat == format.Claude || targetFormat == format.Kiro) && sourceFormat == format.Openai {
		if translated := claudeBodyToOpenAI(body); translated != nil {
			return translated
		}
	}

	// Gemini/Antigravity upstream → OpenAI client: gemini's non-stream reply
	// is {candidates:[{content:{parts:[...]}, finishReason}], usageMetadata}.
	// Convert to OpenAI {choices:[{message}]}. Ports the gemini branch of
	// nonStreamingHandler.js translateNonStreamingResponse.
	if (targetFormat == format.Gemini || targetFormat == format.GeminiCli || targetFormat == format.Antigravity || targetFormat == format.Vertex) && sourceFormat == format.Openai {
		if translated := geminiBodyToOpenAI(body); translated != nil {
			return translated
		}
	}

	// Default passthrough (Claude/Gemini/Kiro non-stream translation is
	// handled by the streaming pipe for stream:true requests; non-stream for
	// those formats is a follow-up). At minimum return the body so OpenAI
	// clients that already got an OpenAI-shaped upstream body keep working.
	if sourceFormat == format.Openai {
		return body
	}
	return body
}

// ollamaBodyToOpenAI converts a single Ollama non-streaming chat response body
// into an OpenAI chat.completion object. Mirrors the legacy
// ollamaBodyToOpenAI() so non-stream clients (including the dashboard
// /api/models/test probe, which inspects parsed.choices) get a real choices
// array instead of the Ollama-native message/eval_count shape.
func ollamaBodyToOpenAI(body map[string]any) map[string]any {
	if body == nil {
		return nil
	}
	message, _ := body["message"].(map[string]any)
	content, _ := message["content"].(string)
	thinking, _ := message["thinking"].(string)
	rawToolCalls, _ := message["tool_calls"].([]any)

	out := map[string]any{
		"role": "assistant",
	}
	hasContent := false
	if content != "" {
		out["content"] = content
		hasContent = true
	}
	if thinking != "" {
		out["reasoning_content"] = thinking
	}
	if len(rawToolCalls) > 0 {
		out["tool_calls"] = convertOllamaToolCallsNonStream(rawToolCalls)
		hasContent = true
	}
	if !hasContent && len(rawToolCalls) == 0 {
		out["content"] = ""
	}

	finishReason := shared.ToOpenAIFinish(fmt.Sprint(body["done_reason"]), "ollama")
	if len(rawToolCalls) > 0 {
		finishReason = "tool_calls"
	}

	model, _ := body["model"].(string)
	if model == "" {
		model = "ollama"
	}
	created := int(time.Now().Unix())

	result := map[string]any{
		"id":      "chatcmpl-" + shared.FallbackChatID(),
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       out,
				"finish_reason": finishReason,
			},
		},
	}
	if usage := shared.ToOpenAIUsage(body, "ollama"); usage != nil {
		result["usage"] = usage
	}
	return result
}

// convertOllamaToolCallsNonStream normalizes Ollama tool_calls into the OpenAI
// shape for a non-streaming completion message. (The streaming variant lives
// in the ollama translator package; this is the message-level counterpart.)
func convertOllamaToolCallsNonStream(raw []any) []map[string]any {
	out := make([]map[string]any, 0, len(raw))
	for i, rawTC := range raw {
		tc, ok := rawTC.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			fn = map[string]any{}
		}
		name, _ := fn["name"].(string)
		args := fn["arguments"]
		argsStr := ""
		switch a := args.(type) {
		case string:
			argsStr = a
		case map[string]any:
			b, _ := json.Marshal(a)
			argsStr = string(b)
		default:
			b, _ := json.Marshal(args)
			argsStr = string(b)
		}
		if argsStr == "" || argsStr == "null" {
			argsStr = "{}"
		}
		tcID, _ := tc["id"].(string)
		if tcID == "" {
			tcID = fmt.Sprintf("call_%d_%d", i, time.Now().UnixMilli())
		}
		out = append(out, map[string]any{
			"id":   tcID,
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": argsStr,
			},
		})
	}
	return out
}

// claudeBodyToOpenAI converts a single Claude/Kiro non-streaming response body
// into an OpenAI chat.completion object. Mirrors the claude branch of legacy
// nonStreamingHandler.js translateNonStreamingResponse. Claude replies with
// {content:[{type:"text"|"thinking"|"tool_use"}], stop_reason, usage:{input_tokens,output_tokens}}.
// Returns nil if the body is already OpenAI-shaped (has choices) — some
// providers reply OpenAI-native even when the request was translated to Claude.
func claudeBodyToOpenAI(body map[string]any) map[string]any {
	if body == nil {
		return nil
	}
	// Already OpenAI-shaped (e.g. xiaomi-tokenplan replies OpenAI even for
	// claude-format requests) — leave as-is.
	if _, ok := body["choices"]; ok {
		return nil
	}
	// content present but not an array → likely a different non-Claude format.
	if c, ok := body["content"]; ok && !isArray(c) {
		return nil
	}

	var textContent, thinkingContent string
	var toolCalls []map[string]any

	if blocks, ok := body["content"].([]any); ok {
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "text":
				raw, _ := block["text"].(string)
				textContent += stripCodeFence(raw)
			case "thinking":
				t, _ := block["thinking"].(string)
				thinkingContent += t
			case "tool_use":
				id, _ := block["id"].(string)
				if id == "" {
					id = fmt.Sprintf("toolu_%d_%d", time.Now().UnixMilli(), len(toolCalls))
				}
				name, _ := block["name"].(string)
				argsStr := "{}"
				if input := block["input"]; input != nil {
					if s, ok := input.(string); ok {
						argsStr = s
					} else {
						bb, _ := json.Marshal(input)
						argsStr = string(bb)
					}
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": argsStr,
					},
				})
			}
		}
	}

	message := map[string]any{"role": "assistant"}
	if textContent != "" {
		message["content"] = textContent
	}
	if thinkingContent != "" {
		message["reasoning_content"] = thinkingContent
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if _, ok := message["content"]; !ok && len(toolCalls) == 0 {
		message["content"] = ""
	}

	finishReason, _ := body["stop_reason"].(string)
	finishReason = shared.ToOpenAIFinish(finishReason, "claude")
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	model, _ := body["model"].(string)
	if model == "" {
		model = "claude"
	}
	id, _ := body["id"].(string)
	if id == "" {
		id = shared.FallbackChatID()
	} else {
		id = strings.TrimPrefix(id, "chatcmpl-")
	}

	result := map[string]any{
		"id":      "chatcmpl-" + id,
		"object":  "chat.completion",
		"created": int(time.Now().Unix()),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage, ok := body["usage"].(map[string]any); ok {
		input, _ := usage["input_tokens"].(float64)
		output, _ := usage["output_tokens"].(float64)
		pi, po := int(input), int(output)
		result["usage"] = map[string]any{
			"prompt_tokens":     pi,
			"completion_tokens": po,
			"total_tokens":      pi + po,
		}
	}
	return result
}

// geminiBodyToOpenAI converts a single Gemini/Antigravity/Vertex non-streaming
// response body into an OpenAI chat.completion object. Mirrors the gemini branch
// of legacy nonStreamingHandler.js translateNonStreamingResponse. Gemini replies
// with {candidates:[{content:{parts:[...]}, finishReason}], usageMetadata}.
// Returns nil if the body has no candidates (leave as-is for non-gemini shapes).
func geminiBodyToOpenAI(body map[string]any) map[string]any {
	if body == nil {
		return nil
	}
	// The response may be wrapped in {response: {...}} (Vertex/Antigravity).
	resp := body
	if inner, ok := body["response"].(map[string]any); ok {
		resp = inner
	}
	candidates, ok := resp["candidates"].([]any)
	if !ok || len(candidates) == 0 {
		return nil
	}
	candidate, ok := candidates[0].(map[string]any)
	if !ok {
		return nil
	}

	var textContent, reasoningContent string
	var toolCalls []map[string]any

	if content, ok := candidate["content"].(map[string]any); ok {
		if parts, ok := content["parts"].([]any); ok {
			for _, p := range parts {
				part, ok := p.(map[string]any)
				if !ok {
					continue
				}
				if thought, _ := part["thought"].(bool); thought {
					if t, ok := part["text"].(string); ok {
						reasoningContent += t
					}
					continue
				}
				if t, ok := part["text"].(string); ok {
					textContent += t
				}
				if fc, ok := part["functionCall"].(map[string]any); ok {
					name, _ := fc["name"].(string)
					args := fc["args"]
					argsStr := "{}"
					if args != nil {
						b, _ := json.Marshal(args)
						argsStr = string(b)
					}
					toolCalls = append(toolCalls, map[string]any{
						"id":   fmt.Sprintf("call_%s_%d_%d", name, time.Now().UnixMilli(), len(toolCalls)),
						"type": "function",
						"function": map[string]any{
							"name":      name,
							"arguments": argsStr,
						},
					})
				}
				// Inline image data from image-generation models.
				if inline := partMap(part, "inlineData", "inline_data"); inline != nil {
					if data, _ := inline["data"].(string); data != "" {
						mime, _ := firstString(inline, "mimeType", "mime_type").(string)
						if mime == "" {
							mime = "image/png"
						}
						textContent += "\n![image](data:" + mime + ";base64," + data + ")\n"
					}
				}
			}
		}
	}

	message := map[string]any{"role": "assistant"}
	if textContent != "" {
		message["content"] = textContent
	}
	if reasoningContent != "" {
		message["reasoning_content"] = reasoningContent
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if _, ok := message["content"]; !ok && len(toolCalls) == 0 {
		message["content"] = ""
	}

	finishReason := strings.ToLower(firstString(candidate, "finishReason").(string))
	if finishReason == "" {
		finishReason = "stop"
	}
	finishReason = shared.ToOpenAIFinish(finishReason, "gemini")
	if len(toolCalls) > 0 && finishReason == "stop" {
		finishReason = "tool_calls"
	}

	model, _ := firstString(resp, "modelVersion").(string)
	if model == "" {
		model = "gemini"
	}
	respID, _ := firstString(resp, "responseId").(string)
	id := respID
	if id == "" {
		id = shared.FallbackChatID()
	}

	result := map[string]any{
		"id":      "chatcmpl-" + id,
		"object":  "chat.completion",
		"created": int(time.Now().Unix()),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}

	usageMeta := firstMap(resp, body, "usageMetadata")
	if usageMeta != nil {
		promptTok := int(toFloat(usageMeta["promptTokenCount"])) + int(toFloat(usageMeta["thoughtsTokenCount"]))
		complTok := int(toFloat(usageMeta["candidatesTokenCount"]))
		totalTok := int(toFloat(usageMeta["totalTokenCount"]))
		if totalTok == 0 {
			totalTok = promptTok + complTok
		}
		usage := map[string]any{
			"prompt_tokens":     promptTok,
			"completion_tokens": complTok,
			"total_tokens":      totalTok,
		}
		if thoughts := int(toFloat(usageMeta["thoughtsTokenCount"])); thoughts > 0 {
			usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": thoughts}
		}
		result["usage"] = usage
	}
	return result
}

// stripCodeFence removes wrapping ```json ... ``` fences (some providers, e.g.
// kimi, wrap JSON tool text in a code block). Mirrors the legacy claude branch.
func stripCodeFence(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx >= 0 {
			first := strings.TrimSpace(s[:idx])
			if strings.HasPrefix(first, "```") {
				s = s[idx+1:]
			}
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return s
}

func isArray(v any) bool { _, ok := v.([]any); return ok }

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}

func firstString(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func firstMap(a, b map[string]any, key string) map[string]any {
	if v, ok := a[key].(map[string]any); ok {
		return v
	}
	if v, ok := b[key].(map[string]any); ok {
		return v
	}
	return nil
}

func partMap(part map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if v, ok := part[k].(map[string]any); ok {
			return v
		}
	}
	return nil
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
