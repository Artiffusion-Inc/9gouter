package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// requestIDKey is the unexported context key carrying the request correlation
// ID. It is unexported so only this package can read/write the value; callers
// use RequestIDFromContext to retrieve it.
type requestIDKey struct{}

// HeaderRequestID is the HTTP header name carrying the request correlation ID,
// echoed on both the request (in) and response (out) so clients and upstream
// services can correlate logs of a single request.
const HeaderRequestID = "X-Request-Id"

// NewRequestID returns a fresh request ID. Uses github.com/google/uuid (already
// in go.mod) to produce a UUIDv4 string. Falls back to a 16-byte hex string
// from crypto/rand if uuid generation fails (extremely unlikely).
func NewRequestID() string {
	if id, err := uuid.NewRandom(); err == nil {
		return id.String()
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	// Last-resort fallback so we never return an empty ID.
	return "00000000000000000000000000000000"
}

// RequestIDFromContext returns the request ID stored in ctx by
// RequestIDMiddleware, or "" if none is present.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

// WithRequestID returns a copy of ctx with the request ID stored under the
// requestIDKey. It is exported so handlers outside this package (e.g. the
// proxy chat usecase) can propagate a request ID into derived contexts that
// flow through translator/provider/proxy layers.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDMiddleware ensures every request carries a correlation ID. If the
// incoming request has an X-Request-Id header it is preserved unchanged;
// otherwise a fresh ID is generated. The ID is stored in the request context
// (retrievable via RequestIDFromContext) and echoed back on the response
// header so clients can correlate. This middleware must wrap BEFORE
// LogMiddleware and RecoverMiddleware so their log records can include the
// request ID via LoggerFromContext(r.Context()).
func RequestIDMiddleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(HeaderRequestID)
			if id == "" {
				id = NewRequestID()
			}
			// Echo on the response so clients/upstreams can correlate.
			w.Header().Set(HeaderRequestID, id)
			ctx := WithRequestID(r.Context(), id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// LoggerFromContext returns a *slog.Logger bound to the request ID stored in
// ctx, if any. When ctx carries a request ID, the returned logger adds a
// "request_id" attribute to every record so structured logs can be correlated
// across the proxychat -> translator -> provider -> proxy stack. When ctx has
// no request ID (e.g. background tasks, tests), the default logger is
// returned unchanged.
func LoggerFromContext(ctx context.Context) *slog.Logger {
	if id := RequestIDFromContext(ctx); id != "" {
		return slog.Default().With("request_id", id)
	}
	return slog.Default()
}