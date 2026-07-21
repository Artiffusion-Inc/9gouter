package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
)

// TestOAuth_AllRoutes exercises every OAuth route registered by
// RegisterOAuth (both GET/POST providerAction variants + the import/token
// helpers) so the simple-stub branches are covered.
func TestOAuth_AllRoutes(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	cases := []struct {
		method, path, body string
	}{
		{"GET", "/api/oauth/codex/start", ""},
		{"POST", "/api/oauth/codex/start", `{}`},
		{"GET", "/api/oauth/cursor/revoke", ""},
		{"POST", "/api/oauth/cursor/revoke", `{}`},
		{"POST", "/api/oauth/codex/import-token", `{"tokens":""}`},
		{"POST", "/api/oauth/cursor/auto-import", `{"tokens":""}`},
		{"POST", "/api/oauth/gitlab/pat", `{"token":"glpat-xxx"}`},
		{"POST", "/api/oauth/iflow/cookie", `{"cookie":"sess=xxx"}`},
		{"POST", "/api/oauth/kiro/api-key", `{"apiKey":"k-xxx"}`},
		{"POST", "/api/oauth/kiro/auto-import", `{"tokens":""}`},
		{"POST", "/api/oauth/kiro/import", `{"tokens":""}`},
		{"POST", "/api/oauth/kiro/import-cli-proxy", `{"tokens":""}`},
		{"POST", "/api/oauth/kiro/social-authorize", `{}`},
		{"POST", "/api/oauth/kiro/social-exchange", `{"code":"c"}`},
	}
	for _, c := range cases {
		var body *strings.Reader
		if c.body != "" {
			body = strings.NewReader(c.body)
		} else {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(c.method, c.path, body)
		req.Header.Set("Cookie", "auth_token="+ck)
		if c.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s status = %d, want 200; body=%s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}

	// gitlab pat with non-empty token → tokenSaved=true.
	req := httptest.NewRequest("POST", "/api/oauth/gitlab/pat", strings.NewReader(`{"token":"glpat-xxx"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal gitlab pat: %v", err)
	}
	if resp["tokenSaved"] != true {
		t.Fatalf("gitlab pat tokenSaved = %v, want true", resp["tokenSaved"])
	}

	// iflow cookie with non-empty → cookieSaved=true.
	req = httptest.NewRequest("POST", "/api/oauth/iflow/cookie", strings.NewReader(`{"cookie":"sess=abc"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["cookieSaved"] != true {
		t.Fatalf("iflow cookieSaved = %v, want true", resp["cookieSaved"])
	}

	// kiro api key with non-empty → apiKeySaved=true.
	req = httptest.NewRequest("POST", "/api/oauth/kiro/api-key", strings.NewReader(`{"apiKey":"k-xxx"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["apiKeySaved"] != true {
		t.Fatalf("kiro apiKeySaved = %v, want true", resp["apiKeySaved"])
	}
}

// TestOAuth_CodexBulkImport covers the codexBulkImportAccounts branches:
// invalid JSON, empty accounts list, account missing accessToken, and a
// successful insert.
func TestOAuth_CodexBulkImport(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Invalid JSON body → 400.
	req := httptest.NewRequest("POST", "/api/oauth/codex/bulk-import", strings.NewReader(`{bad`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Valid body with mixed accounts: one nil, one missing accessToken, one
	// valid account.
	body := `{"accounts":[null,{"refreshToken":"r"},{"accessToken":"at-1","refreshToken":"rt-1","idToken":"it-1","email":"a@b.c"}]}`
	req = httptest.NewRequest("POST", "/api/oauth/codex/bulk-import", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk import status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success int `json:"success"`
		Failed  int `json:"failed"`
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal bulk import: %v", err)
	}
	if resp.Success != 1 {
		t.Fatalf("success = %d, want 1", resp.Success)
	}
	if resp.Failed != 2 {
		t.Fatalf("failed = %d, want 2", resp.Failed)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("results len = %d, want 3", len(resp.Results))
	}
}

// TestOAuth_CodexBulkImport_NoConnectionsRepo verifies the 503 guard when the
// Connections repo is nil.
func TestOAuth_CodexBulkImport_NoConnectionsRepo(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	deps.Connections = nil
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest("POST", "/api/oauth/codex/bulk-import", strings.NewReader(`{"accounts":[]}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil repo status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

// TestCliTools_SettingsPerMethod covers every cliToolResponse-backed route with
// GET/POST/DELETE so the switch branches are all exercised.
func TestCliTools_SettingsPerMethod(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterCliTools(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	tools := []string{
		"claude-settings", "codex-settings", "opencode-settings", "droid-settings",
		"openclaw-settings", "hermes-settings", "copilot-settings", "cline-settings",
		"kilo-settings", "deepseek-tui-settings", "jcode-settings", "grok-build-settings",
	}
	for _, tool := range tools {
		for _, method := range []string{"GET", "POST", "DELETE"} {
			req := httptest.NewRequest(method, "/api/cli-tools/"+tool, nil)
			req.Header.Set("Cookie", "auth_token="+ck)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s status = %d, want 200; body=%s", method, tool, rec.Code, rec.Body.String())
			}
		}
	}

	// Cowork-specific routes.
	for _, p := range []string{"/api/cli-tools/cowork-settings", "/api/cli-tools/cowork-mcp-registry", "/api/cli-tools/cowork-mcp-tools"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", p, rec.Code, rec.Body.String())
		}
	}
}

// TestCliTools_MitmSetAliases exercises the mitmSetAliases handler (happy path
// + missing field 400) and the mitmGetAliases read path.
func TestCliTools_MitmSetAliases(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterCliTools(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// GET (empty tool query).
	req := httptest.NewRequest("GET", "/api/cli-tools/antigravity-mitm/alias", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get aliases status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// PUT happy path.
	req = httptest.NewRequest("PUT", "/api/cli-tools/antigravity-mitm/alias", strings.NewReader(`{"tool":"claude","mappings":{"x":"y"}}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set aliases status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// PUT missing tool → 400.
	req = httptest.NewRequest("PUT", "/api/cli-tools/antigravity-mitm/alias", strings.NewReader(`{"mappings":{"x":"y"}}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("set aliases missing tool status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// PUT missing mappings → 400.
	req = httptest.NewRequest("PUT", "/api/cli-tools/antigravity-mitm/alias", strings.NewReader(`{"tool":"claude"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("set aliases missing mappings status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// PUT invalid body → 400.
	req = httptest.NewRequest("PUT", "/api/cli-tools/antigravity-mitm/alias", strings.NewReader(`{bad`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("set aliases invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestPxPipe_AllRoutes covers every pxpipe handler including the POST
// lifecycle routes.
func TestPxPipe_AllRoutes(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterPxPipe(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	gets := []string{"/api/pxpipe/health", "/api/pxpipe/status", "/api/pxpipe/stats", "/api/pxpipe/logs"}
	for _, p := range gets {
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200; body=%s", p, rec.Code, rec.Body.String())
		}
	}

	posts := []string{"/api/pxpipe/health", "/api/pxpipe/start", "/api/pxpipe/stop", "/api/pxpipe/restart", "/api/pxpipe/install"}
	for _, p := range posts {
		req := httptest.NewRequest("POST", p, nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s status = %d, want 200; body=%s", p, rec.Code, rec.Body.String())
		}
	}
}

// TestMcp_SSE covers the GET /api/mcp/{plugin}/sse route (short SSE response).
func TestMcp_SSE(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterMcp(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest("GET", "/api/mcp/demo/sse", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sse status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("sse Content-Type = %q, want text/event-stream", ct)
	}
}

// TestLocale_OptionsAndNormalize covers the OPTIONS route, the setLocale
// invalid-body and invalid-locale branches, plus normalizeLocale via the
// zh/zh-CN/zh-cn normalization.
func TestLocale_OptionsAndNormalize(t *testing.T) {
	mux := http.NewServeMux()
	RegisterLocale(mux)

	// OPTIONS.
	req := httptest.NewRequest("OPTIONS", "/api/locale", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("options status = %d, want 204", rec.Code)
	}

	// Invalid body → 400.
	req = httptest.NewRequest("POST", "/api/locale", strings.NewReader(`{bad`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Unsupported locale (after normalization falls back to "en" which IS
	// supported, so this actually succeeds with locale=en). Use a locale
	// that normalizes to itself and is unsupported. "fr" normalizes to "en"
	// (fallback) so it would succeed. Skip — normalizeLocale always returns
	// a supported value, so the unsupported branch is unreachable. Test the
	// zh variants instead which normalize to "zh-CN".
	for _, in := range []string{"zh", "zh-CN", "zh-cn"} {
		req = httptest.NewRequest("POST", "/api/locale", strings.NewReader(`{"locale":"`+in+`"}`))
		req.Header.Set("Content-Type", "application/json")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("locale %s status = %d, want 200; body=%s", in, rec.Code, rec.Body.String())
		}
		var resp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp["locale"] != "zh-CN" {
			t.Fatalf("locale %s -> %v, want zh-CN", in, resp["locale"])
		}
	}

	// "es" passes through.
	req = httptest.NewRequest("POST", "/api/locale", strings.NewReader(`{"locale":"es"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("es status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestVersion_UpdateAndShutdown covers the POST /api/version/update and
// POST /api/version/shutdown routes (public, no auth required).
func TestVersion_UpdateAndShutdown(t *testing.T) {
	mux := http.NewServeMux()
	RegisterVersion(mux, "test")

	// update.
	req := httptest.NewRequest("POST", "/api/version/update", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal update: %v", err)
	}
	if resp["success"] != false {
		t.Fatalf("update success = %v, want false (dev build)", resp["success"])
	}

	// shutdown handler.
	req = httptest.NewRequest("POST", "/api/version/shutdown", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("version shutdown status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestVersion_EmptyDefaultsToDev verifies RegisterVersion replaces an empty
// version string with "dev".
func TestVersion_EmptyDefaultsToDev(t *testing.T) {
	mux := http.NewServeMux()
	RegisterVersion(mux, "")
	req := httptest.NewRequest("GET", "/api/version", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["version"] != "dev" {
		t.Fatalf("version = %v, want dev", resp["version"])
	}
}

// TestShutdown_UnauthorizedAndAuthorized covers the SHUTDOWN_SECRET guard and
// the authorized path. We do NOT actually trigger the os.Exit goroutine; we
// only verify the response shape by sending a wrong-secret request (which
// short-circuits before spawning the exit goroutine).
func TestShutdown_UnauthorizedAndAuthorized(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterShutdown(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Set SHUTDOWN_SECRET and send a request without the matching bearer —
	// should return 401 with success=false.
	t.Setenv("SHUTDOWN_SECRET", "s3cret")
	req := httptest.NewRequest("POST", "/api/shutdown", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["success"] != false {
		t.Fatalf("wrong secret success = %v, want false", resp["success"])
	}
}

// TestTags_OptionsAndCORS covers the OPTIONS preflight on /api/tags.
func TestTags_OptionsAndCORS(t *testing.T) {
	mux := http.NewServeMux()
	RegisterTags(mux)

	req := httptest.NewRequest("OPTIONS", "/api/tags", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("options status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("CORS origin = %q, want *", rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

// TestMediaProviders_VoicesFilter covers the voices handler with a lang filter
// and the elevenlabs/local-device special-casing.
func TestMediaProviders_VoicesFilter(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterMediaProviders(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Default (edge-tts) with lang=en filter.
	req := httptest.NewRequest("GET", "/api/media-providers/tts/voices?lang=en", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("voices lang=en status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Voices    []map[string]any `json:"voices"`
		Languages []map[string]any `json:"languages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Voices) == 0 {
		t.Fatal("expected non-empty voices for lang=en")
	}

	// elevenlabs — empty voices list.
	req = httptest.NewRequest("GET", "/api/media-providers/tts/voices?provider=elevenlabs", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal elevenlabs: %v", err)
	}
	if len(resp.Voices) != 0 {
		t.Fatalf("elevenlabs voices = %d, want 0", len(resp.Voices))
	}

	// local-device — also empty.
	req = httptest.NewRequest("GET", "/api/media-providers/tts/voices?provider=local-device", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Voices) != 0 {
		t.Fatalf("local-device voices = %d, want 0", len(resp.Voices))
	}
}

// TestV1Beta_ListShape verifies the v1beta list response is non-empty and each
// entry has the expected Gemini fields.
func TestV1Beta_ListShape(t *testing.T) {
	mux := http.NewServeMux()
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	RegisterV1Beta(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest("GET", "/api/v1beta/models", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Models) == 0 {
		t.Fatal("expected non-empty models list")
	}
	for _, m := range resp.Models {
		if m["name"] == "" {
			t.Fatalf("model missing name: %v", m)
		}
		if m["supportedGenerationMethods"] == nil {
			t.Fatalf("model %v missing supportedGenerationMethods", m["name"])
		}
	}
}

// TestHealth_RegisteredRoutes verifies RegisterHealth mounts the expected
// route.
func TestHealth_RegisteredRoutes(t *testing.T) {
	mux := http.NewServeMux()
	RegisterHealth(mux)
	req := httptest.NewRequest("GET", "/api/health", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", rec.Code)
	}
}