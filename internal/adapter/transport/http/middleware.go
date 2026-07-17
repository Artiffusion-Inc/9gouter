package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Middleware is an HTTP middleware function.
type Middleware func(http.Handler) http.Handler

// Chain composes middlewares in order.
func Chain(ms ...Middleware) Middleware {
	return func(h http.Handler) http.Handler {
		for i := len(ms) - 1; i >= 0; i-- {
			h = ms[i](h)
		}
		return h
	}
}

// RecoverMiddleware recovers from panics, logs them, and returns HTTP 500.
func RecoverMiddleware(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic recovered", "error", rec, "path", r.URL.Path, "method", r.Method)
					http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// LogMiddleware logs each request with method, path, status, duration, and client IP.
func LogMiddleware(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			lr := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
			next.ServeHTTP(lr, r)
			log.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", lr.statusCode,
				"duration", time.Since(start),
				"client_ip", FromRequest(r),
			)
		})
	}
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (lr *loggingResponseWriter) WriteHeader(code int) {
	if !lr.written {
		lr.statusCode = code
		lr.written = true
		lr.ResponseWriter.WriteHeader(code)
	}
}

// ClientIPMiddleware sets X-9r-Real-Ip and X-9r-Via-Proxy headers so downstream
// handlers see the trusted client IP (mirroring custom-server.js).
func ClientIPMiddleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := FromRequest(r)
			r.Header.Set("X-9r-Real-Ip", ip)
			if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-Ip") != "" {
				r.Header.Set("X-9r-Via-Proxy", "1")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BodySizeMiddleware limits request body size. A limit <= 0 disables the limit.
func BodySizeMiddleware(limit int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limit > 0 {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
			// Surface MaxBytesReader errors after the handler returns so the
			// middleware can return a clean 413 even if the handler ignored the
			// read error (common with httptest).
			if r.Body != nil {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					var mr *http.MaxBytesError
					if errors.As(err, &mr) {
						http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
					}
				}
			}
		})
	}
}

// AuthFunc is an injectable authentication check for /api/* routes. It should
// return true if the request is authenticated. Real implementation is T016.
type AuthFunc func(r *http.Request) bool

// APIMiddleware enforces authentication on /api/* paths when auth is provided.
func APIMiddleware(auth AuthFunc) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if auth != nil && strings.HasPrefix(r.URL.Path, "/api/") {
				if !auth(r) {
					w.Header().Set("WWW-Authenticate", `Bearer`)
					http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequestContextMiddleware returns a middleware that ensures every request has
// a request-scoped context (best practice, no-op because http.Request already
// carries ctx, but useful for tests/mocks).
func RequestContextMiddleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ContextErrorMiddleware converts context errors into proper HTTP status codes
// when a handler has not yet written a response.
func ContextErrorMiddleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			if r.Context().Err() != nil {
				if _, ok := w.(interface{ Written() bool }); !ok {
					if errors.Is(r.Context().Err(), context.DeadlineExceeded) {
						http.Error(w, http.StatusText(http.StatusGatewayTimeout), http.StatusGatewayTimeout)
					} else if errors.Is(r.Context().Err(), context.Canceled) {
						// Client closed connection; nothing to write.
					}
				}
			}
		})
	}
}

// parseBodySize parses a human-readable byte size string such as "128mb" or
// "1.5gb". Falls back to bytes if no unit is recognized.
func parseBodySize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "gb"):
		mult = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "gb")
	case strings.HasSuffix(s, "mb"):
		mult = 1024 * 1024
		s = strings.TrimSuffix(s, "mb")
	case strings.HasSuffix(s, "kb"):
		mult = 1024
		s = strings.TrimSuffix(s, "kb")
	}
	n, err := fmt.Sscanf(s, "%d", new(int64))
	if err != nil {
		return 0, fmt.Errorf("invalid body size %q", s)
	}
	return int64(n) * mult, nil
}

// Helper to drain and close a body defensively.
func closeBody(body io.ReadCloser) {
	if body == nil {
		return
	}
	_ = body.Close()
}
