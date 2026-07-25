package imageproxy

// poll_test.go covers the polling lifecycle helper (step 4 point 12): pending
// → completed, terminal failed, poll non-2xx, cancellation, timeout, foreign
// host rejection, validated target metadata per hop. All HTTP scenarios use
// httptest.Server; no mock HTTP client.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newPollHandler builds a Handler with a short poll interval + timeout for
// tests so we do not sleep the production 120s.
func newPollHandler(exec HTTPExecutor) *Handler {
	return New(Dependencies{
		Executor:     exec,
		Logger:       captureLogger{},
		PollInterval: 10 * time.Millisecond,
		PollTimeout:  500 * time.Millisecond,
	})
}

// pollFactory builds a GET request to pollURL with the given provider header.
func pollFactory(provider string) PollRequestFactory {
	return func(ctx context.Context, pollURL string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-9gouter-provider", provider)
		return req, nil
	}
}

// parserOf returns a parser that maps the given body string to a PollStatus.
func parserOf(status PollStatus) PollStatusParser {
	return func(body []byte) (PollStatus, error) {
		switch string(body) {
		case "pending":
			return PollPending, nil
		case "completed":
			return PollCompleted, nil
		case "failed":
			return PollFailed, nil
		case "malformed":
			return PollMalformed, nil
		}
		return PollMalformed, errors.New("unknown body: " + string(body))
	}
}

// httptestExecutor wraps an httptest.Server's client as an HTTPExecutor that
// surfaces 3xx without following (the server client does follow by default;
// for poll tests we return the raw 2xx body the server writes).
type httptestExecutor struct {
	client *http.Client
}

func (e *httptestExecutor) Do(req *http.Request) (*http.Response, error) {
	return e.client.Do(req)
}

func TestPoll_PendingThenCompleted(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			_, _ = io.WriteString(w, "pending")
			return
		}
		_, _ = io.WriteString(w, "completed")
	}))
	defer srv.Close()
	h := newPollHandler(&httptestExecutor{client: srv.Client()})
	res, err := h.poll(context.Background(), srv.URL+"/status/abc", pollFactory("fal-ai"), parserOf(PollPending))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Status != PollCompleted {
		t.Errorf("status = %q, want completed", res.Status)
	}
	if atomic.LoadInt32(&attempts) < 2 {
		t.Errorf("attempts = %d, want >=2", attempts)
	}
}

func TestPoll_TerminalFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "failed")
	}))
	defer srv.Close()
	h := newPollHandler(&httptestExecutor{client: srv.Client()})
	_, err := h.poll(context.Background(), srv.URL+"/status/abc", pollFactory("fal-ai"), parserOf(PollFailed))
	if err == nil {
		t.Fatal("want error for terminal failed")
	}
	pe, ok := err.(*pollError)
	if !ok || pe.HTTPStatus() != http.StatusBadGateway {
		t.Errorf("err = %v, want 502 pollError", err)
	}
}

func TestPoll_PollMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "malformed")
	}))
	defer srv.Close()
	h := newPollHandler(&httptestExecutor{client: srv.Client()})
	_, err := h.poll(context.Background(), srv.URL+"/status/abc", pollFactory("fal-ai"), parserOf(PollMalformed))
	if err == nil {
		t.Fatal("want error for malformed state")
	}
	pe, ok := err.(*pollError)
	if !ok || pe.HTTPStatus() != http.StatusBadGateway {
		t.Errorf("err = %v, want 502", err)
	}
}

func TestPoll_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	defer srv.Close()
	h := newPollHandler(&httptestExecutor{client: srv.Client()})
	_, err := h.poll(context.Background(), srv.URL+"/status/abc", pollFactory("fal-ai"), parserOf(PollPending))
	if err == nil {
		t.Fatal("want error for non-2xx poll")
	}
	pe, ok := err.(*pollError)
	if !ok || pe.HTTPStatus() != http.StatusBadGateway {
		t.Errorf("err = %v, want 502", err)
	}
}

func TestPoll_Cancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "pending")
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := newPollHandler(&httptestExecutor{client: srv.Client()})
	_, err := h.poll(ctx, srv.URL+"/status/abc", pollFactory("fal-ai"), parserOf(PollPending))
	if err == nil {
		t.Fatal("want error for cancelled context")
	}
	// Cancellation must NOT be a false success.
	if errors.Is(err, ErrPollTimeout) {
		t.Errorf("cancellation must not surface as timeout")
	}
}

