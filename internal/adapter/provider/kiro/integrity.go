// integrity.go ports the integrity gate + bounded retry half of
// open-sse/executors/kiro.js (upstream v0.5.40, commit 6994cd1f): the
// attachIntegrityGate / runIntegrityRecovery / readRecoverableIntegrityAttempt /
// readIntegrityAttempt quartet plus the isEllipsisOnly / isShortFutureAction
// heuristics and appendRepairInstruction.
//
// Adaptation note: the JS gate wraps the response in a ReadableStream that
// emits a ": kiro-validation" heartbeat every 10s while it buffers an attempt
// (kiro.js:313-384). The Go executor model (Execute → synthetic *http.Response
// with a fully-buffered body, identical to the cursor executor) does not
// stream-and-buffer concurrently, so the heartbeat has no reader to keep alive
// and is dropped. Every other behavior — the private buffered attempt, the
// ttft/stall timeouts, the maxBytes bound, the terminal-disposition
// classification, the one bounded retry with repair instructions, the
// failure-SSE mapping — is ported 1:1. #103 wires this into the Kiro Execute
// override.
package kiroexec

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// IntegrityOptions configures one integrity-gated attempt (mirror the options
// built in attachIntegrityGate / runIntegrityRecovery in kiro.js:317-326).
type IntegrityOptions struct {
	// MaxBytes bounds the buffered attempt size (KIRO_TOOL_CALL_REPAIR_BUFFER_MAX_BYTES).
	MaxBytes int
	// TTFTTimeoutMs is the time-to-first-token timeout (KIRO_TOOL_CALL_REPAIR_TTFT_TIMEOUT_MS).
	TTFTTimeoutMs int
	// StallTimeoutMs is the inter-chunk stall timeout (KIRO_TOOL_CALL_REPAIR_STALL_TIMEOUT_MS).
	StallTimeoutMs int
	// RepairEnabled gates the bounded retry (KIRO_TOOL_CALL_REPAIR / per-credential override).
	RepairEnabled bool
}

// DefaultIntegrityOptions mirrors the env-overridable defaults in
// attachIntegrityGate (kiro.js:317-322) when no env override is set.
func DefaultIntegrityOptions(repairEnabled bool) IntegrityOptions {
	return IntegrityOptions{
		MaxBytes:       kiroRepairBufferMaxBytes,
		TTFTTimeoutMs:  kiroHeartbeatMs,
		StallTimeoutMs: kiroHeartbeatMs,
		RepairEnabled:  repairEnabled,
	}
}

// kiroHeartbeatMs mirrors KIRO_REPAIR_HEARTBEAT_MS (kiro.js:12). The JS legacy
// timeout fallback (STREAM_FIRST_CHUNK_TIMEOUT_MS) is owned by the caller
// (#103) — the default here is the heartbeat cadence, matching the JS fallback.
const kiroHeartbeatMs = 10_000

// RetryExec re-issues an upstream request with a (possibly repaired) body and
// returns the response body reader + HTTP status. The integrity gate calls this
// once for the bounded retry; a non-nil error or non-2xx status aborts retry
// handling with kiro_integrity_retry_upstream_error (mirror kiro.js:415-430).
type RetryExec func(ctx context.Context, body json.RawMessage) (bodyReader io.ReadCloser, status int, err error)

// IntegrityResult is the final outcome of one full integrity-gated run (first
// attempt + optional retry): the OpenAI SSE bytes to return to the client and
// the HTTP status to synthesize.
type IntegrityResult struct {
	Bytes  []byte
	Status int
}

