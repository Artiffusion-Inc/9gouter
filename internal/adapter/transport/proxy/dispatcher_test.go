package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
)

func testOptions() Options {
	return Options{
		FetchConnectTimeout:        5 * time.Second,
		FetchHeadersTimeout:        5 * time.Second,
		FetchBodyTimeout:           10 * time.Second,
		FetchKeepaliveTimeout:      4 * time.Second,
		SocksHandshakeTimeout:      10 * time.Second,
		ProxyDispatcherConnections: 1,
		ProxyFastFailTimeout:       2 * time.Second,
		ProxyHealthCacheTTL:        30 * time.Second,
		ProxyHealthUnhealthyTTL:    2 * time.Second,
		ProxyFallbackProbeTimeout:  3 * time.Second,
	}
}

func TestNormalizeProxyURL(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
		scheme  string
		host    string
		port    string
		user    string
	}{
		{"http full", "http://user:pass@127.0.0.1:8080", false, "http", "127.0.0.1", "8080", "user"},
		{"bare host:port", "127.0.0.1:8080", false, "http", "127.0.0.1", "8080", ""},
		{"socks5", "socks5://192.168.1.1:1080", false, "socks5", "192.168.1.1", "1080", ""},
		{"empty", "", true, "", "", "", ""},
		{"unsupported", "ftp://1.2.3.4:21", true, "", "", "", ""},
		// Port upstream d8c2298d (security audit): shell metacharacters and
		// CR/LF must be rejected to prevent CRLF injection in CONNECT and
		// env-write injection.
		{"crlf injection", "http://127.0.0.1:8080\r\nX-Inject: yes", true, "", "", "", ""},
		{"lf in host", "http://127.0.0.1\n:8080", true, "", "", "", ""},
		{"backtick", "http://`id`:8080", true, "", "", "", ""},
		{"dollar", "http://$HOME:8080", true, "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NormalizeProxyURL(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Scheme != tc.scheme || p.Host != tc.host || p.Port != tc.port || p.Username != tc.user {
				t.Fatalf("got %+v, want scheme=%s host=%s port=%s user=%s", p, tc.scheme, tc.host, tc.port, tc.user)
			}
		})
	}
}

func TestNewTransportHTTP(t *testing.T) {
	tr, err := NewTransport(testOptions(), "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	if tr.Proxy == nil {
		t.Fatal("expected Proxy set")
	}
	if tr.ResponseHeaderTimeout != 5*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v", tr.ResponseHeaderTimeout)
	}
	if tr.IdleConnTimeout != 4*time.Second {
		t.Fatalf("IdleConnTimeout = %v", tr.IdleConnTimeout)
	}
}

func TestNewTransportSOCKS5(t *testing.T) {
	tr, err := NewTransport(testOptions(), "socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	if tr.Proxy != nil {
		t.Fatal("SOCKS5 transport should not set HTTP Proxy")
	}
	if tr.DialContext == nil {
		t.Fatal("expected DialContext set for SOCKS5")
	}
}

func TestRoundRobinTransports(t *testing.T) {
	opts := testOptions()
	opts.ProxyDispatcherConnections = 3
	rr := NewRoundRobinTransports(opts, opts.ProxyDispatcherConnections)
	seen := make(map[*http.Transport]bool)
	for i := 0; i < 10; i++ {
		seen[rr.Next()] = true
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 distinct transports, got %d", len(seen))
	}
}

func TestOptionsFromConfig(t *testing.T) {
	cfg := config.Config{
		FetchConnectTimeout:        config.DurationMs(60 * time.Second),
		FetchKeepaliveTimeout:      config.DurationMs(4 * time.Second),
		ProxyDispatcherConnections: 2,
	}
	opts := OptionsFromConfig(cfg)
	if opts.FetchConnectTimeout != time.Minute {
		t.Fatalf("FetchConnectTimeout = %v", opts.FetchConnectTimeout)
	}
	if opts.ProxyDispatcherConnections != 2 {
		t.Fatalf("ProxyDispatcherConnections = %d", opts.ProxyDispatcherConnections)
	}
}
