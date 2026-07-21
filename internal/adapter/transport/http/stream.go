// Package http implements the SSE stream pipe used by the /v1 chat pipeline.
// It mirrors open-sse/utils/streamHandler.js: stall detection resets on each
// successful read, thinking models use a longer reasoning timeout, and any
// client disconnect or stall emits the fork-patch error SSE + [DONE].
package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

// PipeOpts configures the SSE pipe and is consumed by Pipe.
type PipeOpts struct {
	// StallTimeout is the maximum idle time before the upstream is considered
	// stalled. It is reset on every successful read from upstream.
	StallTimeout time.Duration
	// StallTimeoutReasoning is the stall timeout for thinking/reasoning models.
	// It is used when IsThinkingModel is true.
	StallTimeoutReasoning time.Duration
	// IsThinkingModel selects the longer reasoning stall timeout.
	IsThinkingModel bool
	// Reason is the error reason string included in the terminal SSE event.
	Reason string
	// Provider and Model are logged with debug diagnostics.
	Provider string
	Model    string

	// FrameMode selects how upstream frames are split. "sse" (default, the
	// historical behaviour) splits on "\n\n" — the OpenAI/Claude standard.
	// "ndjson" splits on "\n" — raw JSON lines without a "data:" prefix, as
	// emitted by Ollama/llama.cpp /api/chat. "auto" detects per upstream: if
	// the first non-empty frame has no "\n\n" but is a JSON line, it switches
	// to ndjson. Empty/unset keeps "sse" for backwards compatibility.
	FrameMode string

	// TranslateResponse, when non-nil, converts a raw upstream frame (already
	// de-framed) into one or more OpenAI-SSE client frames. It mirrors the JS
	// streamHelpers parseSSELine + response-translator pipeline: ollama NDJSON
	// lines are translated to OpenAI chat.completion.chunk SSE events. When
	// nil the pipe does byte-for-byte raw passthrough (the historical
	// behaviour and what TestPipePassthrough asserts).
	TranslateResponse func(frame []byte, state map[string]any) ([][]byte, error)

	// EmitEventPrefix writes the Anthropic SSE "event: <type>\n" line before
	// each "data: <json>\n\n" frame, where <type> is the chunk's "type" field.
	// The Anthropic streaming format requires the event line; without it the
	// official Claude SDK (Claude Code) rejects the response as malformed
	// ("API returned an empty or malformed response"). OpenAI streaming uses
	// only "data:", so this is set only when the client source format is Claude.
	// Mirrors legacy streamHelpers.js formatSSE (sourceFormat === CLAUDE).
	EmitEventPrefix bool
}

// DefaultReason is the reason string used in the terminal error SSE.
const DefaultReason = "stream_disconnected"

