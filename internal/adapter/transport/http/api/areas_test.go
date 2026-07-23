package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
)

// Auth checks for all remaining area groups.
func TestRemainingAreas_AuthRequired(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	paths := []string{
		"/api/cli-tools/all-statuses",
		"/api/cli-tools/antigravity-mitm",
		"/api/cli-tools/codex-settings",
		"/api/headroom/status",
		"/api/headroom/proxy/foo",
		"/api/mcp/test/message",
		"/api/mcp/test/sse",
		"/api/media-providers/tts/voices",
		"/api/oauth/codex/bulk-import",
		"/api/oauth/cursor/import",
		"/api/pxpipe/health",
		"/api/tunnel/status",
		"/api/translator/console-logs",
		"/api/translator/load?file=1_req_client.json",
		"/api/v1beta/models",
		"/api/v1beta/models/foo/bar",
		"/api/providers/suggested-models",
		"/api/usage/chart?period=7d",
		"/api/usage/stream",
		"/api/settings/database",
	}
	for _, path := range paths {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401; body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

// Happy-path smoke tests for newly ported areas.
func TestCliTools_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterCliTools(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	for _, tc := range []struct {
		method, path string
		body         string
	}{
		{"GET", "/api/cli-tools/all-statuses", ""},
		{"GET", "/api/cli-tools/antigravity-mitm", ""},
		{"POST", "/api/cli-tools/antigravity-mitm", `{"tool":"claude","action":"enable"}`},
		{"DELETE", "/api/cli-tools/antigravity-mitm", ""},
		{"PATCH", "/api/cli-tools/antigravity-mitm", `{"tool":"claude","action":"enable"}`},
		{"GET", "/api/cli-tools/antigravity-mitm/alias", ""},
		{"GET", "/api/cli-tools/claude-settings", ""},
		{"GET", "/api/cli-tools/codex-settings", ""},
		{"GET", "/api/cli-tools/opencode-settings", ""},
	} {
		var body *strings.Reader
		if tc.body != "" {
			body = strings.NewReader(tc.body)
		} else {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(tc.method, tc.path, body)
		if tc.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s status = %d, want 200; body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestHeadroom_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterHeadroom(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	for _, path := range []string{"/api/headroom/status", "/api/headroom/extras"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
		}
	}

	for _, path := range []string{"/api/headroom/start", "/api/headroom/stop", "/api/headroom/restart"} {
		req := httptest.NewRequest("POST", path, nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		// Headroom lifecycle requires the Headroom CLI binary installed; the
		// handler returns 412 (precondition failed) when it is absent.
		if rec.Code != http.StatusOK && rec.Code != http.StatusPreconditionFailed {
			t.Fatalf("%s status = %d, want 200 or 412; body=%s", path, rec.Code, rec.Body.String())
		}
	}

	// The proxy now reverse-proxies to the Headroom upstream (481e7e46) and
	// gates on a loopback viewer (LOCAL_ONLY). Set a loopback Host so the gate
	// passes; the upstream itself is not running in tests, so we assert only
	// that the gate did not refuse (i.e. not 403). A 5xx means the proxy reached
	// the build-target-URL stage and failed to dial the upstream — expected.
	req := httptest.NewRequest("GET", "/api/headroom/proxy/foo/bar", nil)
	req.Host = "localhost:20127"
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("proxy gated as 403 for a loopback viewer: body=%s", rec.Body.String())
	}
}

func TestMcp_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterMcp(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest("POST", "/api/mcp/test/message", strings.NewReader(`{}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("message status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMediaProviders_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterMediaProviders(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	for _, path := range []string{
		"/api/media-providers/tts/voices",
		"/api/media-providers/tts/deepgram/voices",
		"/api/media-providers/tts/elevenlabs/voices",
		"/api/media-providers/tts/inworld/voices",
		"/api/media-providers/tts/minimax/voices",
	} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestOAuth_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	for _, tc := range []struct {
		method, path, body string
	}{
		{"POST", "/api/oauth/codex/bulk-import", `{"tokens":""}`},
		{"POST", "/api/oauth/cursor/import", `{"tokens":""}`},
		{"POST", "/api/oauth/gitlab/pat", `{"token":""}`},
		{"POST", "/api/oauth/kiro/api-key", `{"apiKey":""}`},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Cookie", "auth_token="+ck)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s status = %d, want 200; body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestPxPipe_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterPxPipe(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	for _, path := range []string{"/api/pxpipe/health", "/api/pxpipe/status", "/api/pxpipe/stats", "/api/pxpipe/logs"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestTunnel_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterTunnel(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	for _, path := range []string{"/api/tunnel/status", "/api/tunnel/tailscale-check"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
		}
	}

	for _, path := range []string{"/api/tunnel/enable", "/api/tunnel/disable", "/api/tunnel/tailscale-enable", "/api/tunnel/tailscale-disable", "/api/tunnel/tailscale-install"} {
		req := httptest.NewRequest("POST", path, nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		// Cloudflare tunnel + Tailscale orchestration is not implemented in
		// the Go backend; the handlers return 501 with a clear message rather
		// than a fake 200 stub.
		if rec.Code != http.StatusOK && rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s status = %d, want 200 or 501; body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestTranslator_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterTranslator(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// /api/models/test is registered under RegisterModels, not RegisterTranslator.
	// Verify translator routes independently.
	req := httptest.NewRequest("GET", "/api/translator/console-logs", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("console-logs status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestV1Beta_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterV1Beta(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	for _, path := range []string{"/api/v1beta/models", "/api/v1beta/models/foo/bar"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestModelsExtra_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterModels(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	for _, path := range []string{"/api/models/availability"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestAuthOidc_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterAuth(mux, deps, config.Config{})

	for _, path := range []string{"/api/auth/oidc/start", "/api/auth/oidc/callback", "/api/auth/oidc/test"} {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		// OIDC endpoints return 503/400 when OIDC is not configured (no
		// issuer/clientID/clientSecret) — a real, honest response instead of
		// the prior 200 stub.
		if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 200/400/503; body=%s", path, rec.Code, rec.Body.String())
		}
	}
}
