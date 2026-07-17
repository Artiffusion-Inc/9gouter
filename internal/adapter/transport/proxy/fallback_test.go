package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

type staticPoolSource struct {
	pools []ProxyPool
}

func (s *staticPoolSource) List(ctx context.Context, isActive bool) ([]ProxyPool, error) {
	return s.pools, nil
}

func TestFallbackFindWorkingProxy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	// Create a tiny HTTP proxy that forwards to the target.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Minimal forward proxy: accept any host and echo OK.
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	opts := testOptions()
	opts.ProxyFastFailTimeout = 100 * time.Millisecond
	opts.ProxyFallbackProbeTimeout = 500 * time.Millisecond
	h := NewHealth(opts)
	f := NewFallback(opts, nil, h)
	f.resetFallbackCache()

	// Override env lookup so env proxy is empty.
	old := envGetter
	envGetter = func(string) string { return "" }
	defer func() { envGetter = old }()

	ctx := context.Background()
	tr, url, err := f.Find(ctx, target.URL)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if tr != nil {
		t.Fatalf("expected no working proxy without candidates, got %s", url)
	}

	// Now add a working proxy candidate.
	f.source = &staticPoolSource{pools: []ProxyPool{{ProxyURL: proxy.URL}}}
	tr, url, err = f.Find(ctx, target.URL)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if url == "" {
		t.Fatal("expected a working proxy URL")
	}
}

func TestFallbackEnvProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	opts := testOptions()
	opts.ProxyFastFailTimeout = 100 * time.Millisecond
	opts.ProxyFallbackProbeTimeout = 500 * time.Millisecond
	h := NewHealth(opts)
	f := NewFallback(opts, nil, h)
	f.resetFallbackCache()

	old := envGetter
	envGetter = func(key string) string {
		if key == "HTTP_PROXY" {
			return proxy.URL
		}
		return ""
	}
	defer func() { envGetter = old }()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	ctx := context.Background()
	tr, url, err := f.Find(ctx, target.URL)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if url == "" {
		t.Fatal("expected env proxy to be selected")
	}
	_ = tr
}

func TestNoProxyEnv(t *testing.T) {
	oldNoProxy := os.Getenv("NO_PROXY")
	defer os.Setenv("NO_PROXY", oldNoProxy)
	os.Setenv("NO_PROXY", "localhost,example.com")

	if !noProxyMatch("example.com", os.Getenv("NO_PROXY")) {
		t.Fatal("expected example.com to match NO_PROXY")
	}
	if noProxyMatch("other.com", os.Getenv("NO_PROXY")) {
		t.Fatal("expected other.com not to match NO_PROXY")
	}
}
