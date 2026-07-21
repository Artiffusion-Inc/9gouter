package http

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
)

// capturingHandler is a test slog.Handler that records the attributes of every
// record it handles, so tests can assert that "request_id" is present. It also
// tracks attrs bound via WithAttrs (slog.Logger.With pre-binds attrs to the
// handler, not the record — the handler must surface them on the captured
// record). The records slice is shared via a pointer so WithAttrs clones
// append to the same log the test inspects.
type capturingHandler struct {
	records *[]slog.Record
	attrs   []slog.Attr
}

func newCapturingHandler() *capturingHandler {
	records := []slog.Record{}
	return &capturingHandler{records: &records}
}

func (h *capturingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	// Materialize a record that includes both the handler-bound attrs (from
	// Logger.With) and the per-call attrs so tests can inspect a single flat
	// attribute list via recordAttr.
	clone := r.Clone()
	for _, a := range h.attrs {
		clone.AddAttrs(a)
	}
	*h.records = append(*h.records, clone)
	return nil
}

func (h *capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cp := *h
	cp.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &cp
}
func (h *capturingHandler) WithGroup(_ string) slog.Handler { return h }

func (h *capturingHandler) Records() []slog.Record { return *h.records }

// recordAttr returns the string value of the named attribute on r, or "" if
// absent.
func recordAttr(r slog.Record, key string) string {
	var out string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out = a.Value.String()
			return false
		}
		return true
	})
	return out
}

func TestRequestIDMiddleware_PropagatesIncomingHeader(t *testing.T) {
	const incoming = "abc-123-correlate"
	var ctxID string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxID = RequestIDFromContext(r.Context())
	})

	chain := Chain(RequestIDMiddleware())
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderRequestID, incoming)
	rec := httptest.NewRecorder()
	chain(handler).ServeHTTP(rec, req)

	if ctxID != incoming {
		t.Errorf("context id = %q, want %q (header must propagate unchanged)", ctxID, incoming)
	}
	if got := rec.Header().Get(HeaderRequestID); got != incoming {
		t.Errorf("response header = %q, want %q", got, incoming)
	}
}

func TestRequestIDMiddleware_GeneratesWhenAbsent(t *testing.T) {
	var ctxID string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxID = RequestIDFromContext(r.Context())
	})

	chain := Chain(RequestIDMiddleware())
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	chain(handler).ServeHTTP(rec, req)

	if ctxID == "" {
		t.Fatal("context id = empty, want a generated id")
	}
	// uuidv4 string is 36 chars with hyphens; the crypto/rand fallback is 32
	// hex chars. Either is acceptable — just assert sane length and non-empty.
	if len(ctxID) < 32 {
		t.Errorf("generated id length = %d, want >= 32 (uuid v4 = 36, hex fallback = 32)", len(ctxID))
	}
	if got := rec.Header().Get(HeaderRequestID); got != ctxID {
		t.Errorf("response header = %q, want %q (must match context id)", got, ctxID)
	}
}

func TestRequestIDMiddleware_GeneratesDifferentIDs(t *testing.T) {
	chain := Chain(RequestIDMiddleware())
	ids := make(map[string]struct{}, 2)
	for range 2 {
		var id string
		handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			id = RequestIDFromContext(r.Context())
		})
		req := httptest.NewRequest("GET", "/", nil)
		chain(handler).ServeHTTP(httptest.NewRecorder(), req)
		ids[id] = struct{}{}
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 distinct generated IDs, got %d (collision or stale state)", len(ids))
	}
}

func TestRequestIDMiddleware_RejectsEmptyHeader(t *testing.T) {
	var ctxID string
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctxID = RequestIDFromContext(r.Context())
	})

	chain := Chain(RequestIDMiddleware())
	req := httptest.NewRequest("GET", "/", nil)
	// Explicitly empty header — middleware should treat as absent and generate.
	req.Header.Set(HeaderRequestID, "")
	rec := httptest.NewRecorder()
	chain(handler).ServeHTTP(rec, req)

	if ctxID == "" {
		t.Fatal("empty X-Request-Id should trigger generation, not stay empty")
	}
}

