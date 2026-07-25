package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// ValidatedTarget is the immutable, pre-resolved destination for an untrusted
// outbound URL (user-supplied image input / image-result download). It carries
// the original scheme/hostname/port the client requested plus the validated
// resolved IP, so the actual direct dial, HTTP CONNECT or SOCKS5 target can be
// pinned to the address that passed SSRF policy — defeating DNS rebinding
// between the policy check and the actual TCP connect.
//
// Only the production proxy transport interprets this type. imageproxy creates
// it through an injectable resolver after SSRF checks and attaches it to the
// request context; ProxyAwareFetch consumes it. It is never serialised and
// never sent upstream.
type ValidatedTarget struct {
	// Scheme is the original request scheme ("https"). ValidatedTarget is only
	// meaningful for HTTPS image input/download; HTTP is rejected before this
	// type is built.
	Scheme string
	// Hostname is the original request hostname (used as TLS SNI and HTTP Host
	// so the upstream sees the real virtual host, not the pinned IP).
	Hostname string
	// Port is the effective port (explicit or scheme default).
	Port string
	// IP is the validated resolved address. Direct dial, HTTP CONNECT and the
	// SOCKS5 destination all connect to this IP:Port. It must already satisfy the
	// SSRF allowlist (non-loopback, non-private, non-link-local, non-CGNAT, non
	// multicast, non-metadata, non-unspecified).
	IP net.IP
	// RedirectLineage records the chain of validated targets across redirects,
	// each hop re-validated before being appended. It exists for diagnostics and
	// regression assertions, not for upstream transmission.
	RedirectLineage []string
}

// Address returns "IP:Port" for the actual TCP/CONNECT/SOCKS destination.
func (v ValidatedTarget) Address() string {
	return net.JoinHostPort(v.IP.String(), v.Port)
}

// IsPinned reports whether the target has a resolved IP to pin to.
func (v ValidatedTarget) IsPinned() bool {
	return v.IP != nil && v.Port != ""
}

type pinnedTargetCtxKey struct{}

// WithValidatedTarget attaches a ValidatedTarget to the request context so
// ProxyAwareFetch (and the transports it builds) dial the validated IP:port
// instead of re-resolving the request hostname. The original *http.Request URL
// is left untouched so TLS SNI / HTTP Host keep the real virtual host.
func WithValidatedTarget(ctx context.Context, vt ValidatedTarget) context.Context {
	return context.WithValue(ctx, pinnedTargetCtxKey{}, vt)
}

// ValidatedTargetFromContext returns the pinned target attached to the context,
// if any. ok is false when no target was attached (a normal provider lifecycle
// request that should go through the standard proxy pipeline).
func ValidatedTargetFromContext(ctx context.Context) (ValidatedTarget, bool) {
	vt, ok := ctx.Value(pinnedTargetCtxKey{}).(ValidatedTarget)
	return vt, ok
}

// ErrPinnedTargetRejected is returned when a request carries a ValidatedTarget
// but the selected route cannot honour it (relay / fallback paths today do not
// accept a pinned destination). For untrusted image URLs this fails hard rather
// than degrading to an unverified DNS dial.
var ErrPinnedTargetRejected = errors.New("pinned target rejected by route that cannot verify destination")

// pinnedDialer is the seam for the direct-dial and CONNECT-tunnel connect.
// Production uses the real dialer; tests substitute a recording dialer that
// captures the actual destination and SNI without performing a real connect.
type pinnedDialer interface {
	DialPinned(ctx context.Context, vt ValidatedTarget) (net.Conn, error)
}

// realPinnedDialer dials the pinned IP:port over TCP.
type realPinnedDialer struct{}

func (realPinnedDialer) DialPinned(ctx context.Context, vt ValidatedTarget) (net.Conn, error) {
	d := &net.Dialer{}
	return d.DialContext(ctx, "tcp", vt.Address())
}

