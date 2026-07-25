package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// ProxyFetchOptions is the Go equivalent of proxyOptions in proxyFetch.js.
type ProxyFetchOptions struct {
	// VercelRelayUrl forwards the request via x-relay-target / x-relay-path.
	VercelRelayUrl string
	// ConnectionProxyUrl is a per-connection dashboard proxy URL.
	ConnectionProxyUrl string
	// ConnectionProxyEnabled gates the per-connection proxy.
	ConnectionProxyEnabled bool
	// StrictProxy fails hard instead of falling back on proxy errors.
	StrictProxy bool
	// NoProxy is a comma-separated list bypassing the connection proxy.
	NoProxy string
	// Logger receives structured route-diagnostics lines (phase=... route=...
	// fallbackToDirect=... failureSource=...). When nil, diagnostics are
	// silently dropped. Ports decolua/9router #2703 Fix 5.
	Logger *slog.Logger
}

// ProxyAwareFetch implements the proxyFetch.js pipeline:
// 1. Vercel relay
// 2. Connection proxy / env proxy
// 3. Fast-fail / dispatcher
// 4. Fallback
// 5. MITM DNS bypass
// 6. Direct (round-robin if configured)
//
// When the request context carries a proxy.ValidatedTarget (untrusted image
// input / image-result download), the standard relay/proxy/fallback pipeline is
// bypassed and the request is sent through a pinned transport: direct,
// HTTP-CONNECT or SOCKS5 to the validated IP:port with the original hostname
// preserved as TLS SNI/Host. Relay and fallback routes today cannot honour a
// pinned destination, so for a pinned request they are skipped and a proxy
// failure fails hard (no direct/fallback downgrade) instead of leaking the host
// IP to an unverified route.
func ProxyAwareFetch(ctx context.Context, client *http.Client, req *http.Request, opts Options, proxyOpts ProxyFetchOptions, fallback *Fallback) (*http.Response, error) {
	originalURL := req.URL.String()

	// Pinned-target fast path for untrusted image URLs. This route never reaches
	// the relay/env-proxy/fallback machinery; it always dials the validated
	// IP:port (direct or via a pinned HTTP/SOCKS proxy) and keeps the original
	// hostname as SNI/Host.
	if vt, ok := ValidatedTargetFromContext(ctx); ok && vt.IsPinned() {
		return fetchPinned(ctx, opts, proxyOpts, vt, req)
	}

	// 1. Vercel relay.
	if relay := strings.TrimSpace(proxyOpts.VercelRelayUrl); relay != "" {
		relayReq, err := buildRelayRequest(req, relay)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(relayReq)
		if err != nil {
			return nil, &FetchError{Err: err, Cause: DescribeFetchCause(err), Source: FailureSourceRelay}
		}
		return resp, nil
	}

	// 2. Resolve proxy URL.
	proxyURL := resolveConnectionProxyURL(originalURL, proxyOpts)
	if proxyURL == "" {
		proxyURL = resolveEnvProxyURL(originalURL)
	}

	// 3. MITM DNS bypass.
	if shouldBypassMitmDns(req.URL.Hostname()) {
		if proxyURL != "" {
			resp, err := fetchWithProxy(ctx, client, req, opts, proxyURL, proxyOpts.StrictProxy)
			if err == nil {
				return resp, nil
			}
			if proxyOpts.StrictProxy {
				return nil, err
			}
			logProxyFallback(proxyOpts.Logger, "mitm-bypass", proxyURL, originalURL, err)
		}
		if realIP, err := MITMBypassResolve(req.URL.Hostname()); err == nil {
			resp, err := fetchBypass(req, realIP)
			if err == nil {
				return resp, nil
			}
		}
	}

	// 4. Proxy path.
	if proxyURL != "" {
		resp, err := fetchWithProxy(ctx, client, req, opts, proxyURL, proxyOpts.StrictProxy)
		if err == nil {
			return resp, nil
		}
		if proxyOpts.StrictProxy {
			return nil, err
		}
		logProxyFallback(proxyOpts.Logger, "standard-proxy", proxyURL, originalURL, err)
		if fallback != nil {
			if tr, _, _ := fallback.Find(ctx, originalURL); tr != nil {
				fallbackClient := &http.Client{Timeout: opts.FetchBodyTimeout, Transport: tr}
				return fallbackClient.Do(req)
			}
		}
	}

	// 5. Direct fetch.
	return fetchDirect(ctx, client, req, opts)
}

