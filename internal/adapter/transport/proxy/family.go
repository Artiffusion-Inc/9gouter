package proxy

import (
	"net"
	"strings"
)

// stripIPv6Brackets removes surrounding brackets from an IPv6 literal host.
func stripIPv6Brackets(host string) string {
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		return host[1 : len(host)-1]
	}
	return host
}

// detectIPLiteralFamily returns 4 or 6 if host is an IP literal (brackets tolerated), otherwise 0.
func detectIPLiteralFamily(host string) int {
	v := net.ParseIP(stripIPv6Brackets(host))
	if v == nil {
		return 0
	}
	if v.To4() != nil {
		return 4
	}
	return 6
}

// noProxyMatch returns true if hostname matches a comma-separated NO_PROXY list.
func noProxyMatch(hostname, noProxy string) bool {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	for _, p := range strings.Split(noProxy, ",") {
		pattern := strings.ToLower(strings.TrimSpace(p))
		if pattern == "" {
			continue
		}
		if pattern == "*" {
			return true
		}
		if strings.HasPrefix(pattern, ".") {
			if strings.HasSuffix(hostname, pattern) || hostname == pattern[1:] {
				return true
			}
			continue
		}
		if hostname == pattern || strings.HasSuffix(hostname, "."+pattern) {
			return true
		}
	}
	return false
}

// defaultPortForScheme returns the conventional port for a proxy/URL scheme.
func defaultPortForScheme(scheme string) string {
	switch strings.ToLower(strings.TrimSuffix(scheme, ":")) {
	case "https":
		return "443"
	case "socks5", "socks5h":
		return "1080"
	case "http":
		return "8080"
	default:
		return "8080"
	}
}
