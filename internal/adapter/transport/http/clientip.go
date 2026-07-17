package http

import (
	"net"
	"net/http"
	"strings"
)

// FromRequest returns the client IP for request logging and rate-limiting keys.
// It ports custom-server.js: trust X-Real-IP / X-Forwarded-For only when the TCP
// peer is a loopback address (local reverse proxy); otherwise return the socket IP.
func FromRequest(r *http.Request) string {
	socketIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	if socketIP == "" {
		socketIP = r.RemoteAddr
	}
	if !isLoopback(socketIP) {
		return socketIP
	}

	if xRealIP := strings.TrimSpace(r.Header.Get("X-Real-Ip")); xRealIP != "" {
		return xRealIP
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		first := strings.TrimSpace(strings.Split(xff, ",")[0])
		if first != "" {
			return first
		}
	}
	return socketIP
}

func isLoopback(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip == "localhost" || ip == "127.0.0.1" || ip == "::1"
	}
	return parsed.IsLoopback()
}
