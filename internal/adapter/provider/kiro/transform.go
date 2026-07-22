// transform.go ports the event-dispatch + terminal-state half of
// transformEventStreamToSSE in open-sse/executors/kiro.js (upstream v0.5.40,
// commit 6994cd1f). Where eventstream.go owns the binary codec (CRC + framing
// + header decode), this file owns the per-event translation into OpenAI SSE
// chunks and the terminal state machine that decides finish_reason or a
// fail-closed error SSE.
//
// The integrity gate + bounded retry (the attachIntegrityGate /
// runIntegrityRecovery / readRecoverableIntegrityAttempt half of kiro.js) is
// ported separately in integrity.go (#102); it consumes this transformer's
// terminal diagnostics. This file does NOT do retry — it produces one
// attempt's worth of OpenAI SSE + a terminal-state report, and the caller
// decides whether to retry.
package kiroexec

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Kiro repair / protocol bounds (mirror kiro.js:11-14).
const (
	kiroRepairBufferMaxBytes = 8 * 1024 * 1024
	kiroShortFinalMaxChars   = 800
)

// kiroEventTypes is the set of :event-type values the transformer counts
// explicitly; everything else rolls up to "other" (mirror kiro.js:15-27).
var kiroEventTypes = map[string]bool{
	"assistantResponseEvent": true,
	"reasoningContentEvent":  true,
	"codeEvent":              true,
	"toolUseEvent":           true,
	"messageStopEvent":       true,
	"metadataEvent":          true,
	"MetadataEvent":          true,
	"contextUsageEvent":      true,
	"meteringEvent":          true,
	"metricsEvent":           true,
}

// sseDone is the terminal [DONE] sentinel the OpenAI SSE stream closes with.
const sseDone = "data: [DONE]\n\n"

// Transformer converts one Kiro EventStream attempt into OpenAI SSE bytes plus a
// terminal-state report. It mirrors the closure-captured `state` + `processEvent`
// + `processBytes` + `finish` + `fail` in kiro.js:578-1075, but as a struct so
// the integrity gate (#102) can drive one attempt, read the terminal report,
// and decide whether to retry.
//
// The transformer is single-attempt and NOT safe for concurrent use: one
// attempt owns one Transformer.
type Transformer struct {
	responseID string
	created    int
	model      string
	contextWin int

	// Accumulated state (mirror the `state` object in kiro.js:583-608).
	chunkIndex          int
	toolCounter         int
	tools               map[string]*kiroTool
	bufferedToolBytes   int
	hasText             bool
	hasReasoning        bool
	hasCode             bool
	hasToolCalls        bool
	sawToolUse          bool
	explicitStop        bool
	stopReason          string
	terminalProvenance  string
	transportState      string
	totalContentLength  int
	contextUsagePct     float64
	hasContextUsage     bool
	hasMetering         bool
	usage               map[string]any
	inThinking          bool
	toolValidationError string
	validatedFrames     int
	finished            bool

	// maxToolBytes bounds the buffered tool-call assembly (mirror
	// KIRO_REPAIR_BUFFER_MAX_BYTES / 2 default in kiro.js:649).
	maxToolBytes int

	// out accumulates the OpenAI SSE bytes for this attempt.
	out []byte

	// eventCounts mirrors the diagnostics event_counts map.
	eventCounts map[string]int

	// terminal is set by fail() / finish() and read by the integrity gate to
	// decide retry. nil while the attempt is still in progress.
	terminal *TerminalState
}

// kiroTool is the buffered tool-call assembly state (mirror the `tool` objects
// in kiro.js:666-700).
type kiroTool struct {
	id        string
	name      string
	inputKind string // "string" | "object"
	// inputChunks accumulates string fragments; joined + JSON-parsed on emit.
	inputChunks []string
	inputObject map[string]any
	inputBytes  int
}

// TerminalState is the fail-closed terminal report one attempt leaves behind
// (mirror the `diagnostics(...)` object + the code/message fail carries). The
// integrity gate reads `.Code` and `.Provenance` to decide whether to retry.
type TerminalState struct {
	Provenance      string         // terminal_provenance
	TransportState  string         // transport_state
	StopReason      string         // stop_reason
	StopDisposition string         // stop_disposition
	ResponseState   string         // response_state
	EventCounts     map[string]int // event_counts
	IncompleteBytes int            // incomplete_frame_bytes
	Code            string         // SSE error code ("" when the attempt completed cleanly)
	Message         string         // SSE error message ("" when clean)
	Usage           map[string]any // usage reported/synthesized (nil if none)
}