func TestPoll_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "pending")
	}))
	defer srv.Close()
	h := newPollHandler(&httptestExecutor{client: srv.Client()})
	_, err := h.poll(context.Background(), srv.URL+"/status/abc", pollFactory("fal-ai"), parserOf(PollPending))
	if err == nil {
		t.Fatal("want error for poll timeout")
	}
	if !errors.Is(err, ErrPollTimeout) {
		t.Errorf("err = %v, want ErrPollTimeout", err)
	}
}

// TestPoll_ForeignHostRejection proves the factory can validate the poll URL
// against a provider allowlist before issuing the request. The factory
// returns an error for a foreign host, which the helper surfaces as 502. The
// poll URL is a synthetic HTTPS foreign-host URL — the factory rejects it
// before any HTTP call, so no server is needed.
func TestPoll_ForeignHostRejection(t *testing.T) {
	h := newPollHandler(&recordingExecutor{})
	factory := func(ctx context.Context, pollURL string) (*http.Request, error) {
		u, err := validateLifecycleURL(pollURL, FalHostPredicate)
		if err != nil {
			return nil, err
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		req.Header.Set("x-9gouter-provider", "fal-ai")
		return req, nil
	}
	_, err := h.poll(context.Background(), "https://evil.example.com/status/abc", factory, parserOf(PollPending))
	if err == nil {
		t.Fatal("want error for foreign poll host")
	}
	pe, ok := err.(*pollError)
	if !ok || pe.HTTPStatus() != http.StatusBadGateway {
		t.Errorf("err = %v, want 502 pollError", err)
	}
	if !strings.Contains(err.Error(), "unexpected lifecycle host") {
		t.Errorf("err = %v, want unexpected host message", err)
	}
}

// TestPoll_ValidatedTargetMetadataPerHop proves the factory can attach a
// ValidatedHost to the request context (via WithTransportMetadata) and the
// helper's h.do forwards it to the executor. The recording executor asserts
// the validated host reached it on every poll attempt.
func TestPoll_ValidatedTargetMetadataPerHop(t *testing.T) {
	rec := &pollRecordingExecutor{}
	h := newPollHandler(rec)
	var attempts int32
	factory := func(ctx context.Context, pollURL string) (*http.Request, error) {
		n := atomic.AddInt32(&attempts, 1)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://queue.fal.run/status/abc", nil)
		req.Header.Set("x-9gouter-provider", "fal-ai")
		// Attach a validated host so h.do carries it through.
		req = req.WithContext(WithTransportMetadata(req.Context(), TransportMetadata{
			ProviderID: "fal-ai",
			Phase:      "poll",
			ValidatedHost: ValidatedHost{
				Scheme: "https", Hostname: "queue.fal.run", Port: "443",
				IP: []byte{93, 184, 216, 34},
			},
		}))
		if n >= 2 {
			rec.body = "completed"
		} else {
			rec.body = "pending"
		}
		return req, nil
	}
	_, err := h.poll(context.Background(), "https://queue.fal.run/status/abc", factory, parserOf(PollPending))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(rec.hosts) < 2 {
		t.Fatalf("expected >=2 poll attempts, got %d", len(rec.hosts))
	}
	for i, vh := range rec.hosts {
		if vh.Hostname != "queue.fal.run" {
			t.Errorf("attempt %d: host = %q, want queue.fal.run", i, vh.Hostname)
		}
		if !vh.IP.Equal([]byte{93, 184, 216, 34}) {
			t.Errorf("attempt %d: IP = %v", i, vh.IP)
		}
	}
}

// pollRecordingExecutor records the ValidatedHost from each request's
// transport metadata and returns a body that the test sets per-attempt.
type pollRecordingExecutor struct {
	hosts []ValidatedHost
	body  string
}

func (r *pollRecordingExecutor) Do(req *http.Request) (*http.Response, error) {
	if meta, ok := TransportMetadataFromContext(req.Context()); ok {
		r.hosts = append(r.hosts, meta.ValidatedHost)
	}
	body := r.body
	if body == "" {
		body = "pending"
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
	}, nil
}