// RunIntegrityGate drives the full first-attempt → bounded-retry flow against an
// upstream EventStream body, mirroring runIntegrityRecovery in kiro.js:386-468.
// firstBody is the raw EventStream body of the initial upstream response; retry
// re-executes the upstream with a repaired body for the second attempt. model +
// contextWin configure the per-attempt Transformer. origBody is the original
// client request body (used to inject the repair instruction).
//
// The returned bytes are already OpenAI SSE (clean stream, error SSE, or a
// failure-SSE mapping); the caller wraps them in a synthetic *http.Response.
func RunIntegrityGate(ctx context.Context, firstBody io.ReadCloser, retry RetryExec, model string, contextWin int, origBody json.RawMessage, opts IntegrityOptions) IntegrityResult {
	first := readRecoverableIntegrityAttempt(ctx, firstBody, model, contextWin, opts, "initial")
	switch first.Kind {
	case "complete":
		return IntegrityResult{Bytes: first.Bytes, Status: 200}
	case "terminal_stop", "upstream_error":
		return IntegrityResult{Bytes: integrityFailureSSE(first), Status: 200}
	case "invalid_tool":
		if !opts.RepairEnabled {
			return IntegrityResult{Bytes: encodeSSEError("invalid_kiro_tool_call", first.Message, first.Diagnostics), Status: 200}
		}
	}

	// Repairable kinds: ellipsis / short_final / invalid_tool. Anything else
	// (retryable_stop / missing_terminal) retries with the original body
	// (structuredClone), mirroring kiro.js:432-437.
	var repairBody json.RawMessage
	switch first.Kind {
	case "ellipsis", "short_final", "invalid_tool":
		repairKind := first.Kind
		if repairKind == "invalid_tool" {
			repairKind = "tool"
		}
		repairBody = appendRepairInstruction(origBody, repairKind)
	default:
		repairBody = cloneBody(origBody)
	}

	retryBody, retryStatus, err := retry(ctx, repairBody)
	if err != nil || retryStatus >= 400 {
		// Mirror kiro.js:438-454: drain a prefix of the retry error body and emit
		// kiro_integrity_retry_upstream_error.
		msg := ""
		if retryBody != nil {
			prefix := readPrefix(retryBody, min(opts.MaxBytes, 4096))
			_ = retryBody.Close()
			msg = string(prefix)
		}
		if msg == "" {
			status := retryStatus
			if err != nil {
				status = 502
			}
			msg = fmt.Sprintf("Kiro integrity retry failed with HTTP %d", status)
		}
		return IntegrityResult{Bytes: encodeSSEError("kiro_integrity_retry_upstream_error", msg, map[string]any{"status": retryStatus}), Status: 200}
	}
	if retryBody == nil {
		return IntegrityResult{Bytes: encodeSSEError("kiro_integrity_retry_upstream_error", "Kiro integrity retry returned no body", nil), Status: 200}
	}

	second := readRecoverableIntegrityAttempt(ctx, retryBody, model, contextWin, opts, "retry")
	switch second.Kind {
	case "complete":
		return IntegrityResult{Bytes: second.Bytes, Status: 200}
	case "terminal_stop", "upstream_error":
		return IntegrityResult{Bytes: integrityFailureSSE(second), Status: 200}
	}
	// Retry did not complete → map the kind to its retry-failed code (mirror
	// kiro.js:456-466).
	code := "kiro_missing_terminal_retry_failed"
	switch second.Kind {
	case "ellipsis":
		code = "kiro_ellipsis_retry_failed"
	case "short_final":
		code = "kiro_short_final_retry_failed"
	case "invalid_tool":
		code = "kiro_tool_call_repair_retry_failed"
	}
	msg := fmt.Sprintf("Kiro integrity validation failed after one bounded retry: %s", nonEmpty(second.Message, second.Kind))
	return IntegrityResult{Bytes: encodeSSEError(code, msg, map[string]any{"attempts": []any{first.Diagnostics, second.Diagnostics}}), Status: 200}
}

// readRecoverableIntegrityAttempt wraps readIntegrityAttempt with a
// transport-error fallback, mirroring readRecoverableIntegrityAttempt in
// kiro.js:472-495. An abort (ctx-cancel) re-throws; any other read error becomes
// a missing_terminal attempt result.
func readRecoverableIntegrityAttempt(ctx context.Context, body io.ReadCloser, model string, contextWin int, opts IntegrityOptions, attempt string) attemptResult {
	result, err := readIntegrityAttempt(ctx, body, model, contextWin, opts, attempt)
	if err == nil {
		return result
	}
	if ctxErr := ctx.Err(); ctxErr != nil && (err == ctxErr || strings.Contains(err.Error(), "interrupted") || strings.Contains(err.Error(), "timeout")) {
		// Abort: surface as a terminal incomplete so the caller's retry/abort
		// path handles it. The JS path throws AbortError up to the gate's catch.
		return attemptResult{
			Kind:    "terminal_stop",
			Message: err.Error(),
			Diagnostics: &TerminalState{
				Provenance:      "transport_read_error",
				TransportState:  "upstream_error",
				StopDisposition: "terminal_incomplete",
				ResponseState:   "no_semantic_output",
				EventCounts:     map[string]int{},
			},
		}
	}
	return attemptResult{
		Kind:    "missing_terminal",
		Message: nonEmpty(err.Error(), "Kiro transport read failed"),
		Diagnostics: &TerminalState{
			Provenance:      "transport_read_error",
			TransportState:  "upstream_error",
			StopDisposition: "terminal_incomplete",
			ResponseState:   "no_semantic_output",
			EventCounts:     map[string]int{},
		},
	}
}