// NewTransformer returns a Transformer for one attempt. responseID/created are
// the OpenAI chunk id + Unix-second timestamp; contextWin is the model context
// window used to synthesize usage when only metering + context-usage arrived.
func NewTransformer(responseID string, created int, model string, contextWin int) *Transformer {
	return &Transformer{
		responseID:     responseID,
		created:        created,
		model:          model,
		contextWin:     contextWin,
		tools:          make(map[string]*kiroTool),
		transportState: "consuming_response",
		maxToolBytes:   kiroRepairBufferMaxBytes / 2,
		eventCounts:    make(map[string]int),
	}
}

// ProcessBytes feeds a raw EventStream chunk through the framing loop, mirroring
// processBytes in kiro.js:852-916. It returns false (and sets a terminal state)
// on a corrupt frame / over-bound buffer, telling the caller to stop reading.
// A false return with a nil terminal means the buffer is mid-frame and the
// caller should keep feeding bytes — that case never happens here because the
// loop only returns false on hard failure; mid-frame returns true to wait.
func (t *Transformer) ProcessBytes(chunk []byte, frames *FrameStream) bool {
	if err := frames.Push(chunk); err != nil {
		t.fail("corrupt_eventstream_frame", "kiro_missing_terminal", err.Error(), map[string]string{"transport_state": "corrupt_frame"})
		return false
	}
	for {
		frame, err := frames.Next()
		if err != nil {
			t.fail("corrupt_eventstream_frame", "kiro_missing_terminal", err.Error(), map[string]string{"transport_state": "corrupt_frame"})
			return false
		}
		if frame.EventType() == "" && len(frame.Headers) == 0 && frame.Payload == nil {
			// No complete frame buffered yet — wait for more bytes.
			return true
		}
		chunksBefore := t.chunkIndex
		framesBefore := t.validatedFrames
		t.transportState = "valid_complete_frame"
		t.validatedFrames++
		if !t.processEvent(frame) {
			return false
		}
		// A validated frame that produced no SSE chunk gets a heartbeat comment,
		// mirroring the kiro-upstream heartbeat in kiro.js:1044-1047.
		if t.validatedFrames > framesBefore && t.chunkIndex == chunksBefore {
			t.out = append(t.out, []byte(": kiro-upstream\n\n")...)
		}
	}
}

// processEvent dispatches one decoded frame to the per-event-type handler,
// mirroring processEvent in kiro.js:718-840. Returns false when the event is a
// terminal upstream error (the caller stops reading); true to continue.
func (t *Transformer) processEvent(frame EventFrame) bool {
	messageType := frame.MessageType()
	if messageType == "error" || messageType == "exception" {
		msg := "Kiro upstream sent an EventStream " + messageType
		if frame.Payload != nil {
			if m, ok := frame.Payload["message"].(string); ok && m != "" {
				msg = m
			}
		}
		t.fail("upstream_eventstream_error", "kiro_upstream_eventstream_error", msg, map[string]string{"transport_state": "upstream_error"})
		return false
	}

	eventType := frame.EventType()
	countKey := eventType
	if !kiroEventTypes[eventType] {
		countKey = "other"
	}
	t.eventCounts[countKey]++

	switch eventType {
	case "assistantResponseEvent":
		t.handleAssistantResponse(frame)
	case "reasoningContentEvent":
		t.handleReasoning(frame)
	case "codeEvent":
		t.handleCode(frame)
	case "toolUseEvent":
		if !t.handleToolUse(frame) {
			return true // tool-validation error: swallowed, continue (mirror kiro.js:824-831)
		}
	case "messageStopEvent":
		t.handleMessageStop(frame)
	case "metadataEvent", "MetadataEvent":
		t.handleMetadata(frame)
	case "contextUsageEvent":
		t.handleContextUsage(frame)
	case "meteringEvent":
		t.handleMetering(frame)
	case "metricsEvent":
		t.handleMetrics(frame)
	}
	return true
}

