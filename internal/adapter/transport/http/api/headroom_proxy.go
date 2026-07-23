package api

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// headroom_proxy.go ports src/app/api/headroom/proxy/[...path]/route.js (#2372 /
// 481e7e46). The 9Router app proxies the Headroom dashboard and its data
// endpoints (/stats, /health, /stats-history, /transformations/feed) so they
// stay same-origin when the dashboard is opened remotely. The proxy:
//
//   - resolves the Headroom base from settings (HEADROOM_URL or the loopback
//     default) and rebuilds the target URL from {base}/{path}?{search};
//   - forwards the method, body, and forwarded headers, stripping hop-by-hop
//     and Host headers;
//   - strips Cookie/Authorization when the Headroom target is non-loopback so
//     viewer credentials are not leaked to an external host;
//   - follows redirects manually (redirect: "manual") so the dashboard's own
//     relative redirects are not blindly re-issued by the client;
//   - rewrites the dashboard HTML so same-origin fetches inside the page hit
//     /api/headroom/proxy/<path> instead of the bare Headroom paths.
//
// LOCAL_ONLY gate: the JS dashboardGuard restricts /api/headroom/proxy to a
// valid CLI token (machineId-derived) OR a loopback request that is also
// session-authenticated. The Go build has no machineId/CLI-secret subsystem
// yet, so the CLI-token branch is deferred (see docs/upstream-gap-list.md);
// the loopback branch is enforced here as the security-meaningful floor. The
// session-auth half is already provided by APIMiddleware for /api/* routes.

// hopByHopHeaders mirrors the JS HOP_BY_HOP_HEADERS set.
var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

// dashboardProxyPrefix is the 9Router-side mount path; rewritten dashboard
// fetches are prefixed with it.
const dashboardProxyPrefix = "/api/headroom/proxy"

