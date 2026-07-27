package http

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	domainauth "github.com/Artiffusion-Inc/9gouter/internal/domain/auth"
)

// DashboardGuard ports src/dashboardGuard.js:213-252. It wraps the static
// dashboard handler and enforces, for any path under /dashboard, the same
// requireLogin + tunnelDashboardAccess logic the legacy JS middleware applied
// in front of the Next.js dashboard:
//
//  1. If settings.tunnelDashboardAccess is false AND the request Host (port
//     stripped) equals the hostname of settings.tunnelUrl or settings.tailscaleUrl,
//     redirect to /login (block tunnel/tailscale exposure of the dashboard).
//  2. If settings.requireLogin is false, allow through (no session needed).
//  3. Otherwise require a valid auth_token session cookie; on failure redirect
//     to /login.
//
// The gate reads the settings blob once per request through the existing
// requireLoginGate's settingsReader (no separate cache: the requireLogin gate
// already caches requireLogin; tunnelUrl/tailscaleUrl/tunnelDashboardAccess
// are read alongside it and are stable between settings PATCHes). On any
// settings read/parse error it keeps the safe defaults (require login, do not
// block tunnel) — matching dashboardGuard.js's catch{} fallback.
//
// Paths NOT under /dashboard (the SPA assets, /login itself, /v1, /api) pass
// through unchanged; /api/* is already gated by APIMiddleware and /v1 by the
// API-key gate.
type DashboardGuard struct {
	next     http.Handler
	store    domainauth.Store
	settings settingsReader
}

// NewDashboardGuard wraps a static dashboard handler with the dashboard auth
// gate. store may be nil — in that case a requireLogin=false setting still
// lets the dashboard through, but a requireLogin=true setting can never
// validate a session and always redirects to /login (fail-closed). A nil
// settings reader preserves the deny-by-default behaviour (require login).
func NewDashboardGuard(next http.Handler, store domainauth.Store, settings settingsReader) *DashboardGuard {
	return &DashboardGuard{next: next, store: store, settings: settings}
}

func (g *DashboardGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only /dashboard* is gated. Everything else (root index, _next assets,
	// /login) passes through — mirrors dashboardGuard.js only acting on
	// pathname.startsWith("/dashboard").
	p := r.URL.Path
	if !strings.HasPrefix(p, "/dashboard") {
		g.next.ServeHTTP(w, r)
		return
	}

	requireLogin := true
	tunnelDashboardAccess := true
	var tunnelHost, tailscaleHost string

	if g.settings != nil {
		if s, err := g.settings.Get(r.Context()); err == nil && len(s.Data) > 0 {
			var obj map[string]any
			if err := json.Unmarshal(s.Data, &obj); err == nil {
				if v, ok := obj["requireLogin"].(bool); ok {
					requireLogin = v
				}
				if v, ok := obj["tunnelDashboardAccess"].(bool); ok {
					tunnelDashboardAccess = v
				}
				tunnelHost = hostnameOf(obj["tunnelUrl"])
				tailscaleHost = hostnameOf(obj["tailscaleUrl"])
			}
		}
	}

	// 1. Block tunnel/tailscale exposure when the operator disabled it.
	if !tunnelDashboardAccess {
		host := r.Host
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" && ((tunnelHost != "" && host == tunnelHost) || (tailscaleHost != "" && host == tailscaleHost)) {
			redirectToLogin(w, r)
			return
		}
	}

	// 2. requireLogin disabled → open dashboard.
	if !requireLogin {
		g.next.ServeHTTP(w, r)
		return
	}

	// 3. Require a valid session cookie.
	if g.store != nil {
		if _, err := g.store.Get(r); err == nil {
			g.next.ServeHTTP(w, r)
			return
		}
	}
	redirectToLogin(w, r)
}

// hostnameOf extracts the lowercase hostname from a settings URL string,
// returning "" for an empty/invalid URL. Mirrors dashboardGuard.js
// `new URL(settings.tunnelUrl).hostname.toLowerCase()`.
func hostnameOf(v any) string {
	s, ok := v.(string)
	if !ok || s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	// Preserve the original target so /login can return the user to it.
	target := r.URL.RequestURI()
	login := "/login"
	if target != "" && target != "/dashboard" {
		login = "/login?redirect=" + url.QueryEscape(target)
	}
	http.Redirect(w, r, login, http.StatusSeeOther)
}