// handleAssistantResponse emits a text delta, stripping <thinking> blocks the
// way kiro.js:727-749 does. Content inside an unclosed <thinking> is dropped;
// the closing </thinking> re-enables emission and strips a leading newline.
func (t *Transformer) handleAssistantResponse(frame EventFrame) {
	content, _ := frame.Payload["content"].(string)
	if t.inThinking {
		end := strings.Index(content, "</thinking>")
		if end < 0 {
			content = ""
		} else {
			t.inThinking = false
			content = content[end+11:]
			content = strings.TrimPrefix(content, "\n")
		}
	} else {
		start := strings.Index(content, "<thinking>")
		if start >= 0 {
			end := strings.Index(content[start+10:], "</thinking>")
			if end < 0 {
				t.inThinking = true
				content = content[:start]
			} else {
				realEnd := start + 10 + end + 11
				content = content[:start] + strings.TrimPrefix(content[realEnd:], "\n")
			}
		}
	}
	if content != "" || !t.hasReasoning {
		if content != "" {
			t.hasText = true
		}
		t.totalContentLength += len(content)
		t.emitDelta(map[string]any{"content": content})
	}
}

// handleReasoning emits a reasoning_content delta (mirror kiro.js:751-759).
func (t *Transformer) handleReasoning(frame EventFrame) {
	var content string
	if v, ok := frame.Payload["reasoningContentEvent"]; ok {
		if s, ok := v.(string); ok {
			content = s
		} else if m, ok := v.(map[string]any); ok {
			if s, ok := m["text"].(string); ok {
				content = s
			} else if s, ok := m["content"].(string); ok {
				content = s
			}
		}
	} else if s, ok := frame.Payload["text"].(string); ok {
		content = s
	} else if s, ok := frame.Payload["content"].(string); ok {
		content = s
	}
	if content != "" {
		t.hasReasoning = true
		t.totalContentLength += len(content)
		t.emitDelta(map[string]any{"reasoning_content": content})
	}
}

// handleCode emits a code content delta (mirror kiro.js:761-765).
func (t *Transformer) handleCode(frame EventFrame) {
	content, _ := frame.Payload["content"].(string)
	t.hasCode = true
	t.totalContentLength += len(content)
	t.emitDelta(map[string]any{"content": content})
}

// handleToolUse buffers a tool-call fragment, mirroring kiro.js:767-810. Returns
// false on a tool-validation error (the caller swallows it into
// toolValidationError and continues — see processEvent). The tool is NOT
// emitted here; emitTools at finish time flushes the assembled calls.
func (t *Transformer) handleToolUse(frame EventFrame) bool {
	if t.toolValidationError != "" {
		return false
	}
	t.sawToolUse = true
	// The JS path checks `Array.isArray(event.payload)` and iterates; a
	// JSON-decoded payload is always an object (ParseEventFrame unmarshals into
	// map[string]any), so the array branch never triggers here — the payload is
	// a single object. Mirror the values[0] empty guard from kiro.js:776.
	values := []map[string]any{frame.Payload}
	if values[0] == nil {
		t.toolValidationError = "Kiro toolUseEvent is empty"
		t.tools = make(map[string]*kiroTool)
		t.bufferedToolBytes = 0
		return false
	}
	for _, value := range values {
		name, _ := value["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			t.toolValidationError = "Kiro toolUseEvent is missing a tool name"
			t.tools = make(map[string]*kiroTool)
			t.bufferedToolBytes = 0
			return false
		}
		var id string
		if toolUseID, exists := value["toolUseId"]; !exists || toolUseID == nil {
			id = fmt.Sprintf("call_%d_%d", t.created, len(t.tools)+1)
		} else if s, ok := toolUseID.(string); ok && strings.TrimSpace(s) != "" {
			id = s
		} else {
			t.toolValidationError = "Kiro toolUseEvent has an invalid toolUseId"
			t.tools = make(map[string]*kiroTool)
			t.bufferedToolBytes = 0
			return false
		}
		tool, exists := t.tools[id]
		if !exists {
			tool = &kiroTool{id: id, name: name}
			t.tools[id] = tool
			t.bufferedToolBytes += len(id) + len(name) + 32
			if !t.assertToolBufferBound() {
				return false
			}
		} else if tool.name != name {
			t.toolValidationError = "Kiro tool name changed between fragments"
			t.tools = make(map[string]*kiroTool)
			t.bufferedToolBytes = 0
			return false
		}
		if !t.appendToolInput(tool, value["input"]) {
			return false
		}
	}
	return true
}

