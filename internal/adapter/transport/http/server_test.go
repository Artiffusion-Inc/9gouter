package http

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
)

func TestNewServerHealth(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := NewServer(Deps{
		Config: config.Config{Port: 0, ProxyClientMaxBodySize: "128mb"},
		Logger: log,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/health", nil)
	srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("health status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("health body = %q, want %q", rec.Body.String(), "ok")
	}
}

func TestMiddlewareClientIPHeader(t *testing.T) {
	var gotIP string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIP = r.Header.Get("X-9r-Real-Ip")
		w.WriteHeader(http.StatusOK)
	})

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	chain := Chain(
		RecoverMiddleware(log),
		LogMiddleware(log),
		ClientIPMiddleware(),
		BodySizeMiddleware(128*1024*1024),
	)

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "9.8.7.6")
	rec := httptest.NewRecorder()
	chain(handler).ServeHTTP(rec, req)

	if gotIP != "9.8.7.6" {
		t.Errorf("X-9r-Real-Ip = %q, want %q", gotIP, "9.8.7.6")
	}
}

func TestMiddlewareBodySizeLimit(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var mr *http.MaxBytesError
			if errors.As(err, &mr) {
				http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(body)
	})

	chain := Chain(BodySizeMiddleware(10))
	req := httptest.NewRequest("POST", "/", strings.NewReader("this body is way too large"))
	rec := httptest.NewRecorder()
	chain(handler).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestMiddlewareRecover(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	chain := Chain(RecoverMiddleware(log))
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	chain(handler).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestMiddlewareAuth(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	auth := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer good" }
	chain := Chain(APIMiddleware(auth))

	req := httptest.NewRequest("GET", "/api/protected", nil)
	rec := httptest.NewRecorder()
	chain(handler).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	req = httptest.NewRequest("GET", "/api/protected", nil)
	req.Header.Set("Authorization", "Bearer good")
	rec = httptest.NewRecorder()
	chain(handler).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("auth status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestNewServerShutdown(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := NewServer(Deps{
		Config: config.Config{Port: 0, ProxyClientMaxBodySize: "128mb"},
		Logger: log,
	})

	srv.BaseContext = func(l net.Listener) context.Context { return context.Background() }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
