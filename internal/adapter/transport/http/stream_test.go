package http

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// slowReader blocks forever, simulating an upstream that never sends data.
type slowReader struct{}

func (slowReader) Read([]byte) (int, error) {
	time.Sleep(time.Hour)
	return 0, nil
}

func TestPipeStallEmitsErrorSSEAndDone(t *testing.T) {
	rec := httptest.NewRecorder()
	w := New(rec, context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := Pipe(ctx, slowReader{}, w, PipeOpts{
		StallTimeout:          50 * time.Millisecond,
		StallTimeoutReasoning: 200 * time.Millisecond,
		Reason:                "test_stall",
	})
	if err != nil {
		t.Fatalf("Pipe returned error: %v", err)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"code":"stream_stall_timeout"`) {
		t.Fatalf("body missing error SSE: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("body missing [DONE] terminator: %q", body)
	}
}

func TestPipeContextCancelEmitsErrorSSEAndDone(t *testing.T) {
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	w := New(rec, ctx)

	pipeDone := make(chan struct{})
	go func() {
		defer close(pipeDone)
		_ = Pipe(ctx, slowReader{}, w, PipeOpts{
			StallTimeout:          5 * time.Second,
			StallTimeoutReasoning: 10 * time.Second,
		})
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-pipeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Pipe did not return after context cancel")
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"code":"client_disconnect"`) {
		t.Fatalf("body missing client_disconnect error SSE: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("body missing [DONE] terminator: %q", body)
	}
}

func TestPipePassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := New(rec, context.Background())

	upstream := strings.NewReader("data: {\"chunk\":1}\n\ndata: {\"chunk\":2}\n\n")

	err := Pipe(context.Background(), upstream, w, PipeOpts{StallTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Pipe returned error: %v", err)
	}

	body := rec.Body.String()
	want := "data: {\"chunk\":1}\n\ndata: {\"chunk\":2}\n\n"
	if body != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

func TestPipeIsThinkingModelUsesReasoningTimeout(t *testing.T) {
	rec := httptest.NewRecorder()
	w := New(rec, context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- Pipe(context.Background(), slowReader{}, w, PipeOpts{
			StallTimeout:          50 * time.Millisecond,
			StallTimeoutReasoning: 200 * time.Millisecond,
			IsThinkingModel:       true,
		})
	}()

	select {
	case err := <-errCh:
		t.Fatalf("Pipe returned too early: %v", err)
	case <-time.After(120 * time.Millisecond):
		// expected: reasoning timeout (200ms) has not fired yet
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Pipe returned error: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Pipe did not return within reasoning timeout")
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"code":"stream_stall_timeout"`) {
		t.Fatalf("body missing error SSE: %q", body)
	}
}
