package proxy

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFastFailLiveServer(t *testing.T) {
	server := httptest.NewServer(nil)
	defer server.Close()

	opts := testOptions()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Extract host:port from httptest server URL.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	proxyURL := "http://127.0.0.1:" + port
	if err := FastFail(ctx, opts, proxyURL); err != nil {
		t.Fatalf("FastFail on live server: %v", err)
	}
}

func TestFastFailDeadProxy(t *testing.T) {
	opts := testOptions()
	opts.ProxyFastFailTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := FastFail(ctx, opts, "http://127.0.0.1:1")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error for dead proxy")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("FastFail took too long: %v", elapsed)
	}
}