// attemptResult is one attempt's classified outcome (mirror the {kind, message,
// diagnostics, bytes} objects returned by readIntegrityAttempt in kiro.js:519-575).
type attemptResult struct {
	Kind        string
	Message     string
	Diagnostics *TerminalState
	Bytes       []byte // OpenAI SSE bytes, only for Kind=="complete"
}

// readIntegrityAttempt reads one EventStream attempt with ttft/stall timeouts
// and a maxBytes bound, classifies the terminal state, and applies the
// ellipsis/short_final heuristics, mirroring readIntegrityAttempt in
// kiro.js:497-575. The body is consumed and closed.
func readIntegrityAttempt(ctx context.Context, body io.ReadCloser, model string, contextWin int, opts IntegrityOptions, attempt string) (attemptResult, error) {
	defer body.Close()

	tr := NewTransformer("chatcmpl-kiro", nowUnixInt(), model, contextWin)
	tr.maxToolBytes = max(1, opts.MaxBytes/2)
	stream := NewFrameStream()

	totalBytes := 0

	readErr := readStreamWithTimeouts(ctx, body, opts.TTFTTimeoutMs, opts.StallTimeoutMs, func(chunk []byte) (bool, error) {
		totalBytes += len(chunk)
		if totalBytes > opts.MaxBytes {
			// Stop reading; the buffer-exceeded branch below maps this to
			// terminal_stop (mirror kiro.js:516-522). Returning (false, nil) ends
			// the read loop without a transport error so readRecoverableIntegrity
			// Attempt does not classify it as missing_terminal.
			return false, nil
		}
		if !tr.ProcessBytes(chunk, stream) {
			// Corrupt frame → the transformer recorded a fail-closed terminal.
			return false, nil
		}
		return true, nil
	})
	if readErr != nil {
		// Abort / timeout propagates as an error so readRecoverableIntegrityAttempt
		// can classify it (mirror the throw in kiro.js:526-530).
		return attemptResult{}, readErr
	}

	// Buffer-exceeded mid-stream → terminal_stop (mirror kiro.js:516-522).
	if totalBytes > opts.MaxBytes {
		return attemptResult{
			Kind:        "terminal_stop",
			Message:     fmt.Sprintf("kiro integrity buffer exceeded %d bytes", opts.MaxBytes),
			Diagnostics: &TerminalState{Provenance: "integrity_buffer_exceeded"},
		}, nil
	}

	// EOF: finalize the transformer and build the safe diagnostics.
	tr.Finish(stream.Remainder())
	diag := tr.Terminal()
	safe := safeDiagnostics(diag, attempt)

	// Inspect the OpenAI SSE the transformer emitted for the ellipsis/short_final
	// heuristics (mirror the JS output.content/reasoning/hasToolCalls, populated
	// per-chunk from the *transformed* body — here, post-Finish from tr.Bytes()).
	output := &sseInspection{}
	inspectSSEChunk(tr.Bytes(), output)

	// Classify the disposition, mirroring kiro.js:536-570.
	switch safe.StopDisposition {
	case "retryable_protocol_failure":
		kind := "retryable_stop"
		if safe.Provenance == "invalid_tool_call" {
			kind = "invalid_tool"
		}
		return attemptResult{Kind: kind, Message: output.error, Diagnostics: safe}, nil
	case "terminal_incomplete", "terminal_refusal", "unknown_failure":
		kind := "missing_terminal"
		switch safe.Provenance {
		case "upstream_eventstream_error":
			kind = "upstream_error"
		case "integrity_buffer_exceeded":
			kind = "terminal_stop"
		case "metadata_stop_reason", "message_stop_event":
			kind = "terminal_stop"
		}
		return attemptResult{Kind: kind, Message: output.error, Diagnostics: safe}, nil
	}
	// Clean disposition, but the SSE stream carried an error event.
	if output.error != "" {
		return attemptResult{Kind: "missing_terminal", Message: output.error, Diagnostics: safe}, nil
	}
	// No tool calls: apply the ellipsis / short_final heuristics.
	if !output.hasToolCalls {
		if isEllipsisOnly(output.content) || (strings.TrimSpace(output.content) == "" && isEllipsisOnly(output.reasoning)) {
			return attemptResult{Kind: "ellipsis", Diagnostics: safe}, nil
		}
		if isShortFutureAction(output.content) {
			return attemptResult{Kind: "short_final", Diagnostics: safe}, nil
		}
	}
	// Complete: the OpenAI SSE bytes are the transformer's accumulated output.
	return attemptResult{Kind: "complete", Bytes: tr.Bytes(), Diagnostics: safe}, nil
}

