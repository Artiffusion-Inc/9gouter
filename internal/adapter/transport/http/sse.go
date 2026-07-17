package http

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// Writer emits Server-Sent Events over an http.ResponseWriter that supports
// http.Flusher. It observes ctx.Done() and stops writing when the client
// disconnects.
type Writer struct {
	w   http.ResponseWriter
	f   http.Flusher
	ctx context.Context
}

// New creates an SSE writer. It sets the standard SSE response headers.
func New(w http.ResponseWriter, ctx context.Context) *Writer {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	f, _ := w.(http.Flusher)
	return &Writer{w: w, f: f, ctx: ctx}
}

// WriteEvent writes a single SSE event frame. data may contain newlines; each
// line is emitted with its own "data: " prefix. Empty event name writes a frame
// without an event: line.
func (s *Writer) WriteEvent(event string, data []byte) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}

	var buf bytes.Buffer
	if event != "" {
		fmt.Fprintf(&buf, "event: %s\n", event)
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		buf.Write([]byte("data: "))
		buf.Write(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')

	if _, err := io.Copy(s.w, &buf); err != nil {
		return err
	}
	if s.f != nil {
		s.f.Flush()
	}
	return nil
}

// Flush flushes the underlying writer if it supports http.Flusher.
func (s *Writer) Flush() {
	if s.f != nil {
		s.f.Flush()
	}
}