// pinnedDial is the seam used by the direct and CONNECT transports. Tests
// override it via SetPinnedDialerForTest to record the actual dial/CONNECT
// destination and SNI without performing a real network connect.
var pinnedDial pinnedDialer = realPinnedDialer{}

// SetPinnedDialerForTest overrides the pinned-dial seam. It is intended for the
// recording regression in image parity step 1 that proves the validated IP (not
// a re-resolved hostname) reaches the actual direct/CONNECT/SOCKS path while
// TLS SNI/Host keep the original hostname.
func SetPinnedDialerForTest(d pinnedDialer) func() {
	prev := pinnedDial
	pinnedDial = d
	return func() { pinnedDial = prev }
}

// noRedirect makes the pinned http.Client surface 3xx to the adapter instead of
// following; the adapter re-validates each Location hop and rebuilds a fresh
// pinned request. CheckRedirect is a Client field, so it is set on the client
// the caller builds from the returned transport (see fetchPinned).
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// pinnedDirectTransport builds an *http.Transport that dials the pinned target
// directly (no proxy) and keeps the original hostname as TLS ServerName and the
// HTTP request Host. The transport does NOT follow redirects.
func pinnedDirectTransport(opts Options, vt ValidatedTarget) *http.Transport {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			// `address` is the URL host:port; ignore it and dial the pinned IP.
			return pinnedDial.DialPinned(ctx, vt)
		},
		ResponseHeaderTimeout: opts.FetchHeadersTimeout,
		IdleConnTimeout:       opts.FetchKeepaliveTimeout,
		TLSHandshakeTimeout:   opts.FetchConnectTimeout,
		MaxIdleConnsPerHost:   1,
		ForceAttemptHTTP2:     false,
		// Preserve the original hostname as SNI so the upstream virtual host
		// matches the validated origin, not the pinned IP literal.
		TLSClientConfig: &tls.Config{ServerName: vt.Hostname},
	}
	return tr
}

// pinnedHTTPProxyTransport builds an *http.Transport that reaches the validated
// target through an HTTP proxy by performing an explicit CONNECT to the pinned
// IP:port (not the request hostname). net/http's built-in Transport.Proxy
// writes the CONNECT line to req.URL.Host from its internals, which we cannot
// intercept, so we do NOT set Transport.Proxy. Instead DialContext dials the
// proxy, writes the CONNECT to the pinned IP:port itself, and returns the
// tunneled connection; net/http then performs TLS over it with the original
// hostname as SNI. The HTTP request Host keeps the original hostname.
func pinnedHTTPProxyTransport(opts Options, proxyRaw string, vt ValidatedTarget) (*http.Transport, error) {
	p, err := NormalizeProxyURL(proxyRaw)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		// No Proxy set: we own the CONNECT to the pinned target.
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			// `address` is the request host:port net/http would dial; we ignore
			// it and produce an already-tunneled connection to the pinned
			// IP:port via the proxy.
			return pinnedHTTPConnect(ctx, opts, p, vt)
		},
		ResponseHeaderTimeout: opts.FetchHeadersTimeout,
		IdleConnTimeout:       opts.FetchKeepaliveTimeout,
		TLSHandshakeTimeout:   opts.FetchConnectTimeout,
		MaxIdleConnsPerHost:   1,
		ForceAttemptHTTP2:     false,
		TLSClientConfig:       &tls.Config{ServerName: vt.Hostname},
	}
	return tr, nil
}

// proxyDialer is the seam for reaching the proxy itself (NOT the pinned target).
// The proxy address is operator-trusted (dashboard connection proxy / env), so it
// is dialed with the standard resolver, not the pinned-target seam. Tests
// override it via SetProxyDialerForTest to record that the CONNECT line targets
// the pinned IP:port while the proxy dial stays on the proxy address.
type proxyDialer interface {
	DialProxy(ctx context.Context, network, address string) (net.Conn, error)
}

// realProxyDialer dials the proxy via the standard resolver.
type realProxyDialer struct{}

func (realProxyDialer) DialProxy(ctx context.Context, network, address string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second}
	return d.DialContext(ctx, network, address)
}

