package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"golang.org/x/net/proxy"
)

// ParsedURL holds the decomposed proxy URL for dispatcher construction.
type ParsedURL struct {
	Scheme   string
	Host     string
	Port     string
	Username string
	Password string
}

// supportedProxySchemes matches the JS dispatcher.
var supportedProxySchemes = map[string]struct{}{
	"http":    {},
	"https":   {},
	"socks5":  {},
	"socks5h": {},
}

// NormalizeProxyURL parses a proxy URL string. It allows bare host:port by
// defaulting to http://, matching proxyDispatcher.js.
func NormalizeProxyURL(raw string) (*ParsedURL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty proxy URL")
	}
	// Bare host:port inputs (e.g. "127.0.0.1:8080") parse as a URL path and
	// produce no scheme; prepend http:// for those. Inputs with an unsupported
	// scheme (e.g. "ftp://...") must still be rejected.
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		var err2 error
		u, err2 = url.Parse("http://" + raw)
		if err2 != nil {
			return nil, err2
		}
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		var err2 error
		u, err2 = url.Parse("http://" + raw)
		if err2 != nil {
			return nil, err2
		}
		scheme = "http"
	}
	if scheme == "" {
		scheme = "http"
	}
	if _, ok := supportedProxySchemes[scheme]; !ok {
		return nil, fmt.Errorf("unsupported proxy scheme %q", scheme)
	}

	host := stripIPv6Brackets(u.Hostname())
	port := u.Port()
	if port == "" {
		port = defaultPortForScheme(scheme)
	}
	return &ParsedURL{
		Scheme:   scheme,
		Host:     host,
		Port:     port,
		Username: u.User.Username(),
		Password: func() string {
			p, _ := u.User.Password()
			return p
		}(),
	}, nil
}

// ProxyURLForLogs returns a proxy URL with credentials masked.
func ProxyURLForLogs(raw string) string {
	p, err := NormalizeProxyURL(raw)
	if err != nil {
		return raw
	}
	var auth string
	if p.Username != "" {
		auth = fmt.Sprintf("%s:****@", p.Username)
	}
	return fmt.Sprintf("%s://%s%s:%s", p.Scheme, auth, p.Host, p.Port)
}

// NewTransport builds a single *http.Transport for the given proxy URL.
// For HTTP/HTTPS proxies it uses Transport.Proxy. For SOCKS5 it uses
// golang.org/x/net/proxy. It applies the undici-equivalent timeout config:
//   - DialContext timeout = FetchConnectTimeout
//   - ResponseHeaderTimeout = FetchHeadersTimeout
//   - IdleConnTimeout = FetchKeepaliveTimeout
func NewTransport(opts Options, proxyRaw string) (*http.Transport, error) {
	p, err := NormalizeProxyURL(proxyRaw)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{
		Timeout:   opts.FetchConnectTimeout,
		KeepAlive: opts.FetchKeepaliveTimeout,
	}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		ResponseHeaderTimeout: opts.FetchHeadersTimeout,
		IdleConnTimeout:       opts.FetchKeepaliveTimeout,
		TLSHandshakeTimeout:   opts.FetchConnectTimeout,
		MaxIdleConnsPerHost:   1,
		ForceAttemptHTTP2:     false,
	}

	switch p.Scheme {
	case "http", "https":
		u, _ := url.Parse(fmt.Sprintf("%s://%s%s:%s", p.Scheme, p.Host, formatAuth(p.Username, p.Password), p.Port))
		if p.Username != "" {
			u.User = url.UserPassword(p.Username, p.Password)
		}
		tr.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if p.Username != "" {
			auth = &proxy.Auth{User: p.Username, Password: p.Password}
		}
		addr := net.JoinHostPort(p.Host, p.Port)
		socksDialer, err := proxy.SOCKS5("tcp", addr, auth, &familyPinDialer{
			base:   dialer,
			family: FamilyAuto,
		})
		if err != nil {
			return nil, fmt.Errorf("socks5 dialer: %w", err)
		}
		// SOCKS5 returns proxy.Dialer which exposes Dial; wrap to DialContext.
		tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if ctxDialer, ok := socksDialer.(proxy.ContextDialer); ok {
				return ctxDialer.DialContext(ctx, network, address)
			}
			return socksDialer.Dial(network, address)
		}
	}

	return tr, nil
}

// NewDirectTransport builds a direct (no-proxy) transport. When
// ProxyDispatcherConnections > 1 it returns a RoundRobinTransports holder.
func NewDirectTransport(opts Options) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   opts.FetchConnectTimeout,
		KeepAlive: opts.FetchKeepaliveTimeout,
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		ResponseHeaderTimeout: opts.FetchHeadersTimeout,
		IdleConnTimeout:       opts.FetchKeepaliveTimeout,
		TLSHandshakeTimeout:   opts.FetchConnectTimeout,
		MaxIdleConnsPerHost:   1,
		ForceAttemptHTTP2:     false,
	}
}

// RoundRobinTransports fans out across N one-connection transports and is used
// to mitigate Node 24-style same-origin SSE serialization; for Go this reduces
// the chance of one long-lived stream monopolising a pooled connection.
type RoundRobinTransports struct {
	transports []*http.Transport
	counter    atomic.Uint64
}

// NewRoundRobinTransports creates N direct transports.
func NewRoundRobinTransports(opts Options, n int) *RoundRobinTransports {
	if n <= 0 {
		n = 1
	}
	transports := make([]*http.Transport, n)
	for i := 0; i < n; i++ {
		transports[i] = NewDirectTransport(opts)
	}
	return &RoundRobinTransports{transports: transports}
}

// Next returns the next transport in round-robin order.
func (r *RoundRobinTransports) Next() *http.Transport {
	if len(r.transports) == 1 {
		return r.transports[0]
	}
	idx := r.counter.Add(1) % uint64(len(r.transports))
	return r.transports[idx]
}

// CloseIdleConnections closes idle connections on all underlying transports.
func (r *RoundRobinTransports) CloseIdleConnections() {
	for _, t := range r.transports {
		t.CloseIdleConnections()
	}
}

// RoundTrip implements http.RoundTripper so RoundRobinTransports can be used
// directly as an http.Client transport.
func (r *RoundRobinTransports) RoundTrip(req *http.Request) (*http.Response, error) {
	return r.Next().RoundTrip(req)
}

// familyPinDialer is a net.Dialer wrapper that optionally restricts the IP
// family for outgoing TCP connections. It is used as the forward dialer for
// SOCKS5 proxies.
type familyPinDialer struct {
	base   *net.Dialer
	family Family
}

func (d *familyPinDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch d.family {
	case FamilyIPv4:
		return d.base.DialContext(ctx, "tcp4", address)
	case FamilyIPv6:
		return d.base.DialContext(ctx, "tcp6", address)
	default:
		return d.base.DialContext(ctx, network, address)
	}
}

func (d *familyPinDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func formatAuth(user, pass string) string {
	if user == "" {
		return ""
	}
	if pass == "" {
		return user + "@"
	}
	return fmt.Sprintf("%s:%s@", user, pass)
}

// CloneTLSConfig returns a shallow copy of the default TLS config with
// server name set. It is exposed for tests and future extensibility.
func CloneTLSConfig(serverName string) *tls.Config {
	return &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: false,
	}
}