// appendToolInput appends one input fragment to a buffered tool, mirroring
// appendToolInput in kiro.js:664-689. Returns false on a validation error or
// the buffer-exceeded integrity failure (the latter sets a terminal state via
// fail(), not toolValidationError).
func (t *Transformer) appendToolInput(tool *kiroTool, input any) bool {
	if input == nil {
		return true
	}
	switch v := input.(type) {
	case string:
		if tool.inputKind != "" && tool.inputKind != "string" {
			t.toolValidationError = "Kiro tool input changed fragment type"
			t.tools = make(map[string]*kiroTool)
			t.bufferedToolBytes = 0
			return false
		}
		tool.inputKind = "string"
		tool.inputChunks = append(tool.inputChunks, v)
		t.bufferedToolBytes += len(v)
	case map[string]any:
		if tool.inputKind != "" && tool.inputKind != "object" {
			t.toolValidationError = "Kiro tool input changed fragment type"
			t.tools = make(map[string]*kiroTool)
			t.bufferedToolBytes = 0
			return false
		}
		tool.inputKind = "object"
		t.bufferedToolBytes -= tool.inputBytes
		tool.inputObject = v
		raw, _ := json.Marshal(v)
		tool.inputBytes = len(raw)
		t.bufferedToolBytes += tool.inputBytes
	default:
		t.toolValidationError = "Kiro tool input must be a JSON object"
		t.tools = make(map[string]*kiroTool)
		t.bufferedToolBytes = 0
		return false
	}
	return t.assertToolBufferBound()
}

// assertToolBufferBound enforces the integrity memory bound on buffered tool
// input, mirroring assertToolBufferBound in kiro.js:647-656. On overflow it
// fails the attempt with integrity_buffer_exceeded (a terminal, non-retryable
// failure) and returns false.
func (t *Transformer) assertToolBufferBound() bool {
	if t.bufferedToolBytes <= t.maxToolBytes {
		return true
	}
	t.fail("integrity_buffer_exceeded", "kiro_integrity_buffer_exceeded",
		"Kiro buffered tool input exceeded the integrity memory bound",
		map[string]string{"transport_state": t.transportState, "stop_disposition": "terminal_incomplete"})
	return false
}

// handleMessageStop records the explicit stop reason (mirror kiro.js:812-820).
func (t *Transformer) handleMessageStop(frame EventFrame) {
	t.explicitStop = true
	var raw any
	if r, ok := frame.Payload["stopReason"]; ok {
		raw = r
	} else if r, ok := frame.Payload["stop_reason"]; ok {
		raw = r
	}
	reason := normalizeStopReason(raw)
	if reason == "" {
		if t.sawToolUse {
			reason = "tool_use"
		} else {
			reason = "end_turn"
		}
	}
	merged := mergeStopReason(t.stopReason, reason)
	if merged != t.stopReason {
		t.terminalProvenance = "message_stop_event"
	}
	t.stopReason = merged
}

// handleMetadata records a stop reason carried in metadata (mirror kiro.js:822-832).
func (t *Transformer) handleMetadata(frame EventFrame) {
	var metadata map[string]any
	if v, ok := frame.Payload["metadataEvent"].(map[string]any); ok {
		metadata = v
	} else if v, ok := frame.Payload["metadata"].(map[string]any); ok {
		metadata = v
	} else {
		metadata = frame.Payload
	}
	var raw any
	if r, ok := metadata["stopReason"]; ok {
		raw = r
	} else if r, ok := metadata["stop_reason"]; ok {
		raw = r
	}
	reason := normalizeStopReason(raw)
	if reason != "" {
		t.explicitStop = true
		merged := mergeStopReason(t.stopReason, reason)
		if merged != t.stopReason {
			t.terminalProvenance = "metadata_stop_reason"
		}
		t.stopReason = merged
	}
}

// handleContextUsage records the context-usage percentage (mirror kiro.js:834-840).
func (t *Transformer) handleContextUsage(frame EventFrame) {
	pct, ok := toFloat(frame.Payload["contextUsagePercentage"])
	if ok {
		t.contextUsagePct = pct
		t.hasContextUsage = true
	}
}

// handleMetering records kiro credit usage (mirror kiro.js:842-852).
func (t *Transformer) handleMetering(frame EventFrame) {
	t.hasMetering = true
	var metering map[string]any
	if v, ok := frame.Payload["meteringEvent"].(map[string]any); ok {
		metering = v
	} else {
		metering = frame.Payload
	}
	credits, ok := toFloat(metering["usage"])
	if !ok {
		return
	}
	if t.usage == nil {
		t.usage = make(map[string]any)
	}
	t.usage["kiro_credits"] = credits
	unit := "credit"
	if u, ok := metering["unit"].(string); ok && u != "" {
		unit = u
	}
	t.usage["kiro_credit_unit"] = unit
}