func TestRequestIDFromContext_EmptyWhenAbsent(t *testing.T) {
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("RequestIDFromContext(empty ctx) = %q, want %q", got, "")
	}
}

func TestWithRequestID_RoundTrip(t *testing.T) {
	const id = "rt-xyz"
	ctx := WithRequestID(context.Background(), id)
	if got := RequestIDFromContext(ctx); got != id {
		t.Errorf("round-trip = %q, want %q", got, id)
	}
}

func TestWithRequestID_EmptyNoOp(t *testing.T) {
	ctx := WithRequestID(context.Background(), "")
	if got := RequestIDFromContext(ctx); got != "" {
		t.Errorf("WithRequestID(empty) should be a no-op, got %q", got)
	}
}

func TestLoggerFromContext_CarriesRequestID(t *testing.T) {
	cap := newCapturingHandler()
	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(slog.New(cap))

	const id = "log-correlate-1"
	ctx := WithRequestID(context.Background(), id)
	logger := LoggerFromContext(ctx)
	logger.Info("hello")

	if len(cap.Records()) != 1 {
		t.Fatalf("expected 1 captured record, got %d", len(cap.Records()))
	}
	if got := recordAttr(cap.Records()[0], "request_id"); got != id {
		t.Errorf("record request_id = %q, want %q", got, id)
	}
}

func TestLoggerFromContext_NoIDReturnsDefault(t *testing.T) {
	cap := newCapturingHandler()
	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(slog.New(cap))

	logger := LoggerFromContext(context.Background())
	if logger != slog.Default() {
		t.Errorf("expected default logger when no request id in ctx")
	}
	logger.Info("background")
	if len(cap.Records()) != 1 {
		t.Errorf("expected 1 record on default handler, got %d", len(cap.Records()))
	}
	if got := recordAttr(cap.Records()[0], "request_id"); got != "" {
		t.Errorf("background record should not carry request_id, got %q", got)
	}
}

// TestLogMiddleware_IncludesRequestID is an end-to-end check that the composed
// chain RequestID -> Log emits a log record carrying the request_id attribute.
func TestLogMiddleware_IncludesRequestID(t *testing.T) {
	cap := newCapturingHandler()
	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(slog.New(cap))

	// Use the default logger explicitly so LogMiddleware's fallback path also
	// routes through `cap` when context lookup returns the default.
	chain := Chain(
		RequestIDMiddleware(),
		LogMiddleware(slog.New(cap)),
	)

	req := httptest.NewRequest("GET", "/v1/chat", nil)
	req.Header.Set(HeaderRequestID, "e2e-rid")
	rec := httptest.NewRecorder()
	chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)

	if got := rec.Header().Get(HeaderRequestID); got != "e2e-rid" {
		t.Errorf("response header = %q, want %q", got, "e2e-rid")
	}
	// Find a record that came from LogMiddleware (has "request" message +
	// method/path attrs). Capturing the With-bound logger's records: the
	// request_id attr is pre-bound so it appears on every record.
	var found bool
	for _, r := range cap.Records() {
		if !strings.Contains(r.Message, "request") {
			continue
		}
		if got := recordAttr(r, "request_id"); got != "e2e-rid" {
			t.Errorf("log record request_id = %q, want %q (msg=%q)", got, "e2e-rid", r.Message)
		}
		found = true
	}
	if !found {
		t.Fatalf("no 'request' log record captured; got %d records", len(cap.Records()))
	}
}

// TestNewServer_RequestIDHeader ensures the real NewServer chain emits the
// X-Request-Id response header on a health request.
func TestNewServer_RequestIDHeader(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := NewServer(Deps{
		Config: config.Config{Port: 0, ProxyClientMaxBodySize: "128mb"},
		Logger: log,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/health", nil)
	req.Header.Set(HeaderRequestID, "health-rid")
	srv.Handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(HeaderRequestID); got != "health-rid" {
		t.Errorf("health response X-Request-Id = %q, want %q", got, "health-rid")
	}
	// And absent header on a separate request should still produce one.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/health", nil)
	srv.Handler.ServeHTTP(rec2, req2)
	if got := rec2.Header().Get(HeaderRequestID); got == "" {
		t.Error("health response missing generated X-Request-Id")
	}
}