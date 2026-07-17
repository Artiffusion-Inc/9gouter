package proxy

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"
)

// tcpCheckFn is overridable in tests.
var tcpCheckFn = tcpCheck

// tcpCheck attempts a TCP connection to host:port within timeout. It returns nil
// on success, otherwise a non-nil error.
func tcpCheck(ctx context.Context, host string, port int, timeout time.Duration) error {
	dialer := &net.Dialer{Timeout: timeout}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// FastFail performs a TCP reachability check on a proxy URL. It returns nil if
// the proxy's TCP port is reachable within ProxyFastFailTimeout, otherwise an
// error. Results are cached via the Health cache.
func FastFail(ctx context.Context, opts Options, proxyRaw string) error {
	health := GlobalHealth(opts)
	if cached, ok := health.Get(proxyRaw); ok {
		if cached {
			return nil
		}
		return fmt.Errorf("proxy %s cached unreachable", ProxyURLForLogs(proxyRaw))
	}

	u, err := url.Parse(proxyRaw)
	if err != nil {
		health.Set(proxyRaw, false)
		return fmt.Errorf("invalid proxy URL %q: %w", proxyRaw, err)
	}
	host := stripIPv6Brackets(u.Hostname())
	portStr := u.Port()
	if portStr == "" {
		portStr = defaultPortForScheme(u.Scheme)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		health.Set(proxyRaw, false)
		return fmt.Errorf("invalid proxy port %q: %w", portStr, err)
	}

	err = health.Probe(ctx, proxyRaw, func(ctx context.Context) error {
		return tcpCheckFn(ctx, host, port, opts.ProxyFastFailTimeout)
	})
	return err
}
