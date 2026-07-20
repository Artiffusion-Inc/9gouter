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
func ProxyAwareFetch(ctx context.Context, client *http.Client, req *http.Request, opts Options, proxyOpts ProxyFetchOptions, fallback *Fallback) (*http.Response, error) {
	originalURL := req.URL.String()

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
