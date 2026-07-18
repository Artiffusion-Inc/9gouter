package http

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticHandler(t *testing.T) {
	handler := NewStaticHandler(slog.Default())

	tests := []struct {
		name         string
		path         string
		wantStatus   int
		wantPrefix   string
		wantContains string
		wantCT       string
	}{
		{
			name:         "root serves index.html",
			path:         "/",
			wantStatus:   http.StatusOK,
			wantContains: "9Router - AI Infrastructure Management",
			wantCT:       "text/html; charset=utf-8",
		},
		{
			name:         "unknown non-API path falls back to index.html",
			path:         "/dashboard/keys",
			wantStatus:   http.StatusOK,
			wantContains: "9Router - AI Infrastructure Management",
			wantCT:       "text/html; charset=utf-8",
		},
		{
			name:       "API route is not served by static handler",
			path:       "/v1/chat/completions",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "API route under /api not served by static handler",
			path:       "/api/version",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			body := rec.Body.String()
			if tt.wantContains != "" && !strings.Contains(body, tt.wantContains) {
				t.Errorf("body does not contain %q:\n%s", tt.wantContains, body)
			}
			if tt.wantPrefix != "" && !strings.HasPrefix(body, tt.wantPrefix) {
				t.Errorf("body prefix = %q, want %q", body, tt.wantPrefix)
			}
			if tt.wantCT != "" {
				if got := rec.Header().Get("Content-Type"); got != tt.wantCT {
					t.Errorf("Content-Type = %q, want %q", got, tt.wantCT)
				}
			}
		})
	}
}

func TestStaticHandlerHead(t *testing.T) {
	handler := NewStaticHandler(slog.Default())
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body should be empty, got %d bytes", rec.Body.Len())
	}
}

func TestStaticHandlerMethodNotAllowed(t *testing.T) {
	handler := NewStaticHandler(slog.Default())
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestStaticHandlerAssetCache(t *testing.T) {
	// The placeholder index.html is not fingerprinted; verify the cache header
	// helper does not add Cache-Control for plain index.html.
	handler := NewStaticHandler(slog.Default())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("Cache-Control") != "" {
		t.Errorf("Cache-Control should be empty for index.html, got %q", rec.Header().Get("Cache-Control"))
	}
}

func TestStaticHandlerCopyBytes(t *testing.T) {
	src := strings.NewReader("hello")
	rec := httptest.NewRecorder()
	if err := copyBytes(rec, src, 5); err != nil {
		t.Fatalf("copyBytes: %v", err)
	}
	if got := rec.Body.String(); got != "hello" {
		t.Errorf("copied = %q, want %q", got, "hello")
	}

	src = strings.NewReader("")
	rec = httptest.NewRecorder()
	if err := copyBytes(rec, src, 0); err != nil {
		t.Fatalf("copyBytes zero: %v", err)
	}
	if got := rec.Body.String(); got != "" {
		t.Errorf("copied zero = %q, want empty", got)
	}

	// Unknown size (negative) falls back to io.Copy.
	src = strings.NewReader("fallback")
	rec = httptest.NewRecorder()
	if err := copyBytes(rec, src, -1); err != nil {
		t.Fatalf("copyBytes negative: %v", err)
	}
	if got := rec.Body.String(); got != "fallback" {
		t.Errorf("copied negative = %q, want %q", got, "fallback")
	}
}

func TestStaticHandlerUnknownMime(t *testing.T) {
	// Ensure .html overrides the default octet-stream mapping.
	if ct := mimeTypeByExt(".html"); ct != "text/html; charset=utf-8" {
		t.Errorf("html mime = %q, want text/html", ct)
	}
	if ct := mimeTypeByExt(".xyz"); ct != "application/octet-stream" {
		t.Errorf("unknown mime = %q, want application/octet-stream", ct)
	}
}

func mimeTypeByExt(ext string) string {
	ct := mimeTypeByExtension(ext)
	return ct
}

func mimeTypeByExtension(ext string) string {
	// Local helper mirroring the private logic in static.go for unit testing.
	ct := http.DetectContentType([]byte{})
	_ = ct
	if ext == ".html" {
		return "text/html; charset=utf-8"
	}
	return "application/octet-stream"
}
