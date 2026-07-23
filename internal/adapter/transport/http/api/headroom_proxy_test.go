package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// headroom_proxy_test.go covers the 481e7e46 reverse-proxy: target URL
// construction, hop-by-hop + credential stripping, dashboard HTML rewrite, the
// LOCAL_ONLY loopback gate, and the end-to-end passthrough against a real
// httptest upstream server (no dependency mocks).

// proxyViewerRequest builds an inbound request shaped like the 9Router mux
// delivers it to the headroom proxy handler: URL.Path is the full
// /api/headroom/proxy/<sub> route and Host is the viewer's host (used by the
// LOCAL_ONLY loopback gate).
func proxyViewerRequest(method, path string, body io.Reader, host string) *http.Request {
	r := httptest.NewRequest(method, path, body)
	r.RequestURI = ""
	r.Host = host
	return r
}

// TestBuildHeadroomTargetURL verifies the target is base + stripped sub-path +
// original query, and that a non-http(s) scheme is rejected.
func TestBuildHeadroomTargetURL(t *testing.T) {
	cases := []struct {
		base   string
		path   string
		query  string
		want   string
		scheme bool // expect scheme error
	}{
		{"http://localhost:8787", "/api/headroom/proxy/stats", "limit=10", "http://localhost:8787/stats?limit=10", false},
		{"http://localhost:8787", "/api/headroom/proxy/dashboard", "", "http://localhost:8787/dashboard", false},
		{"https://hr.example.com", "/api/headroom/proxy/transformations/feed", "x=1", "https://hr.example.com/transformations/feed?x=1", false},
		{"ftp://localhost", "/api/headroom/proxy/stats", "", "", true},
	}
	for _, c := range cases {
		u, err := buildHeadroomTargetURL(c.base, c.path, c.query)
		if c.scheme {
			if err == nil {
				t.Errorf("buildHeadroomTargetURL(%q) expected scheme error, got %v", c.base, u)
			}
			continue
		}
		if err != nil {
			t.Fatalf("buildHeadroomTargetURL(%q): %v", c.base, err)
		}
		if got := u.String(); got != c.want {
			t.Errorf("buildHeadroomTargetURL(%q,%q,%q) = %q, want %q", c.base, c.path, c.query, got, c.want)
		}
	}
}

