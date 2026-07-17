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

	framer := newFrameReader(upstream, 1<<20)

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
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && err != io.EOF {
					errCh <- err
				}
				if err == io.EOF {
					close(eofCh)
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
		for frame := range frameCh {
			// Ensure each frame is terminated by the SSE blank line. The
			// frameReader strips the trailing "\n\n"; add it back here.
			frame = append(frame, '\n', '\n')
			if err := w.WriteRaw(frame); err != nil {
				errCh <- err
				closeFrameCh()
				return
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