// proxyDial is the seam for dialing the proxy host. Tests override it to record
// the proxy address and the CONNECT target without a real network connect.
var proxyDial proxyDialer = realProxyDialer{}

// SetProxyDialerForTest overrides the proxy-dial seam. It records the proxy
// address actually dialed (must be the operator proxy, not the pinned target)
// while the CONNECT line targets the pinned IP:port.
func SetProxyDialerForTest(d proxyDialer) func() {
	prev := proxyDial
	proxyDial = d
	return func() { proxyDial = prev }
}

// pinnedHTTPConnect dials an HTTP proxy, issues CONNECT to the pinned IP:port,
// and returns the tunneled connection. Credentials from the proxy URL are sent
// via Proxy-Authorization (basic auth) when present. The CONNECT target is the
// validated IP:port — the proxy never learns the original hostname, and TLS
// SNI/Host stay on the original hostname.
func pinnedHTTPConnect(ctx context.Context, opts Options, p *ParsedURL, vt ValidatedTarget) (net.Conn, error) {
	proxyAddr := net.JoinHostPort(p.Host, p.Port)
	// Dial the proxy itself via the standard resolver — the proxy address is
	// operator-trusted (dashboard/env), not an untrusted image URL. The pinned
	// seam is only for the CONNECT target, written into the CONNECT line below.
	conn, err := proxyDial.DialProxy(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(opts.FetchConnectTimeout)); err != nil {
		conn.Close()
		return nil, err
	}
	connectReq := "CONNECT " + vt.Address() + " HTTP/1.1\r\nHost: " + vt.Address() + "\r\n"
	if p.Username != "" {
		connectReq += "Proxy-Authorization: Basic " + basicAuth(p.Username, p.Password) + "\r\n"
	}
	connectReq += "\r\n"
	if _, err := conn.Write([]byte(connectReq)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !containsStatus200(line) {
		conn.Close()
		return nil, fmt.Errorf("proxy CONNECT to %s failed: %s", vt.Address(), trimSpaceCRLF(line))
	}
	// Drain remaining response headers.
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if h == "\r\n" || h == "\n" {
			break
		}
	}
	// bufio.Reader may have buffered bytes past the headers; wrap so the TLS
	// layer sees them. For the recording test (no real bytes) this is a no-op.
	if br.Buffered() > 0 {
		buffered, _ := br.Peek(br.Buffered())
		conn = &prependConn{Conn: conn, prefix: buffered}
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func containsStatus200(line string) bool {
	// Status line like "HTTP/1.1 200 Connection established\r\n".
	return strings.Contains(line, " 200 ") || strings.Contains(line, " 200\t")
}

// trimSpaceCRLF strips trailing CR/LF/space/tab from a status line.
func trimSpaceCRLF(s string) string {
	return strings.TrimRight(s, "\r\n \t")
}

// prependConn serves buffered bytes read from the CONNECT response reader
// before delegating to the underlying connection.
type prependConn struct {
	net.Conn
	prefix []byte
}

func (c *prependConn) Read(b []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(b, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

// basicAuth mirrors net/http basic auth encoding without importing it.
func basicAuth(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}

// PinnedTargetJSON is a debug/serialization helper for diagnostics and
// regression assertions. It is NOT sent upstream.
type PinnedTargetJSON struct {
	Scheme   string   `json:"scheme"`
	Hostname string   `json:"hostname"`
	Port     string   `json:"port"`
	IP       string   `json:"ip"`
	Lineage  []string `json:"lineage,omitempty"`
}

// DebugJSON returns a redacted JSON representation for logs/assertions.
func (v ValidatedTarget) DebugJSON() string {
	b, _ := json.Marshal(PinnedTargetJSON{
		Scheme:   v.Scheme,
		Hostname: v.Hostname,
		Port:     v.Port,
		IP:       v.IP.String(),
		Lineage:  v.RedirectLineage,
	})
	return string(b)
}
