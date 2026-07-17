package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProxyAwareFetchDirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen", "1")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := ProxyAwareFetch(req.Context(), client, req, testOptions(), ProxyFetchOptions{}, nil)
	if err != nil {
		t.Fatalf("ProxyAwareFetch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Seen") != "1" {
		t.Fatal("expected X-Seen header")
	}
}

func TestProxyAwareFetchVercelRelay(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-relay-target") == "" {
			t.Fatal("expected x-relay-target header")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer relay.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not hit upstream")
	}))
	defer upstream.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, upstream.URL, nil)
	_, err := ProxyAwareFetch(req.Context(), client, req, testOptions(), ProxyFetchOptions{VercelRelayUrl: relay.URL}, nil)
	if err != nil {
		t.Fatalf("ProxyAwareFetch: %v", err)
	}
}