// TestHeadroomProxySubPath verifies the /api/headroom/proxy/ prefix strip.
func TestHeadroomProxySubPath(t *testing.T) {
	cases := map[string]string{
		"/api/headroom/proxy/stats":                "stats",
		"/api/headroom/proxy/dashboard":            "dashboard",
		"/api/headroom/proxy/transformations/feed": "transformations/feed",
		"/api/headroom/proxy/":                     "",
		"/api/headroom/proxy":                      "",
	}
	for in, want := range cases {
		if got := headroomProxySubPath(in); got != want {
			t.Errorf("headroomProxySubPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestForwardedHeadroomHeadersStripsHopByHopAndHost verifies hop-by-hop + Host
// are dropped and other headers forwarded.
func TestForwardedHeadroomHeadersStripsHopByHopAndHost(t *testing.T) {
	in := http.Header{}
	in.Set("Connection", "keep-alive")
	in.Set("Transfer-Encoding", "chunked")
	in.Set("Upgrade", "h2c")
	in.Set("Host", "should-be-dropped")
	in.Set("X-Custom", "keep-me")
	in.Set("Cookie", "auth=1")
	in.Set("Authorization", "Bearer viewer")
	target, _ := buildHeadroomTargetURL("http://localhost:8787", "/api/headroom/proxy/stats", "")
	out := forwardedHeadroomHeaders(in, target)
	for _, hop := range []string{"Connection", "Transfer-Encoding", "Upgrade", "Host"} {
		if out.Get(hop) != "" {
			t.Errorf("hop-by-hop/host header %q leaked: %q", hop, out.Get(hop))
		}
	}
	if out.Get("X-Custom") != "keep-me" {
		t.Errorf("X-Custom should survive, got %q", out.Get("X-Custom"))
	}
	// Loopback target keeps viewer credentials.
	if out.Get("Cookie") != "auth=1" {
		t.Errorf("Cookie should survive for loopback target, got %q", out.Get("Cookie"))
	}
}

// TestForwardedHeadroomHeadersStripsCredentialsForNonLoopback verifies
// Cookie/Authorization are stripped when the Headroom target is non-loopback.
func TestForwardedHeadroomHeadersStripsCredentialsForNonLoopback(t *testing.T) {
	in := http.Header{}
	in.Set("Cookie", "auth=1")
	in.Set("Authorization", "Bearer viewer")
	target, _ := buildHeadroomTargetURL("https://hr.example.com", "/api/headroom/proxy/stats", "")
	out := forwardedHeadroomHeaders(in, target)
	if out.Get("Cookie") != "" {
		t.Errorf("Cookie should be stripped for non-loopback target, got %q", out.Get("Cookie"))
	}
	if out.Get("Authorization") != "" {
		t.Errorf("Authorization should be stripped for non-loopback target, got %q", out.Get("Authorization"))
	}
}

// TestRewriteDashboardHTML verifies same-origin fetches are prefixed.
func TestRewriteDashboardHTML(t *testing.T) {
	html := []byte(`<script>fetch('/stats');fetch('/health');fetch('/stats-history');fetch('/transformations/feed')</script>`)
	out := string(rewriteDashboardHTML(html))
	for _, ep := range []string{"/stats", "/health", "/stats-history", "/transformations/feed"} {
		want := "fetch('" + dashboardProxyPrefix + ep
		if !strings.Contains(out, want) {
			t.Errorf("rewrite did not prefix %q: %s", ep, out)
		}
	}
	// Ensure a bare data fetch no longer exists.
	for _, ep := range []string{"fetch('/stats')", "fetch('/health')", "fetch('/stats-history')", "fetch('/transformations/feed')"} {
		if strings.Contains(out, ep) {
			t.Errorf("bare fetch %q should be rewritten away: %s", ep, out)
		}
	}
}

// TestRewriteDashboardHTMLNonMatchingUnchanged verifies non-data fetches are
// not touched.
func TestRewriteDashboardHTMLNonMatchingUnchanged(t *testing.T) {
	html := []byte(`fetch('/api/keys');fetch('/some/other')`)
	out := string(rewriteDashboardHTML(html))
	if out != string(html) {
		t.Errorf("non-data-endpoint HTML should be unchanged, got %s", out)
	}
}

// TestIsLocalProxyRequest verifies the loopback gate.
func TestIsLocalProxyRequest(t *testing.T) {
	cases := []struct {
		name   string
		host   string
		origin string
		via    string
		want   bool
	}{
		{"loopback host", "localhost:20127", "", "", true},
		{"127.0.0.1 host", "127.0.0.1:20127", "", "", true},
		{"remote host", "example.com", "", "", false},
		{"loopback host remote origin", "localhost:20127", "https://evil.com", "", false},
		{"loopback host loopback origin", "localhost:20127", "http://localhost:8787", "", true},
		{"via-proxy stamp blocks", "localhost:20127", "", "1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/headroom/proxy/stats", nil)
			r.RequestURI = ""
			r.Host = c.host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if c.via != "" {
				r.Header.Set("X-9r-Via-Proxy", c.via)
			}
			if got := isLocalProxyRequest(r); got != c.want {
				t.Errorf("isLocalProxyRequest = %v, want %v", got, c.want)
			}
		})
	}
}

// TestIsLoopbackHostname covers the loopback host classifier.
func TestIsLoopbackHostname(t *testing.T) {
	cases := map[string]bool{
		"localhost":      true,
		"127.0.0.1":      true,
		"localhost:8787": true,
		"example.com":    false,
		"":               false,
	}
	for in, want := range cases {
		if got := isLoopbackHostname(in); got != want {
			t.Errorf("isLoopbackHostname(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestProxyLocalOnlyPassthrough runs the full handler against a real httptest
// upstream, asserting status, body, and forwarded method/path.
func TestProxyLocalOnlyPassthrough(t *testing.T) {
	var gotMethod, gotPath, gotCookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "yes")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	os.Setenv("HEADROOM_URL", upstream.URL)
	defer os.Unsetenv("HEADROOM_URL")

	// Loopback viewer + loopback upstream → credentials kept.
	r := proxyViewerRequest(http.MethodGet, "/api/headroom/proxy/stats", nil, "localhost:20127")
	r.Header.Set("Cookie", "viewer=1")
	rec := httptest.NewRecorder()
	proxyLocalOnly(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotMethod != http.MethodGet {
		t.Errorf("forwarded method = %q, want GET", gotMethod)
	}
	if gotPath != "/stats" {
		t.Errorf("forwarded path = %q, want /stats", gotPath)
	}
	if gotCookie != "viewer=1" {
		t.Errorf("cookie should be forwarded to loopback upstream, got %q", gotCookie)
	}
	if rec.Header().Get("X-Upstream") != "yes" {
		t.Errorf("upstream response header not copied: %+v", rec.Header())
	}
}

// TestProxyLocalOnlyNonLoopbackViewerRefused verifies a remote viewer is
// refused by the LOCAL_ONLY gate before reaching the upstream.
func TestProxyLocalOnlyNonLoopbackViewerRefused(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer upstream.Close()
	os.Setenv("HEADROOM_URL", upstream.URL)
	defer os.Unsetenv("HEADROOM_URL")

	r := proxyViewerRequest(http.MethodGet, "/api/headroom/proxy/stats", nil, "example.com:20127")
	rec := httptest.NewRecorder()
	proxyLocalOnly(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Error("upstream should not be called for a non-loopback viewer")
	}
	if !strings.Contains(rec.Body.String(), "Local only") {
		t.Errorf("body = %s, want 'Local only' message", rec.Body.String())
	}
}

// TestProxyLocalOnlyNonLoopbackUpstreamStripsCredentials verifies credentials
// are stripped when the configured Headroom target is non-loopback. The strip
// is keyed on the *configured* Headroom URL, so we assert via
// forwardedHeadroomHeaders directly (the resolved upstream socket is not the
// decision input).
func TestProxyLocalOnlyNonLoopbackUpstreamStripsCredentials(t *testing.T) {
	target, _ := buildHeadroomTargetURL("https://hr.example.com", "/api/headroom/proxy/stats", "")
	out := forwardedHeadroomHeaders(http.Header{
		"Cookie":        []string{"viewer=1"},
		"Authorization": []string{"Bearer v"},
	}, target)
	if out.Get("Cookie") != "" || out.Get("Authorization") != "" {
		t.Fatalf("credentials should be stripped for non-loopback target: cookie=%q auth=%q", out.Get("Cookie"), out.Get("Authorization"))
	}
}

// TestProxyLocalOnlyDashboardHTMLRewrite verifies the dashboard path triggers
// the HTML rewrite branch.
func TestProxyLocalOnlyDashboardHTMLRewrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dashboard" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<script>fetch('/stats')</script>`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()
	os.Setenv("HEADROOM_URL", upstream.URL)
	defer os.Unsetenv("HEADROOM_URL")

	r := proxyViewerRequest(http.MethodGet, "/api/headroom/proxy/dashboard", nil, "localhost:20127")
	rec := httptest.NewRecorder()
	proxyLocalOnly(rec, r)
	if !strings.Contains(rec.Body.String(), dashboardProxyPrefix+"/stats") {
		t.Errorf("dashboard HTML not rewritten: %s", rec.Body.String())
	}
	// Content-Length must be dropped since the body changed.
	if rec.Header().Get("Content-Length") != "" {
		t.Errorf("Content-Length should be dropped on rewrite, got %q", rec.Header().Get("Content-Length"))
	}
}

// TestProxyLocalOnlyPOSTBody verifies request bodies are forwarded for
// non-GET methods.
func TestProxyLocalOnlyPOSTBody(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	os.Setenv("HEADROOM_URL", upstream.URL)
	defer os.Unsetenv("HEADROOM_URL")

	body := strings.NewReader(`{"q":"hi"}`)
	r := proxyViewerRequest(http.MethodPost, "/api/headroom/proxy/stats", body, "localhost:20127")
	rec := httptest.NewRecorder()
	proxyLocalOnly(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if gotBody != `{"q":"hi"}` {
		t.Errorf("forwarded body = %q, want %q", gotBody, `{"q":"hi"}`)
	}
}

// TestProxyLocalOnlyManualRedirect verifies the proxy does not follow upstream
// redirects (redirect: "manual" semantics).
func TestProxyLocalOnlyManualRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer upstream.Close()
	os.Setenv("HEADROOM_URL", upstream.URL)
	defer os.Unsetenv("HEADROOM_URL")

	r := proxyViewerRequest(http.MethodGet, "/api/headroom/proxy/stats", nil, "localhost:20127")
	rec := httptest.NewRecorder()
	proxyLocalOnly(rec, r)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (manual, not followed)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasSuffix(loc, "/elsewhere") {
		t.Errorf("Location header = %q, want .../elsewhere", loc)
	}
}
