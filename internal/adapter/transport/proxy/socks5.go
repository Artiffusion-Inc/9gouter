package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"golang.org/x/net/proxy"
)

// Socks5Transport builds an *http.Transport that routes through a SOCKS5 proxy.
// Family pinning is applied to the forward dialer so the TCP connection to the
// SOCKS server honours a configured IPv4/IPv6 policy.
func Socks5Transport(opts Options, proxyRaw string) (*http.Transport, error) {
	p, err := NormalizeProxyURL(proxyRaw)
	if err != nil {
		return nil, err
	}

	baseDialer := &net.Dialer{
		Timeout:   opts.FetchConnectTimeout,
		KeepAlive: opts.FetchKeepaliveTimeout,
	}

	family := FamilyAuto
	literalFamily := detectIPLiteralFamily(p.Host)
	if literalFamily != 0 {
		family = Family(fmt.Sprintf("ipv%d", literalFamily))
	}
	forward := &familyPinDialer{base: baseDialer, family: family}

	addr := net.JoinHostPort(p.Host, p.Port)
	var auth *proxy.Auth
	if p.Username != "" {
		auth = &proxy.Auth{User: p.Username, Password: p.Password}
	}

	socksDialer, err := proxy.SOCKS5("tcp", addr, auth, forward)
	if err != nil {
		return nil, fmt.Errorf("socks5 dialer: %w", err)
	}

	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if ctxDialer, ok := socksDialer.(proxy.ContextDialer); ok {
				return ctxDialer.DialContext(ctx, network, address)
			}
			return socksDialer.Dial(network, address)
		},
		ResponseHeaderTimeout: opts.FetchHeadersTimeout,
		IdleConnTimeout:       opts.FetchKeepaliveTimeout,
		TLSHandshakeTimeout:   opts.FetchConnectTimeout,
		MaxIdleConnsPerHost:   1,
		ForceAttemptHTTP2:     false,
	}
	return tr, nil
}