// handleMetrics records token usage from a metrics event (mirror kiro.js:854-870).
func (t *Transformer) handleMetrics(frame EventFrame) {
	var metrics map[string]any
	if v, ok := frame.Payload["metricsEvent"].(map[string]any); ok {
		metrics = v
	} else {
		metrics = frame.Payload
	}
	prompt, _ := toFloat(metrics["inputTokens"])
	completion, _ := toFloat(metrics["outputTokens"])
	if prompt == 0 && completion == 0 {
		return
	}
	if t.usage == nil {
		t.usage = make(map[string]any)
	}
	t.usage["prompt_tokens"] = int(prompt)
	t.usage["completion_tokens"] = int(completion)
	t.usage["total_tokens"] = int(prompt + completion)
	// cacheReadInputTokens || cache_read_input_tokens — camelCase wins when truthy,
	// else snake_case (mirror kiro.js:866-867).
	if cacheRead := firstPositiveFloat(metrics["cacheReadInputTokens"], metrics["cache_read_input_tokens"]); cacheRead > 0 {
		t.usage["cache_read_input_tokens"] = int(cacheRead)
	}
	if cacheCreate := firstPositiveFloat(metrics["cacheCreationInputTokens"], metrics["cache_creation_input_tokens"]); cacheCreate > 0 {
		t.usage["cache_creation_input_tokens"] = int(cacheCreate)
	}
}

// firstPositiveFloat returns the first of a/b that decodes to a finite number > 0,
// mirroring the JS `Number(a || b) || 0` idiom used for camelCase||snake_case
// metrics fields (kiro.js:866-867). 0 when neither is a positive number.
func firstPositiveFloat(a, b any) float64 {
	if f, ok := toFloat(a); ok && f > 0 {
		return f
	}
	if f, ok := toFloat(b); ok && f > 0 {
		return f
	}
	return 0
}

// emitDelta appends one OpenAI chat.completion.chunk SSE line. The first chunk
// carries role:assistant (mirror emitDelta in kiro.js:627-631).
func (t *Transformer) emitDelta(delta map[string]any) {
	if t.chunkIndex == 0 {
		merged := map[string]any{"role": "assistant"}
		for k, v := range delta {
			merged[k] = v
		}
		delta = merged
	}
	t.chunkIndex++
	t.out = append(t.out, t.sseChunk(delta, nil, nil)...)
}

