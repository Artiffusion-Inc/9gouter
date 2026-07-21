package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
)

// TestUsageExtra_RequestLogsAndStream covers the request-logs and stream routes
// registered by RegisterUsageExtra.
func TestUsageExtra_RequestLogsAndStream(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterUsageExtra(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// request-logs.
	req := httptest.NewRequest("GET", "/api/usage/request-logs", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request-logs status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// stream — short SSE response.
	req = httptest.NewRequest("GET", "/api/usage/stream", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream Content-Type = %q, want text/event-stream", ct)
	}
}

// TestUsageExtra_ByConnection covers GET /api/usage/{connectionId} for the
// missing-connection, not-eligible, and eligible branches.
func TestUsageExtra_ByConnection(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterProviders(mux, deps)
	RegisterUsageExtra(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Missing connection → 404.
	req := httptest.NewRequest("GET", "/api/usage/nope", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("byConnection missing status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// Seed an apikey connection (not in the usage-eligible whitelist).
	body := `{"provider":"openai","apiKey":"sk-test","name":"P"}`
	req = httptest.NewRequest("POST", "/api/providers", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Connection map[string]any `json:"connection"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created.Connection["id"].(string)

	// apikey + non-whitelisted provider → "not eligible" 200 branch.
	req = httptest.NewRequest("GET", "/api/usage/"+id, nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("byConnection not-eligible status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["message"] != "Usage not available for this connection" {
		t.Fatalf("not-eligible message = %v, want Usage not available...", resp["message"])
	}

	// Seed an oauth connection (eligible by authType).
	body = `{"provider":"any","apiKey":"sk-test","name":"OAuth"}`
	req = httptest.NewRequest("POST", "/api/providers", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create oauth status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	oauthID, _ := created.Connection["id"].(string)
	// Force the connection to oauth authType via repo Update so the eligibility
	// check (authType == "oauth") passes.
	conn, err := deps.Connections.GetByID(context.Background(), oauthID)
	if err != nil || conn == nil {
		t.Fatalf("get oauth conn: %v", err)
	}
	conn.AuthType = "oauth"
	conn.Provider = "anything"
	if err := deps.Connections.Update(context.Background(), *conn); err != nil {
		t.Fatalf("update oauth conn: %v", err)
	}

	req = httptest.NewRequest("GET", "/api/usage/"+oauthID, nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("byConnection eligible status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal eligible: %v", err)
	}
	if resp["provider"] != "anything" {
		t.Fatalf("eligible provider = %v, want anything", resp["provider"])
	}
}

// TestUsageExtra_CodexResetCredits covers the GET and POST codex reset credits
// routes.
func TestUsageExtra_CodexResetCredits(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterUsageExtra(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// GET.
	req := httptest.NewRequest("GET", "/api/usage/conn-1/codex-reset-credits", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("codex reset GET status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal GET: %v", err)
	}
	if resp["credits"] == nil {
		t.Fatal("expected credits field")
	}

	// POST.
	req = httptest.NewRequest("POST", "/api/usage/conn-1/codex-reset-credits", strings.NewReader(`{}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("codex reset POST status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal POST: %v", err)
	}
	if resp["code"] != "no_credit" {
		t.Fatalf("POST code = %v, want no_credit", resp["code"])
	}
}

// TestUsageExtra_isUsageEligible exercises the eligibility whitelist helper
// directly.
func TestUsageExtra_isUsageEligible(t *testing.T) {
	cases := []struct {
		provider, authType string
		want              bool
	}{
		{"openai", "oauth", true},
		{"openai", "apikey", false},
		{"openai", "api_key", false},
		{"openai", "", false},
		{"glm", "apikey", true},
		{"glm-cn", "apikey", true},
		{"minimax", "apikey", true},
		{"kiro", "apikey", true},
		{"codebuddy-cn", "apikey", true},
		{"grok-cli", "apikey", true},
		{"qoder", "apikey", true},
		{"vercel-ai-gateway", "apikey", true},
		{"unknown", "apikey", false},
		{"glm", "api_key", true},
		{"glm", "weird", false},
	}
	for _, c := range cases {
		if got := isUsageEligible(c.provider, c.authType); got != c.want {
			t.Fatalf("isUsageEligible(%q, %q) = %v, want %v", c.provider, c.authType, got, c.want)
		}
	}
}

// keep context import alive (used in byConnection test).
var _ = context.Background