// logProxyFallback emits the structured route-diagnostics line for a proxy
// failure that is about to fall back to direct (or to a fallback pool). It
// mirrors the JS chatCore.js "PROXY | provider | model | conn= | pool= | url="
// log plus the #2703 Fix 5 fields the JS build never emitted: phase,
// fallbackToDirect, and failureSource. The log is a Warn because a non-strict
// fallback means the host IP may now be exposed to the upstream — the
// operator-visible signal that strictProxy should be enabled for this route.
func logProxyFallback(logger *slog.Logger, route, proxyURL, targetURL string, err error) {
	if logger == nil {
		return
	}
	logger.Warn("proxy fallback to direct",
		"phase", "inference",
		"route", route,
		"fallbackToDirect", true,
		"failureSource", string(FailureSourceProxy),
		"proxyUrl", proxyURL,
		"targetUrl", targetURL,
		"cause", DescribeFetchCause(err),
	)
}

func resolveConnectionProxyURL(targetURL string, proxyOpts ProxyFetchOptions) string {
	if !proxyOpts.ConnectionProxyEnabled {
		return ""
	}
	raw := strings.TrimSpace(proxyOpts.ConnectionProxyUrl)
	if raw == "" {
		return ""
	}
	if noProxyMatch(hostOf(targetURL), proxyOpts.NoProxy) {
		return ""
	}
	parsed, err := NormalizeProxyURL(raw)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s://%s%s:%s", parsed.Scheme, parsed.Host, formatAuth(parsed.Username, parsed.Password), parsed.Port)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func buildRelayRequest(req *http.Request, relay string) (*http.Request, error) {
	relayReq := req.Clone(req.Context())
	relayURL, err := url.Parse(relay)
	if err != nil {
		return nil, err
	}
	relayReq.URL = relayURL
	relayReq.Host = relayURL.Host
	relayReq.Header.Set("x-relay-target", fmt.Sprintf("%s://%s", req.URL.Scheme, req.URL.Host))
	relayReq.Header.Set("x-relay-path", fmt.Sprintf("%s%s", req.URL.Path, relayQuery(req.URL.RawQuery)))
	return relayReq, nil
}

func relayQuery(raw string) string {
	if raw == "" {
		return ""
	}
	return "?" + raw
}

func fetchWithProxy(ctx context.Context, client *http.Client, req *http.Request, opts Options, proxyURL string, strict bool) (*http.Response, error) {
	if err := FastFail(ctx, opts, proxyURL); err != nil {
		if strict {
			return nil, &FetchError{Err: err, Cause: DescribeFetchCause(err), Source: FailureSourceProxy}
		}
		return nil, err
	}
	tr, err := NewTransport(opts, proxyURL)
	if err != nil {
		return nil, err
	}
	proxyClient := &http.Client{Timeout: opts.FetchBodyTimeout, Transport: tr}
	resp, err := proxyClient.Do(req)
	if err != nil {
		GlobalHealth(opts).Invalidate(proxyURL)
		if strict {
			return nil, &FetchError{Err: err, Cause: DescribeFetchCause(err), Source: FailureSourceProxy}
		}
		return nil, err
	}
	return resp, nil
}

func fetchDirect(ctx context.Context, client *http.Client, req *http.Request, opts Options) (*http.Response, error) {
	if opts.ProxyDispatcherConnections <= 1 {
		return client.Do(req)
	}
	// Use round-robin direct transports.
	tr := NewRoundRobinTransports(opts, opts.ProxyDispatcherConnections)
	rrClient := &http.Client{Timeout: opts.FetchBodyTimeout, Transport: tr}
	return rrClient.Do(req)
}

// fetchBypass performs an HTTPS request by connecting directly to the provided
// real IP while preserving the original SNI (servername). This bypasses
// /etc/hosts spoofing for MITM targets.
func fetchBypass(req *http.Request, realIP net.IP) (*http.Response, error) {
	addr := net.JoinHostPort(realIP.String(), "443")
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	tlsConn := tls.Client(conn, &tls.Config{ServerName: req.URL.Hostname()})
	if err := tlsConn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		conn.Close()
		return nil, err
	}
	if err := tlsConn.Handshake(); err != nil {
		tlsConn.Close()
		return nil, err
	}

	newReq := req.Clone(req.Context())
	newReq.URL.Scheme = "https"
	newReq.URL.Host = req.URL.Hostname()
	newReq.Host = req.URL.Hostname()
	newReq.Header.Set("Host", req.URL.Hostname())

	if err := newReq.Write(tlsConn); err != nil {
		tlsConn.Close()
		return nil, err
	}
	return http.ReadResponse(bufio.NewReader(tlsConn), newReq)
}