// terminalErrorEvent returns the JSON bytes for the fork-patch error SSE.
// It matches streamHandler.js exactly:
//
//	{"error": {"message": "Stream aborted: <reason>", "type": "stream_error", "code": "<reason>"}}
func terminalErrorEvent(reason string) []byte {
	if reason == "" {
		reason = DefaultReason
	}
	payload := map[string]any{
		"error": map[string]any{
			"message": "Stream aborted: " + reason,
			"type":    "stream_error",
			"code":    reason,
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

// Pipe reads raw SSE frames from upstream and writes them to the client SSE
// writer. It watches the request context for cancellation and uses a stall
// timer reset on every successful frame read. If the context is cancelled or
// the stall timer fires, it writes a single structured error SSE followed by
// data: [DONE] and returns.
func Pipe(ctx context.Context, upstream io.Reader, w *Writer, opts PipeOpts) error {
	if opts.StallTimeout <= 0 {
		opts.StallTimeout = 120 * time.Second
	}
	if opts.StallTimeoutReasoning <= 0 {
		opts.StallTimeoutReasoning = 300 * time.Second
	}

	stallTimeout := opts.StallTimeout
	if opts.IsThinkingModel {
		stallTimeout = opts.StallTimeoutReasoning
	}

	reason := opts.Reason
	if reason == "" {
		reason = DefaultReason
	}

	framer := newFramer(upstream, 1<<20, opts.FrameMode)

	stall := time.NewTimer(stallTimeout)
	defer stall.Stop()

	// frameCh carries frames read from upstream to the writer goroutine.
	frameCh := make(chan []byte, 1)
	// resetCh is signaled by the writer after each successful frame write.
	resetCh := make(chan struct{}, 1)
	// errCh carries terminal scanner/writer errors back to the main loop.
	errCh := make(chan error, 1)
	// eofCh is closed when the upstream reader reaches EOF (no error).
	eofCh := make(chan struct{})

	var closeFrameChOnce sync.Once
	closeFrameCh := func() { closeFrameChOnce.Do(func() { close(frameCh) }) }

	go func() {
		for {
			frame, err := framer.NextFrame()
			if err != nil {
				// A cancelled/expired upstream context (the fetch context that
				// owns resp.Body, or the request context on client disconnect)
				// surfaces as context.Canceled / DeadlineExceeded from
				// framer.Read. Treat it as end-of-stream, not a hard error: the
				// main loop would otherwise block on the stall timer until it
				// fires (the ollama NDJSON 90s hang when the fetch context was
				// cancelled prematurely). Close eofCh so the main loop returns
				// promptly without emitting a spurious error SSE.
				if err == io.EOF || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					close(eofCh)
				} else {
					errCh <- err
				}
				closeFrameCh()
				return
			}
			if frame == nil {
				close(eofCh)
				closeFrameCh()
				return
			}
			frameCh <- frame
		}
	}()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		// Per-stream translation state (mirrors translator.InitState on the
		// JS side). Initialized lazily on the first translated frame.
		var state map[string]any
		for frame := range frameCh {
			out, err := translateOrPassthrough(w, opts.TranslateResponse, opts.EmitEventPrefix, &state, frame)
			if err != nil {
				errCh <- err
				closeFrameCh()
				return
			}
			for _, ev := range out {
				if err := w.WriteRaw(ev); err != nil {
					errCh <- err
					closeFrameCh()
					return
				}
			}
			select {
			case resetCh <- struct{}{}:
			default:
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			reason = contextReason(ctx.Err())
			closeFrameCh()
			waitDone(writerDone, 100*time.Millisecond)
			emitTerminal(w, reason)
			return nil
		case <-stall.C:
			reason = "stream_stall_timeout"
			closeFrameCh()
			waitDone(writerDone, 100*time.Millisecond)
			emitTerminal(w, reason)
			return nil
		case <-eofCh:
			closeFrameCh()
			waitDone(writerDone, 100*time.Millisecond)
			return nil
		case err := <-errCh:
			closeFrameCh()
			waitDone(writerDone, 100*time.Millisecond)
			emitTerminal(w, reason)
			return err
		case <-resetCh:
			if !stall.Stop() {
				select {
				case <-stall.C:
				default:
				}
			}
			stall.Reset(stallTimeout)
		}
	}
}

// emitTerminal writes the fork-patch terminal error SSE + [DONE]. It bypasses
// the SSE writer's context check because the client may have disconnected and
// we still want to enqueue the terminal bytes best-effort (matching the JS
// controller.enqueue behavior in streamHandler.js).
func emitTerminal(w *Writer, reason string) {
	writeRawNoCtx(w, terminalErrorEvent(reason))
	writeRawNoCtx(w, []byte("[DONE]"))
}

// writeRawNoCtx writes an unnamed SSE event directly to the underlying
// ResponseWriter and flushes, bypassing the context check. This mirrors the
// JS terminal enqueue and is safe because both stream.go and sse.go live in
// package http.
func writeRawNoCtx(w *Writer, data []byte) {
	var buf bytes.Buffer
	for _, line := range bytes.Split(data, []byte("\n")) {
		buf.Write([]byte("data: "))
		buf.Write(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	_, _ = io.Copy(w.w, &buf)
	if w.f != nil {
		w.f.Flush()
	}
}

func waitDone(doneCh <-chan struct{}, timeout time.Duration) {
	select {
	case <-doneCh:
	case <-time.After(timeout):
	}
}

func contextReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "stream_deadline_exceeded"
	}
	return "client_disconnect"
}

// frameReader reads SSE frames separated by "\n\n" from an io.Reader.
type frameReader struct {
	r       io.Reader
	buf     []byte
	maxSize int
	err     error
}

func newFrameReader(r io.Reader, maxSize int) *frameReader {
	return &frameReader{
		r:       r,
		buf:     make([]byte, 0, 64*1024),
		maxSize: maxSize,
	}
}

// NextFrame returns the next SSE frame (without the trailing "\n\n") or io.EOF.
func (fr *frameReader) NextFrame() ([]byte, error) {
	for {
		if fr.err != nil {
			return nil, fr.err
		}
		if idx := findDoubleNewline(fr.buf); idx >= 0 {
			frame := make([]byte, idx)
			copy(frame, fr.buf[:idx])
			fr.buf = fr.buf[idx+2:]
			return frame, nil
		}
		if len(fr.buf) > fr.maxSize {
			fr.err = errors.New("frame exceeds maximum size")
			return nil, fr.err
		}
		tmp := make([]byte, 64*1024)
		n, err := fr.r.Read(tmp)
		if n > 0 {
			fr.buf = append(fr.buf, tmp[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(fr.buf) == 0 {
					fr.err = io.EOF
					return nil, io.EOF
				}
				frame := make([]byte, len(fr.buf))
				copy(frame, fr.buf)
				fr.buf = fr.buf[:0]
				fr.err = io.EOF
				return frame, nil
			}
			fr.err = err
			return nil, err
		}
	}
}

func findDoubleNewline(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == '\n' && b[i+1] == '\n' {
			return i
		}
	}
	return -1
}

// framer is the de-framing strategy used by Pipe. The SSE framer splits on
// "\n\n"; the NDJSON framer splits on "\n" (raw JSON lines, Ollama/llama.cpp).
type framer interface {
	NextFrame() ([]byte, error)
}

// newFramer selects a de-framer by mode. "ndjson" forces line splitting;
// "sse" or empty keeps the historical double-newline behaviour; "auto"
// starts as SSE and, if a frame looks like a bare JSON line (no "\n\n"
// boundary found but a complete "{...}\n" is present), re-reads it as
// NDJSON.
func newFramer(r io.Reader, maxSize int, mode string) framer {
	switch strings.ToLower(mode) {
	case "ndjson":
		return &ndjsonFramer{r: r, maxSize: maxSize}
	case "auto":
		return &autoFramer{sse: &frameReader{r: r, buf: make([]byte, 0, 64*1024), maxSize: maxSize}, nd: newNDJSONFramer(r, maxSize)}
	default:
		return &frameReader{r: r, buf: make([]byte, 0, 64*1024), maxSize: maxSize}
	}
}

// ndjsonFramer reads raw JSON lines separated by single "\n". Blank lines
// are skipped. A trailing line without "\n" is flushed on EOF.
type ndjsonFramer struct {
	r       io.Reader
	buf     []byte
	maxSize int
	err     error
}

func newNDJSONFramer(r io.Reader, maxSize int) *ndjsonFramer {
	return &ndjsonFramer{r: r, maxSize: maxSize}
}

func (fr *ndjsonFramer) NextFrame() ([]byte, error) {
	for {
		if fr.err != nil {
			return nil, fr.err
		}
		if idx := bytes.IndexByte(fr.buf, '\n'); idx >= 0 {
			line := make([]byte, idx)
			copy(line, fr.buf[:idx])
			fr.buf = fr.buf[idx+1:]
			if trimmed := bytes.TrimSpace(line); len(trimmed) == 0 {
				continue
			}
			return line, nil
		}
		if len(fr.buf) > fr.maxSize {
			fr.err = errors.New("frame exceeds maximum size")
			return nil, fr.err
		}
		tmp := make([]byte, 64*1024)
		n, err := fr.r.Read(tmp)
		if n > 0 {
			fr.buf = append(fr.buf, tmp[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(bytes.TrimSpace(fr.buf)) == 0 {
					fr.err = io.EOF
					return nil, io.EOF
				}
				line := make([]byte, len(fr.buf))
				copy(line, fr.buf)
				fr.buf = fr.buf[:0]
				fr.err = io.EOF
				return line, nil
			}
			fr.err = err
			return nil, err
		}
	}
}

// autoFramer starts as SSE; the first frame that yields via the SSE path but
// contains no "data:" prefix (i.e. a bare JSON line) switches the stream to
// NDJSON for all subsequent reads. This mirrors streamHelpers.js auto-detect
// of raw JSON lines from ollama/llama.cpp.
type autoFramer struct {
	sse *frameReader
	nd  *ndjsonFramer
}

func (a *autoFramer) NextFrame() ([]byte, error) {
	frame, err := a.sse.NextFrame()
	if err == nil {
		// SSE split on "\n\n". If it is a bare JSON object (no "data:" prefix),
		// the upstream is NDJSON: feed the remainder through the ndjson framer
		// and return this line as-is.
		trimmed := bytes.TrimSpace(frame)
		if len(trimmed) > 0 && trimmed[0] == '{' && !bytes.HasPrefix(trimmed, []byte("data:")) {
			a.nd.buf = append(a.nd.buf, a.sse.buf...)
			a.sse.buf = a.sse.buf[:0]
			a.nd.err = nil
			return frame, nil
		}
		return frame, nil
	}
	// EOF on SSE with leftover bytes that never saw "\n\n": treat as the last
	// NDJSON line(s).
	if errors.Is(err, io.EOF) && len(bytes.TrimSpace(a.sse.buf)) > 0 {
		leftover := make([]byte, len(a.sse.buf))
		copy(leftover, a.sse.buf)
		a.sse.buf = a.sse.buf[:0]
		return leftover, nil
	}
	return nil, err
}

// translateOrPassthrough maps one de-framed upstream frame into client SSE
// frames. With no translator (raw passthrough) the frame is re-terminated
// with "\n\n" exactly as the historical Pipe did. With a translator each
// produced chunk is wrapped as an OpenAI "data: <json>\n\n" event.
func translateOrPassthrough(
	w *Writer,
	translate func([]byte, map[string]any) ([][]byte, error),
	emitEventPrefix bool,
	state *map[string]any,
	frame []byte,
) ([][]byte, error) {
	if translate == nil {
		out := make([]byte, len(frame))
		copy(out, frame)
		out = append(out, '\n', '\n')
		return [][]byte{out}, nil
	}
	if *state == nil {
		*state = map[string]any{}
	}
	chunks, err := translate(frame, *state)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(chunks))
	for _, c := range chunks {
		if len(bytes.TrimSpace(c)) == 0 {
			continue
		}
		// Anthropic streaming requires "event: <type>\ndata: <json>\n\n"; OpenAI
		// uses only "data:". When EmitEventPrefix is set (Claude source format),
		// extract the chunk's "type" field and prepend the event line. Mirrors
		// legacy streamHelpers.js formatSSE (sourceFormat === CLAUDE).
		ev := make([]byte, 0, len(c)+24)
		if emitEventPrefix {
			if eventType := extractEventType(c); len(eventType) > 0 {
				ev = append(ev, []byte("event: ")...)
				ev = append(ev, eventType...)
				ev = append(ev, '\n')
			}
		}
		ev = append(ev, []byte("data: ")...)
		ev = append(ev, c...)
		ev = append(ev, '\n', '\n')
		out = append(out, ev)
	}
	if len(out) == 0 {
		// Translator dropped the chunk (e.g. empty content delta). Emit nothing
		// but keep the writer alive.
		return nil, nil
	}
	return out, nil
}

// extractEventType reads the "type" field from a JSON SSE chunk. Returns "" if
// the chunk is not a JSON object or has no "type" field, in which case no
// "event:" line is emitted (OpenAI-style frame falls back to "data:" only).
func extractEventType(chunk []byte) []byte {
	// Fast path: a streaming chunk always starts with "{" and the "type" field
	// is conventionally near the start. Scan a bounded prefix rather than
	// unmarshalling the whole object on every delta.
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	// Unmarshal is simplest and correct; chunks are small (one delta each).
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(chunk, &probe); err != nil {
		return nil
	}
	if probe.Type == "" {
		return nil
	}
	return []byte(probe.Type)
}
