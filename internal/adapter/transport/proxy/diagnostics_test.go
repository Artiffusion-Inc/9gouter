package proxy

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFetchErrorSource verifies the #2703 Fix 5 typed failure source is set
// on a FetchError constructed for a relay failure (Vercel relay path).
func TestFetchErrorSource(t *testing.T) {
	err := errors.New("relay dial failed")
	fe := &FetchError{Err: err, Cause: DescribeFetchCause(err), Source: FailureSourceRelay}
	if fe.Source != FailureSourceRelay {
		t.Fatalf("source = %q, want %q", fe.Source, FailureSourceRelay)
	}
	if fe.Source == FailureSourceProxy || fe.Source == FailureSourceUpstream {
		t.Fatalf("relay failure misclassified as %q", fe.Source)
	}
}

// TestProxyAwareFetchRelayFailureSource confirms a Vercel relay failure
// returns a FetchError tagged FailureSourceRelay so account-selection logic
// can distinguish a relay outage from a provider/account failure.
func TestProxyAwareFetchRelayFailureSource(t *testing.T) {
	// Relay that hangs until cancelled → context deadline surfaces a fetch error.
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer relay.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not hit upstream")
	}))
	defer upstream.Close()

	client := &http.Client{Timeout: 50 * time.Millisecond}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, upstream.URL, nil)
	_, err := ProxyAwareFetch(req.Context(), client, req, testOptions(), ProxyFetchOptions{VercelRelayUrl: relay.URL}, nil)
	if err == nil {
		t.Fatal("expected error from relay timeout")
	}
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FetchError, got %T: %v", err, err)
	}
	if fe.Source != FailureSourceRelay {
		t.Fatalf("source = %q, want %q", fe.Source, FailureSourceRelay)
	}
}

// TestProxyAwareFetchFallbackLogsDiagnostics verifies the #2703 Fix 5
// structured diagnostics line is emitted when a connection proxy fails and
// the request falls back to direct (non-strict). The log must carry
// phase=inference, route=standard-proxy, fallbackToDirect=true, and
// failureSource=proxy — the fields the JS build never emitted.
func TestProxyAwareFetchFallbackLogsDiagnostics(t *testing.T) {
	// Upstream reachable directly.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// A proxy URL pointing at a dead port so fetchWithProxy fails fast.
	deadProxy := "http://127.0.0.1:1"

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, upstream.URL, nil)
	_, err := ProxyAwareFetch(req.Context(), client, req, testOptions(), ProxyFetchOptions{
		ConnectionProxyUrl:    deadProxy,
		ConnectionProxyEnabled: true,
		StrictProxy:           false,
		Logger:                logger,
	}, nil)
	if err != nil {
		t.Fatalf("expected fallback to direct, got error: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"proxy fallback to direct", "phase", "inference", "standard-proxy", "fallbackToDirect", "true", "failureSource", "proxy"} {
		if !strings.Contains(out, want) {
			t.Fatalf("diagnostics log missing %q; got:\n%s", want, out)
		}
	}
}

// TestProxyAwareFetchStrictNoFallbackLog confirms strict mode does not emit
// a fallback log (it fails hard instead) — the diagnostics line is only for
// the non-strict fallback-to-direct path.
func TestProxyAwareFetchStrictNoFallbackLog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not hit upstream in strict mode")
	}))
	defer upstream.Close()

	deadProxy := "http://127.0.0.1:1"
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, upstream.URL, nil)
	_, err := ProxyAwareFetch(req.Context(), client, req, testOptions(), ProxyFetchOptions{
		ConnectionProxyUrl:    deadProxy,
		ConnectionProxyEnabled: true,
		StrictProxy:           true,
		Logger:                logger,
	}, nil)
	if err == nil {
		t.Fatal("expected strict mode to fail hard")
	}
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FetchError, got %T", err)
	}
	if fe.Source != FailureSourceProxy {
		t.Fatalf("strict proxy failure source = %q, want %q", fe.Source, FailureSourceProxy)
	}
	if strings.Contains(buf.String(), "proxy fallback to direct") {
		t.Fatalf("strict mode must not log a fallback; got:\n%s", buf.String())
	}
}