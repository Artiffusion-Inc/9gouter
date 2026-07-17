package http

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSEWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	w := New(rec, context.Background())

	eventData := []byte("hello\nworld")
	if err := w.WriteEvent("message", eventData); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	wantHeaders := map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	}
	for h, want := range wantHeaders {
		if got := rec.Header().Get(h); got != want {
			t.Errorf("header %q = %q, want %q", h, got, want)
		}
	}

	body := rec.Body.String()
	wantBody := "event: message\ndata: hello\ndata: world\n\n"
	if body != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}
}

func TestSSEWriterEmptyEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	w := New(rec, context.Background())
	if err := w.WriteEvent("", []byte("ping")); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	if got, want := rec.Body.String(), "data: ping\n\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestSSEWriterContextCancel(t *testing.T) {
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := New(rec, ctx)

	err := w.WriteEvent("message", []byte("data"))
	if err == nil {
		t.Fatal("expected error after context cancel")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("expected context canceled error, got %v", err)
	}
}

func TestSSEWriterFlushCalled(t *testing.T) {
	// httptest.ResponseRecorder is not a Flusher; use a real HTTP handler to
	// verify flush happens on a flushable writer.
	var body bytes.Buffer
	done := make(chan struct{})
	handler := func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		se := New(w, r.Context())
		_ = se.WriteEvent("message", []byte("flush me"))
		body.WriteString("ok")
	}

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest("GET", "/", nil))
	<-done

	if got := rec.Body.String(); !strings.Contains(got, "flush me") {
		t.Errorf("response body missing event: %q", got)
	}
}

func TestSSEWriterMultilineData(t *testing.T) {
	rec := httptest.NewRecorder()
	w := New(rec, context.Background())
	data := []byte("line1\nline2\nline3")
	if err := w.WriteEvent("chunk", data); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	want := "event: chunk\ndata: line1\ndata: line2\ndata: line3\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func BenchmarkSSEWriterWriteEvent(b *testing.B) {
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		w := New(rec, context.Background())
		_ = w.WriteEvent("message", []byte(fmt.Sprintf("event %d", i)))
	}
}
