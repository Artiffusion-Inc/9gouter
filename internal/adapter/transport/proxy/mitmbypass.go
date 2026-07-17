package proxy

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// defaultBypassHosts matches open-sse/utils/proxyFetch.js.
var defaultBypassHosts = []string{
	"cloudcode-pa.googleapis.com",
	"daily-cloudcode-pa.googleapis.com",
	"api.individual.githubcopilot.com",
	"q.us-east-1.amazonaws.com",
	"codewhisperer.us-east-1.amazonaws.com",
	"api2.cursor.sh",
}

// MITMBypassHosts is overridable by callers; defaults to the JS list.
var MITMBypassHosts = defaultBypassHosts

// MITMBypassResolve checks whether host is a known MITM-bypass host and resolves
// it via Google DNS (8.8.8.8:53) using a custom net.Resolver to avoid /etc/hosts
// spoofing. It returns the first IPv4 address.
func MITMBypassResolve(host string) (net.IP, error) {
	if !shouldBypassMitmDns(host) {
		return nil, fmt.Errorf("host %s is not in MITM bypass list", host)
	}

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "udp", "8.8.8.8:53")
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	addrs, err := resolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil, fmt.Errorf("MITM bypass DNS resolve %s: %w", host, err)
	}
	for _, addr := range addrs {
		ip := addr.AsSlice()
		if ip != nil {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("MITM bypass DNS resolve %s: no IPv4 addresses", host)
}

// shouldBypassMitmDns returns true if host (or a substring) is in the bypass list.
func shouldBypassMitmDns(host string) bool {
	for _, h := range MITMBypassHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}