// readStreamWithTimeouts copies an io.Reader chunk-by-chunk into onChunk, with a
// ttft timeout before the first chunk and a stall timeout between chunks. This
// mirrors readWithTimeout(reader, signal, sawChunk ? stall : ttft, ...) in
// kiro.js:507-515. onChunk returns (keepGoing, err); a false return stops the
// read loop without error (used for the maxBytes + corrupt-frame early exits).
func readStreamWithTimeouts(ctx context.Context, body io.Reader, ttftMs, stallMs int, onChunk func(chunk []byte) (bool, error)) error {
	buf := make([]byte, 32*1024)
	deadline := time.Now().Add(time.Duration(ttftMs) * time.Millisecond)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, err := readChunkWithTimeout(ctx, body, buf, deadline)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		keep, cerr := onChunk(chunk)
		if cerr != nil {
			return cerr
		}
		if !keep {
			return nil
		}
		// Reset the stall deadline after each chunk.
		deadline = time.Now().Add(time.Duration(stallMs) * time.Millisecond)
	}
}

// readChunkWithTimeout reads one chunk from body into buf, bounded by deadline.
// On deadline it returns a timeout error (surfaced via ctx cancel to abort the
// read goroutine). Returns io.EOF when the body is exhausted.
func readChunkWithTimeout(ctx context.Context, body io.Reader, buf []byte, deadline time.Time) ([]byte, error) {
	type readOutcome struct {
		n   int
		err error
	}
	ch := make(chan readOutcome, 1)
	go func() {
		n, err := body.Read(buf)
		ch <- readOutcome{n, err}
	}()
	remaining := time.Until(deadline)
	if remaining < 0 {
		remaining = 0
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.n == 0 && r.err != nil {
			return nil, r.err
		}
		out := make([]byte, r.n)
		copy(out, buf[:r.n])
		return out, r.err
	case <-timer.C:
		// Stall: surface as a timeout error so readRecoverableIntegrityAttempt
		// classifies it as terminal_stop/abort.
		return nil, fmt.Errorf("kiro integrity validation timed out")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// safeDiagnostics coerces a (possibly nil) TerminalState into a non-nil report
// with defaults, mirroring safeDiagnostics in kiro.js:533-545. The diagnostics
// returned by the transformer carry the full EventCounts/Usage; this keeps the
// same shape so the caller's diagnostics are never nil.
func safeDiagnostics(diag *TerminalState, attempt string) *TerminalState {
	if diag == nil {
		return &TerminalState{
			Provenance:      "missing_terminal_diagnostics",
			TransportState:  "unknown",
			StopDisposition: "terminal_incomplete",
			ResponseState:   "no_semantic_output",
			EventCounts:     map[string]int{},
		}
	}
	// The transformer's StopReason is a string (""); the JS diagnostics use
	// null for a missing reason. Keep the string here — consumers treat "" as
	// absent. Ensure EventCounts is never nil for JSON marshal parity.
	if diag.EventCounts == nil {
		diag.EventCounts = map[string]int{}
	}
	return diag
}

// integrityFailureSSE maps a terminal_stop / upstream_error attempt to its
// fail-closed SSE, mirroring integrityFailureSSE in kiro.js:456-468.
func integrityFailureSSE(attempt attemptResult) []byte {
	diag := attempt.Diagnostics
	disposition := ""
	provenance := ""
	if diag != nil {
		disposition = diag.StopDisposition
		provenance = diag.Provenance
	}
	code := "kiro_unknown_stop_reason"
	switch {
	case provenance == "integrity_buffer_exceeded":
		code = "kiro_integrity_buffer_exceeded"
	case attempt.Kind == "upstream_error":
		code = "kiro_upstream_eventstream_error"
	case disposition == "terminal_refusal":
		code = "kiro_terminal_refusal"
	case disposition == "terminal_incomplete":
		code = "kiro_terminal_incomplete"
	}
	return encodeSSEError(code, nonEmpty(attempt.Message, "Kiro stream ended with a terminal failure"), diag)
}

// -----------------------------------------------------------------------------
// Repair instruction injection (mirror kiro.js:126-133 appendRepairInstruction)
// -----------------------------------------------------------------------------

// repairInstructions mirrors REPAIR_INSTRUCTIONS in kiro.js:37-41. The "tool"
// kind is used for invalid_tool; ellipsis / short_final map to their own keys.
var repairInstructions = map[string]string{
	"tool":        "Retry the previous response because its Kiro tool_call wrapper was malformed. If you use the wrapper tool named tool_call, its input must contain a non-empty name and an arguments field.",
	"ellipsis":    "Retry the previous response because it ended with only an ellipsis. Return the complete final answer, not only ... or ….",
	"short_final": "Retry the previous response because its final only announced a future action. Complete the check now and return the result or a concrete blocker.",
}

// appendRepairInstruction clones the request body and appends a repair
// instruction to systemPrompt, mirroring appendRepairInstruction in kiro.js:126-133.
// kind is "tool" / "ellipsis" / "short_final"; an unknown kind falls back to the
// generic instruction (mirror the `||` default).
func appendRepairInstruction(body json.RawMessage, kind string) json.RawMessage {
	repaired := cloneBody(body)
	instruction := repairInstructions[kind]
	if instruction == "" {
		instruction = "Retry the previous incomplete Kiro response."
	}
	var obj map[string]any
	if len(repaired) == 0 {
		obj = map[string]any{"systemPrompt": instruction}
	} else {
		if err := json.Unmarshal(repaired, &obj); err != nil {
			// Non-object body: wrap so the instruction still lands.
			obj = map[string]any{"systemPrompt": instruction}
		}
		if existing, ok := obj["systemPrompt"].(string); ok && existing != "" {
			obj["systemPrompt"] = existing + "\n\n" + instruction
		} else {
			obj["systemPrompt"] = instruction
		}
	}
	out, _ := json.Marshal(obj)
	return out
}

// cloneBody returns a copy of body (or an empty object literal when body is
// nil/empty), mirroring `structuredClone(body || {})` in kiro.js:127.
func cloneBody(body json.RawMessage) json.RawMessage {
	if len(body) == 0 {
		return []byte("{}")
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	return cp
}

// -----------------------------------------------------------------------------
// OpenAI SSE inspection (mirror kiro.js:192-208 inspectSSEChunk)
// -----------------------------------------------------------------------------

// sseInspection accumulates the OpenAI SSE stream's visible content/reasoning/
// toolCalls/error, mirroring the `output` object in readIntegrityAttempt
// (kiro.js:502-504) populated by inspectSSEChunk.
type sseInspection struct {
	content      string
	reasoning    string
	hasToolCalls bool
	error        string
}

// inspectSSEChunk parses the OpenAI SSE chunks the transformer emitted and
// accumulates their content/reasoning/tool_calls/error, mirroring
// inspectSSEChunk in kiro.js:192-208. A malformed SSE line is ignored (the
// transformer's own diagnostics own that failure).
func inspectSSEChunk(chunk []byte, out *sseInspection) {
	text := string(chunk)
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimSpace(line[len("data: "):])
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		if e, ok := event["error"].(map[string]any); ok {
			if m, ok := e["message"].(string); ok {
				out.error = m
			}
		}
		choices, _ := event["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if s, ok := delta["content"].(string); ok {
				out.content += s
			}
			if s, ok := delta["reasoning_content"].(string); ok {
				out.reasoning += s
			}
			if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
				out.hasToolCalls = true
			}
		}
	}
}