// fetchPinned sends req through a pinned transport for an untrusted image URL
// whose destination was validated before this call. It prefers a connection /
// env proxy (HTTP or SOCKS5) when configured so the egress still honours the
// operator's proxy topology, but the CONNECT/SOCKS target is the validated
// IP:port, not the request hostname. When no proxy is configured it dials the
// pinned IP:port directly. Relay and fallback are never used for a pinned
// request (they cannot verify the destination), so a proxy failure fails hard
// rather than degrading to an unverified direct dial.
func fetchPinned(ctx context.Context, opts Options, proxyOpts ProxyFetchOptions, vt ValidatedTarget, req *http.Request) (*http.Response, error) {
	proxyURL := resolveConnectionProxyURL(req.URL.String(), proxyOpts)
	if proxyURL == "" {
		proxyURL = resolveEnvProxyURL(req.URL.String())
	}
	var tr http.RoundTripper
	var err error
	if proxyURL == "" {
		tr = pinnedDirectTransport(opts, vt)
	} else {
		p, perr := NormalizeProxyURL(proxyURL)
		if perr != nil {
			return nil, &FetchError{Err: perr, Cause: DescribeFetchCause(perr), Source: FailureSourceProxy}
		}
		switch p.Scheme {
		case "socks5", "socks5h":
			tr, err = pinnedSocksTransport(ctx, opts, p, vt)
		default: // http, https
			tr, err = pinnedHTTPProxyTransport(opts, proxyURL, vt)
		}
		if err != nil {
			return nil, &FetchError{Err: err, Cause: DescribeFetchCause(err), Source: FailureSourceProxy}
		}
		if err := FastFail(ctx, opts, proxyURL); err != nil {
			return nil, &FetchError{Err: err, Cause: DescribeFetchCause(err), Source: FailureSourceProxy}
		}
	}
	pinnedClient := &http.Client{Timeout: opts.FetchBodyTimeout, Transport: tr, CheckRedirect: noRedirect}
	resp, err := pinnedClient.Do(req)
	if err != nil {
		return nil, &FetchError{Err: err, Cause: DescribeFetchCause(err), Source: FailureSourceProxy}
	}
	return resp, nil
}

// pinnedSocksTransport builds a SOCKS5 transport whose SOCKS5 destination is the
// pinned IP:port (not the request hostname). The original hostname is kept as
// TLS SNI/Host via TLSClientConfig. Credentials come from the proxy URL.
func pinnedSocksTransport(ctx context.Context, opts Options, p *ParsedURL, vt ValidatedTarget) (*http.Transport, error) {
	var auth *proxy.Auth
	if p.Username != "" {
		auth = &proxy.Auth{User: p.Username, Password: p.Password}
	}
	addr := net.JoinHostPort(p.Host, p.Port)
	base := &net.Dialer{Timeout: opts.FetchConnectTimeout, KeepAlive: opts.FetchKeepaliveTimeout}
	socksDialer, err := proxy.SOCKS5("tcp", addr, auth, &familyPinDialer{base: base, family: FamilyAuto})
	if err != nil {
		return nil, fmt.Errorf("socks5 dialer: %w", err)
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			// Replace the request host:port with the pinned IP:port so SOCKS5
			// connects to the validated address, not the re-resolved hostname.
			pinnedAddr := vt.Address()
			if ctxDialer, ok := socksDialer.(proxy.ContextDialer); ok {
				conn, derr := ctxDialer.DialContext(ctx, network, pinnedAddr)
				return conn, derr
			}
			return socksDialer.Dial(network, pinnedAddr)
		},
		ResponseHeaderTimeout: opts.FetchHeadersTimeout,
		IdleConnTimeout:       opts.FetchKeepaliveTimeout,
		TLSHandshakeTimeout:   opts.FetchConnectTimeout,
		MaxIdleConnsPerHost:   1,
		ForceAttemptHTTP2:     false,
		TLSClientConfig:       &tls.Config{ServerName: vt.Hostname},
	}
	_ = ctx
	return tr, nil
}