// sseChunk renders one OpenAI chat.completion.chunk SSE line, mirroring sseChunk
// in kiro.js:633-641.
func (t *Transformer) sseChunk(delta map[string]any, finishReason any, usage map[string]any) []byte {
	chunk := map[string]any{
		"id":      t.responseID,
		"object":  "chat.completion.chunk",
		"created": t.created,
		"model":   t.model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	raw, _ := json.Marshal(chunk)
	return append([]byte("data: "), append(raw, '\n', '\n')...)
}

// fail records a fail-closed terminal state and appends an error SSE event +
// [DONE], mirroring fail() in kiro.js:643-656. extra overrides diagnostics
// fields (transport_state / stop_disposition / terminal_provenance).
func (t *Transformer) fail(provenance, code, message string, extra map[string]string) {
	t.finished = true
	t.terminalProvenance = provenance
	if ts, ok := extra["transport_state"]; ok {
		t.transportState = ts
	} else {
		t.transportState = "corrupt_frame"
	}
	stopDisposition := "terminal_incomplete"
	if sd, ok := extra["stop_disposition"]; ok {
		stopDisposition = sd
	}
	t.terminal = t.diagnostics(map[string]string{
		"terminal_provenance": provenance,
		"stop_disposition":    stopDisposition,
	})
	t.terminal.Code = code
	t.terminal.Message = message
	t.out = append(t.out, encodeSSEError(code, message, t.terminal)...)
}

// diagnostics builds the terminal report, mirroring diagnostics() in
// kiro.js:619-635. overrides replace default fields.
func (t *Transformer) diagnostics(overrides map[string]string) *TerminalState {
	provenance := t.terminalProvenance
	if p, ok := overrides["terminal_provenance"]; ok && p != "" {
		provenance = p
	}
	if provenance == "" {
		provenance = "clean_eventstream_eof"
	}
	transport := t.transportState
	if ts, ok := overrides["transport_state"]; ok && ts != "" {
		transport = ts
	}
	stopDisp := stopDisposition(t.stopReason, t.hasToolCalls)
	if sd, ok := overrides["stop_disposition"]; ok && sd != "" {
		stopDisp = sd
	}
	var responseState string
	switch {
	case t.hasToolCalls:
		responseState = "valid_tool"
	case t.hasText || t.hasReasoning || t.hasCode:
		responseState = "text_reasoning"
	case t.explicitStop:
		responseState = "explicit_stop"
	default:
		responseState = "no_semantic_output"
	}
	counts := make(map[string]int, len(t.eventCounts))
	for k, v := range t.eventCounts {
		counts[k] = v
	}
	return &TerminalState{
		Provenance:      provenance,
		TransportState:  transport,
		StopReason:      t.stopReason,
		StopDisposition: stopDisp,
		ResponseState:   responseState,
		EventCounts:     counts,
		IncompleteBytes: 0, // set by Finish on a truncated frame
		Usage:           t.usage,
	}
}

// Finish is the terminal validation at EOF, mirroring finish() in kiro.js:922-1075.
// It is idempotent (guarded by state.finished). It either emits a clean finish
// chunk + [DONE] and a clean TerminalState (Code=""), or fails with the
// appropriate kiro_* error code.
func (t *Transformer) Finish(remainder []byte) {
	if t.finished {
		return
	}
	if len(remainder) > 0 {
		t.fail("incomplete_eventstream_frame", "kiro_missing_terminal",
			"Kiro EventStream ended with a truncated frame",
			map[string]string{"transport_state": "incomplete_frame"})
		if t.terminal != nil {
			t.terminal.IncompleteBytes = len(remainder)
		}
		return
	}
	t.transportState = "clean_eof"
	declaredDisposition := stopDisposition(t.stopReason, t.sawToolUse)
	switch declaredDisposition {
	case "retryable_protocol_failure", "terminal_incomplete", "terminal_refusal", "unknown_failure":
		code := stopDispositionErrorCode(declaredDisposition)
		t.fail(t.terminalProvenance, code,
			fmt.Sprintf("Kiro ended with non-success stop reason: %s", t.stopReason),
			map[string]string{"transport_state": t.transportState, "stop_disposition": declaredDisposition})
		// fail overwrote provenance with the empty t.terminalProvenance; the JS
		// uses `state.terminalProvenance || "metadata_stop_reason"`.
		if t.terminal != nil && t.terminalProvenance == "" {
			t.terminal.Provenance = "metadata_stop_reason"
		}
		return
	}
	if t.toolValidationError != "" {
		t.fail("invalid_tool_call", "invalid_kiro_tool_call", t.toolValidationError,
			map[string]string{"transport_state": t.transportState, "stop_disposition": "retryable_protocol_failure"})
		return
	}
	if !t.emitTools() {
		t.fail("invalid_tool_call", "invalid_kiro_tool_call", t.toolValidationError,
			map[string]string{"transport_state": t.transportState, "stop_disposition": "retryable_protocol_failure"})
		return
	}
	hasOutput := t.hasText || t.hasReasoning || t.hasCode || t.hasToolCalls
	if !hasOutput && !t.explicitStop {
		t.fail("empty_response_eof", "kiro_missing_terminal",
			"Kiro EventStream ended without model output",
			map[string]string{"transport_state": t.transportState})
		return
	}
	disposition := stopDisposition(t.stopReason, t.hasToolCalls)
	switch disposition {
	case "retryable_protocol_failure", "terminal_incomplete", "terminal_refusal", "unknown_failure":
		code := stopDispositionErrorCode(disposition)
		t.fail(t.terminalProvenance, code,
			fmt.Sprintf("Kiro ended with non-success stop reason: %s", t.stopReason),
			map[string]string{"transport_state": t.transportState, "stop_disposition": disposition})
		if t.terminal != nil && t.terminalProvenance == "" {
			t.terminal.Provenance = "metadata_stop_reason"
		}
		return
	}
	// Synthesize usage from metering + context-usage when the upstream did not
	// report token counts (mirror kiro.js:1055-1067).
	if t.hasMetering && t.hasContextUsage && (t.usage == nil || t.usage["total_tokens"] == nil) {
		completion := 0
		if t.totalContentLength > 0 {
			completion = t.totalContentLength / 4
			if completion < 1 {
				completion = 1
			}
		}
		prompt := int(t.contextUsagePct * float64(t.contextWin) / 100)
		if t.usage == nil {
			t.usage = make(map[string]any)
		}
		t.usage["prompt_tokens"] = prompt
		t.usage["completion_tokens"] = completion
		t.usage["total_tokens"] = prompt + completion
	}
	finishReason := "stop"
	switch {
	case t.hasToolCalls:
		finishReason = "tool_calls"
	case disposition == "length":
		finishReason = "length"
	}
	t.out = append(t.out, t.sseChunk(map[string]any{}, finishReason, t.usage)...)
	t.out = append(t.out, []byte(sseDone)...)
	t.finished = true
	t.terminal = t.diagnostics(map[string]string{
		"terminal_provenance": t.terminalProvenance,
		"transport_state":     t.transportState,
		"stop_disposition":    disposition,
	})
}

// emitTools flushes the assembled tool calls as OpenAI tool_calls deltas,
// mirroring emitTools in kiro.js:692-716. Returns false (and sets
// toolValidationError) on an invalid assembled call; the caller fails the
// attempt.
func (t *Transformer) emitTools() bool {
	for _, tool := range t.tools {
		input, ok := t.parsedToolInput(tool)
		if !ok {
			return false
		}
		if tool.name == "tool_call" {
			name, _ := input["name"].(string)
			if strings.TrimSpace(name) == "" {
				t.toolValidationError = "Invalid Kiro tool_call payload: missing nested MCP tool name"
				t.tools = make(map[string]*kiroTool)
				t.bufferedToolBytes = 0
				return false
			}
			if _, exists := input["arguments"]; !exists {
				t.toolValidationError = "Invalid Kiro tool_call payload: missing nested MCP tool arguments"
				t.tools = make(map[string]*kiroTool)
				t.bufferedToolBytes = 0
				return false
			}
		}
		index := t.toolCounter
		t.toolCounter++
		t.emitDelta(map[string]any{
			"tool_calls": []any{
				map[string]any{
					"index":    index,
					"id":       tool.id,
					"type":     "function",
					"function": map[string]any{"name": tool.name, "arguments": ""},
				},
			},
		})
		argsJSON, _ := json.Marshal(input)
		t.emitDelta(map[string]any{
			"tool_calls": []any{
				map[string]any{
					"index":    index,
					"function": map[string]any{"arguments": string(argsJSON)},
				},
			},
		})
		t.hasToolCalls = true
	}
	t.tools = make(map[string]*kiroTool)
	t.bufferedToolBytes = 0
	if t.stopReason == "tool_use" && !t.hasToolCalls {
		t.toolValidationError = "Kiro tool_use stop reason did not include a complete tool call"
		return false
	}
	return true
}

// parsedToolInput resolves a buffered tool's input to an object, mirroring
// parsedToolInput in kiro.js:702-713. Returns ok=false (and sets
// toolValidationError) on missing/invalid input.
func (t *Transformer) parsedToolInput(tool *kiroTool) (map[string]any, bool) {
	if tool.inputKind == "" {
		t.toolValidationError = "Kiro tool call is missing input"
		return nil, false
	}
	if tool.inputKind == "object" {
		return tool.inputObject, true
	}
	joined := strings.Join(tool.inputChunks, "")
	var input map[string]any
	if err := json.Unmarshal([]byte(joined), &input); err != nil {
		t.toolValidationError = fmt.Sprintf("Kiro tool input must be valid object JSON (%v)", err)
		return nil, false
	}
	if input == nil {
		t.toolValidationError = "Kiro tool input must be valid object JSON (not an object)"
		return nil, false
	}
	return input, true
}

// Bytes returns the accumulated OpenAI SSE bytes for this attempt. The
// integrity gate drains this after the attempt completes (clean or failed).
func (t *Transformer) Bytes() []byte { return t.out }

// Terminal returns the terminal-state report (nil while the attempt is still
// in progress). Set by fail() or Finish().
func (t *Transformer) Terminal() *TerminalState { return t.terminal }

// Failed reports whether the attempt failed (a fail-closed error SSE was
// emitted) vs completed cleanly.
func (t *Transformer) Failed() bool { return t.terminal != nil && t.terminal.Code != "" }

// -----------------------------------------------------------------------------
// Stop-reason taxonomy (mirror kiro.js:135-167)
// -----------------------------------------------------------------------------

// stopReasonSep matches the camelCase boundaries and whitespace/dash separators
// normalizeStopReason collapses to underscores.
var stopReasonSep = regexp.MustCompile(`([a-z])([A-Z])`)

// normalizeStopReason canonicalizes a Kiro stop reason, mirroring
// normalizeStopReason in kiro.js:135-141. Returns "" for a missing/blank reason.
func normalizeStopReason(value any) string {
	reason := strings.TrimSpace(fmt.Sprint(value))
	reason = stopReasonSep.ReplaceAllString(reason, "${1}_${2}")
	reason = strings.ToLower(reason)
	reason = strings.ReplaceAll(reason, " ", "_")
	reason = strings.ReplaceAll(reason, "-", "_")
	reason = strings.ReplaceAll(reason, "\t", "_")
	switch reason {
	case "endturn", "end_turn", "stop", "stop_sequence":
		return "end_turn"
	case "tooluse", "tool_use", "tool_calls":
		return "tool_use"
	case "maxtokens", "max_tokens", "max_output_tokens", "length":
		return "max_tokens"
	}
	if reason == "" {
		return ""
	}
	return reason
}

// stopDisposition classifies a stop reason into a terminal disposition,
// mirroring stopDisposition in kiro.js:143-152.
func stopDisposition(stopReason string, hasToolCalls bool) string {
	switch stopReason {
	case "malformed_model_output", "invalid_model_output":
		return "retryable_protocol_failure"
	case "cancelled", "pause_turn", "model_context_window_exceeded":
		return "terminal_incomplete"
	}
	if stopReason == "refusal" || refusalPattern.MatchString(stopReason) {
		return "terminal_refusal"
	}
	if stopReason == "max_tokens" {
		if hasToolCalls {
			return "terminal_incomplete"
		}
		return "length"
	}
	if stopReason != "" && stopReason != "end_turn" && stopReason != "tool_use" {
		return "unknown_failure"
	}
	if hasToolCalls || stopReason == "tool_use" {
		return "tool_use"
	}
	if stopReason == "" || stopReason == "end_turn" {
		return "complete"
	}
	return "unknown_failure"
}

// refusalPattern mirrors the /(?:content.*filter|guardrail|safety|policy|blocked)/u
// test in stopDisposition (kiro.js:149).
var refusalPattern = regexp.MustCompile(`(?i)(?:content.*filter|guardrail|safety|policy|blocked)`)

// mergeStopReason keeps the more severe of two stop reasons, mirroring
// mergeStopReason in kiro.js:154-167. An empty incoming is ignored; an empty
// current is replaced.
func mergeStopReason(current, incoming string) string {
	if incoming == "" {
		return current
	}
	if current == "" {
		return incoming
	}
	if stopSeverity(incoming) > stopSeverity(current) {
		return incoming
	}
	return current
}

// stopSeverity ranks a stop reason by its disposition severity (mirror the
// severity() closure in kiro.js:158-166): refusal 6 > terminal_incomplete 5 >
// unknown_failure 4 > retryable_protocol_failure 3 > length 2 > else 1.
func stopSeverity(reason string) int {
	switch stopDisposition(reason, false) {
	case "terminal_refusal":
		return 6
	case "terminal_incomplete":
		return 5
	case "unknown_failure":
		return 4
	case "retryable_protocol_failure":
		return 3
	case "length":
		return 2
	default:
		return 1
	}
}

// stopDispositionErrorCode maps a non-success disposition to its SSE error code,
// mirroring the ternary in kiro.js:929-939 and 1069-1079.
func stopDispositionErrorCode(disposition string) string {
	switch disposition {
	case "retryable_protocol_failure":
		return "kiro_retryable_protocol_failure"
	case "terminal_refusal":
		return "kiro_terminal_refusal"
	case "terminal_incomplete":
		return "kiro_terminal_incomplete"
	default:
		return "kiro_unknown_stop_reason"
	}
}

// -----------------------------------------------------------------------------
// SSE error encoding (mirror kiro.js:183-190 encodeSSEError)
// -----------------------------------------------------------------------------

// encodeSSEError renders an OpenAI-style error SSE event + [DONE], mirroring
// encodeSSEError in kiro.js:183-190 (which always closes with data: [DONE]).
// details is surfaced as error.details; it may be a *TerminalState (per-attempt
// diagnostics) or an arbitrary map (the retry path's {status}/{attempts}).
func encodeSSEError(code, message string, details any) []byte {
	errObj := map[string]any{
		"message": message,
		"type":    "upstream_error",
		"code":    code,
	}
	if details != nil {
		errObj["details"] = details
	}
	raw, _ := json.Marshal(map[string]any{"error": errObj})
	out := append([]byte("data: "), append(raw, '\n', '\n')...)
	out = append(out, []byte(sseDone)...)
	return out
}

// -----------------------------------------------------------------------------
// numeric coercion (mirror the JS Number() + Number.isFinite guards)
// -----------------------------------------------------------------------------

// toFloat extracts a finite float from a JSON-decoded number. Returns ok=false
// for non-numbers and non-finite values (NaN/Inf), matching Number.isFinite.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		if isNaNOrInf(n) {
			return 0, false
		}
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil || isNaNOrInf(f) {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func isNaNOrInf(f float64) bool {
	return f != f || f > 1e308 || f < -1e308
}