// -----------------------------------------------------------------------------
// Heuristics (mirror kiro.js:169-181)
// -----------------------------------------------------------------------------

// isEllipsisOnly reports whether the trimmed value is exactly "..." or "…",
// mirroring isEllipsisOnly in kiro.js:169-171.
func isEllipsisOnly(value string) bool {
	switch strings.TrimSpace(value) {
	case "...", "…":
		return true
	}
	return false
}

// isShortFutureAction detects a final that only announces a future action,
// mirroring isShortFutureAction in kiro.js:173-181.
func isShortFutureAction(value string) bool {
	text := strings.ReplaceAll(strings.TrimSpace(value), "’", "'")
	if observedTrailingFutureAction.MatchString(text) {
		return true
	}
	if englishFutureAction.MatchString(text) && englishResultClause.MatchString(text) {
		return false
	}
	if chineseFutureAction.MatchString(text) && chineseResultClause.MatchString(text) {
		return false
	}
	return len(text) > 0 && len([]rune(text)) <= kiroShortFinalMaxChars &&
		shortFutureAction.MatchString(text) && !userWait.MatchString(text) &&
		!completedFinal.MatchString(text) && !resultEvidence.MatchString(text)
}

// shortFutureAction / observedTrailingFutureAction / englishFutureAction /
// englishResultClause / chineseFutureAction / chineseResultClause / userWait /
// completedFinal / resultEvidence mirror the regex literals in kiro.js:43-52.
// JS flags `iu`/`u` map to Go RE2: `i` → (?i); the `u` (unicode) flag is the
// default in Go (UTF-8). Each pattern is compiled once at init.
var (
	shortFutureAction = regexp.MustCompile(`(?i)^(?:(?:(?:現在|接著|接下來|下一步)[，,:：\s]*(?:我(?:只)?(?:會|要|將|再)?\s*)?|我只再)(?:補|查|確認|驗證|追(?:查|蹤)?|繼續|檢查|測試)|我(?:會|要|將)(?:再|重新)?(?:補(?:齊|查)?|抓取|查(?:詢)?|確認|驗證|追(?:查|蹤)?|繼續|檢查|測試)|(?:(?:next|now|then)\b[\s,:-]*)?(?:i(?:'ll| will| am going to| need to)|let me)\s+(?:verify|check|confirm|validate|investigate|trace|continue|follow up|test)\b)`)

	observedTrailingFutureAction = regexp.MustCompile(`(?i)^目前證據顯示[\s\S]{1,700}[。.!?；;]\s*最後補查\s+504\s+access\s+log[，,]\s*確認\s+host[／/]路徑與是否為集中流量[。.!?]?$`)

	englishFutureAction = regexp.MustCompile(`(?i)^(?:(?:next|now|then)\b[\s,:-]*)?(?:i(?:'ll| will| am going to| need to)|let me)\s+(?:verify|check|confirm|validate|investigate|trace|continue|follow up|test)\b`)

	englishResultClause = regexp.MustCompile(`(?i)(?:[:;\n]|[.!?]\s+\S|\b(?:status|checksum|response|deployment)\s+(?:is|are|was|were|matches?|equals?|returned)\b)`)

	chineseFutureAction = regexp.MustCompile(`^(?:(?:現在|接著|接下來|下一步)[，,:：\s]*(?:我(?:只)?(?:會|要|將|再)?\s*)?|我只再|我(?:會|要|將)(?:再|重新)?)(?:補|抓取|查|確認|驗證|追|繼續|檢查|測試)`)

	chineseResultClause = regexp.MustCompile(`(?:[。！？]\s*\S|(?:版本|狀態|回應|結果|部署|校驗碼)(?:是|為|等於|顯示))`)

	userWait = regexp.MustCompile(`(?i)(?:請(?:你|先)|你(?:先|需要|可以|提供|確認|批准|允許)|等待(?:你|使用者)|等你|核准|同意|授權|\b(?:after|when|once)\s+you\b|\byour\s+(?:approval|confirmation|permission|input)\b|\bwait(?:ing)?\s+for\s+you\b|\bplease\s+(?:approve|confirm|provide|send)\b)`)

	completedFinal = regexp.MustCompile(`(?i)(?:已(?:經)?完成|完成(?:了|驗證|確認)|修復完成|確認無誤|驗證(?:完成|通過)|測試(?:均)?通過|結論|總結|\b(?:done|completed|fixed|verified|confirmed|passed|in conclusion|summary)\b|\b(?:is|are) complete\b)`)

	resultEvidence = regexp.MustCompile(`(?i)(?:顯示|發現|因此|成功|失敗|正常|無錯誤|沒有錯誤|\b(?:found|shows?|showed|because|therefore|succeeded|failed|healthy|green|no errors?)\b)`)
)

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// readPrefix reads at most n bytes from r, best-effort (mirror the
// readResponsePrefix usage in kiro.js:439-444).
func readPrefix(r io.Reader, n int) []byte {
	buf := make([]byte, n)
	read, _ := io.ReadFull(r, buf)
	if read < 0 {
		read = 0
	}
	return buf[:read]
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// nowUnixInt is the per-attempt created timestamp. Wrapped so tests can stub it
// (the package-level cursor nowUnix lives in cursorexec; this is kiroexec-local).
var nowUnixInt = func() int { return int(time.Now().Unix()) }