// proxyLocalOnly is the real reverse-proxy behind h.proxy. It returns an
// error response (JSON) on failure so the dashboard UI gets a structured
// message rather than an empty 200, matching the JS `catch` branch.
func proxyLocalOnly(w http.ResponseWriter, r *http.Request) {
	// LOCAL_ONLY gate: restrict to loopback viewers. Without the CLI-token
	// subsystem (deferred), non-loopback callers are refused outright so a
	// remote browser cannot drive the dashboard proxy. Session auth is handled
	// upstream by APIMiddleware.
	if !isLocalProxyRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "Local only: CLI token required",
		})
		return
	}

	base := headroomURLFromSettings()
	target, err := buildHeadroomTargetURL(base, r.URL.Path, r.URL.RawQuery)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	req, err := http.NewRequest(r.Method, target.String(), r.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	req.Header = forwardedHeadroomHeaders(r.Header, target)

	client := &http.Client{
		// redirect: "manual" — do not follow upstream redirects.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	// Strip hop-by-hop headers from the upstream response before copying.
	for k := range resp.Header {
		if _, hop := hopByHopHeaders[strings.ToLower(k)]; hop {
			resp.Header.Del(k)
		}
	}

	// Dashboard HTML rewrite: same-origin fetches for the data endpoints.
	path := headroomProxySubPath(r.URL.Path)
	if path == "dashboard" {
		ct := resp.Header.Get("Content-Type")
		if strings.Contains(ct, "text/html") {
			body, rerr := io.ReadAll(resp.Body)
			if rerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": rerr.Error()})
				return
			}
			resp.Header.Del("Content-Length")
			copyHeader(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(rewriteDashboardHTML(body))
			return
		}
	}

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// buildHeadroomTargetURL reconstructs the upstream URL from the Headroom base,
// the proxied sub-path, and the original query string. path is the full
// /api/headroom/proxy/<sub...> path; the /api/headroom/proxy prefix is
// stripped so the upstream sees its own route shape.
func buildHeadroomTargetURL(base, path, rawQuery string) (*url.URL, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return nil, fmt.Errorf("Headroom URL must use http or https")
	}
	sub := headroomProxySubPath(path)
	target := *baseURL
	target.Path = "/" + sub
	target.RawQuery = rawQuery
	return &target, nil
}

// headroomProxySubPath strips the /api/headroom/proxy prefix from a proxied
// path and returns the Headroom-side sub-path (e.g. "stats", "dashboard"). The
// bare proxy root (/api/headroom/proxy or /api/headroom/proxy/) yields "".
func headroomProxySubPath(path string) string {
	const prefix = "/api/headroom/proxy"
	if path == prefix {
		return ""
	}
	s := strings.TrimPrefix(path, prefix+"/")
	// A request to the bare proxy root has no sub-path.
	s = strings.TrimPrefix(s, "/")
	return s
}

// forwardedHeadroomHeaders copies the inbound headers, dropping hop-by-hop and
// Host, and stripping Cookie/Authorization when the target is non-loopback so
// viewer credentials are not leaked to an external Headroom host.
func forwardedHeadroomHeaders(in http.Header, target *url.URL) http.Header {
	out := http.Header{}
	for k, vs := range in {
		if _, hop := hopByHopHeaders[strings.ToLower(k)]; hop {
			continue
		}
		if strings.EqualFold(k, "Host") {
			continue
		}
		out[k] = append(out[k], vs...)
	}
	if !isLoopbackHeadroomURL(target.String()) {
		out.Del("Cookie")
		out.Del("Authorization")
	}
	return out
}

// rewriteDashboardHTML rewrites the dashboard's same-origin fetches so they go
// through /api/headroom/proxy instead of the bare Headroom paths. Mirrors the
// JS rewriteDashboardHtml regex: fetch('/stats|/health|/stats-history|
// /transformations/feed) → fetch('/api/headroom/proxy/stats|...).
func rewriteDashboardHTML(html []byte) []byte {
	s := string(html)
	// Match fetch(' followed by one of the data-endpoint paths, preserving the
	// leading slash. Replace by inserting the proxy prefix after the quote.
	for _, ep := range []string{"/stats", "/health", "/stats-history", "/transformations/feed"} {
		needle := "fetch('" + ep
		repl := "fetch('" + dashboardProxyPrefix + ep
		s = strings.ReplaceAll(s, needle, repl)
	}
	return []byte(s)
}

// isLocalProxyRequest ports the loopback half of dashboardGuard.canAccessLocalOnlyRoute:
// the request must originate from a loopback peer (and not arrive via a
// reverse-proxy hop, which the X-9r-Via-Proxy stamp marks). The CLI-token half
// is deferred until the machineId/CLI-secret subsystem is ported.
func isLocalProxyRequest(r *http.Request) bool {
	if r.Header.Get("X-9r-Via-Proxy") != "" {
		return false
	}
	host := r.Host
	if host == "" {
		host = r.Header.Get("Host")
	}
	if !isLoopbackHostname(host) {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !isLoopbackHostname(u.Hostname()) {
			return false
		}
	}
	return true
}

// isLoopbackHostname reports whether h is a loopback host, mirroring the JS
// isLoopbackHostname (strip :port, strip IPv6 brackets, lowercase, compare).
func isLoopbackHostname(h string) bool {
	if h == "" {
		return false
	}
	name := h
	if i := strings.LastIndex(name, ":"); i >= 0 {
		// Only strip if the tail is a port (all digits) OR the host is bracketed
		// IPv6 ([::1]:port / [::1]). For bare [::1] with no port, the bracket
		// strip below handles it.
		if strings.HasPrefix(name, "[") {
			// [host]:port or [host]
			if j := strings.Index(name, "]"); j >= 0 {
				name = name[1:j]
			}
		} else if isAllDigits(name[i+1:]) {
			name = name[:i]
		}
	}
	name = strings.TrimPrefix(name, "[")
	if i := strings.Index(name, "]"); i >= 0 {
		name = name[:i]
	}
	name = strings.ToLower(name)
	_, ok := loopbackHosts[name]
	return ok
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// copyHeader copies src into dst without overwriting existing keys.
func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
