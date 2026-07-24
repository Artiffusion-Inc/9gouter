package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// codex_reset_credits_test.go ports the GET half of 5cc4f222 (codex #2290)
// with real httptest.Server upstreams (no mock framework) and a real codex
// connection row built from settings.ProviderConnection.

// codexConn builds a codex OAuth connection row with an access token and an
// account id under providerSpecificData, mirroring the shape the OAuth import
// stores in conn.Data.
func codexConn(id, token, accountID string) *settings.ProviderConnection {
	data := map[string]any{"accessToken": token}
	if accountID != "" {
		data["providerSpecificData"] = map[string]any{"chatgptAccountId": accountID}
	}
	b, _ := json.Marshal(data)
	return &settings.ProviderConnection{ID: id, Provider: "codex", Data: b}
}

func TestParseCodexResetCredits(t *testing.T) {
	body := []byte(`{
		"available_count": 2,
		"credits": [
			{"status": "active", "granted_at": "2026-07-01T10:00:00Z", "expires_at": "2026-07-08T10:00:00Z"},
			{"status": "expired", "granted_at": "2026-06-01T10:00:00Z", "expires_at": "2026-06-08T10:00:00Z"}
		]
	}`)
	out := parseCodexResetCredits(body, "conn-1")
	if out["availableCount"] != 2 {
		t.Errorf("availableCount = %v, want 2", out["availableCount"])
	}
	credits, _ := out["credits"].([]any)
	if len(credits) != 2 {
		t.Fatalf("credits len = %d, want 2", len(credits))
	}
	first, _ := credits[0].(map[string]any)
	if first["status"] != "active" {
		t.Errorf("credit[0].status = %v, want active", first["status"])
	}
	if first["grantedAt"] != "2026-07-01T10:00:00Z" {
		t.Errorf("credit[0].grantedAt = %v, want 2026-07-01T10:00:00Z", first["grantedAt"])
	}
	if first["expiresAt"] != "2026-07-08T10:00:00Z" {
		t.Errorf("credit[0].expiresAt = %v, want 2026-07-08T10:00:00Z", first["expiresAt"])
	}
}

func TestParseCodexResetCredits_EmptyAndCamelCase(t *testing.T) {
	// No credits, availableCount in camelCase form.
	out := parseCodexResetCredits([]byte(`{"availableCount": 5}`), "c")
	if out["availableCount"] != 5 {
		t.Errorf("availableCount (camelCase) = %v, want 5", out["availableCount"])
	}
	credits, _ := out["credits"].([]any)
	if len(credits) != 0 {
		t.Errorf("credits len = %d, want 0", len(credits))
	}
	// Negative available_count is clamped to 0 (JS Math.max(0, ...)).
	out = parseCodexResetCredits([]byte(`{"available_count": -3}`), "c")
	if out["availableCount"] != 0 {
		t.Errorf("availableCount negative = %v, want 0", out["availableCount"])
	}
}

func TestParseCodexResetCredits_UnknownStatusAndMissingDates(t *testing.T) {
	out := parseCodexResetCredits([]byte(`{"credits":[{"foo":"bar"}]}`), "c")
	credits, _ := out["credits"].([]any)
	if len(credits) != 1 {
		t.Fatalf("credits len = %d, want 1", len(credits))
	}
	c, _ := credits[0].(map[string]any)
	if c["status"] != "unknown" {
		t.Errorf("missing status → %q, want unknown", c["status"])
	}
	if c["grantedAt"] != nil {
		t.Errorf("missing grantedAt → %v, want nil", c["grantedAt"])
	}
	if c["expiresAt"] != nil {
		t.Errorf("missing expiresAt → %v, want nil", c["expiresAt"])
	}
}

func TestCodexConnectionCreds(t *testing.T) {
	// Non-codex provider → ok=false.
	nonCodex := &settings.ProviderConnection{ID: "c", Provider: "openai", Data: []byte(`{}`)}
	if _, _, ok := codexConnectionCreds(nonCodex); ok {
		t.Error("non-codex provider should not be eligible")
	}
	// codex with token + account id.
	conn := codexConn("c", "tok-123", "acct-9")
	tok, acct, ok := codexConnectionCreds(conn)
	if !ok {
		t.Fatal("codex connection should be eligible")
	}
	if tok != "tok-123" {
		t.Errorf("token = %q, want tok-123", tok)
	}
	if acct != "acct-9" {
		t.Errorf("accountID = %q, want acct-9", acct)
	}
	// workspaceId takes precedence over chatgptAccountId.
	b, _ := json.Marshal(map[string]any{
		"accessToken": "tok",
		"providerSpecificData": map[string]any{
			"workspaceId":      "ws-1",
			"chatgptAccountId": "acct-2",
		},
	})
	conn2 := &settings.ProviderConnection{ID: "c", Provider: "codex", Data: b}
	_, acct2, _ := codexConnectionCreds(conn2)
	if acct2 != "ws-1" {
		t.Errorf("accountID = %q, want ws-1 (workspaceId precedence)", acct2)
	}
}

// TestFetchCodexResetCredits_RealUpstream hits a real httptest.Server and
// verifies the codex fingerprint headers are sent and the inventory is parsed
// into the dashboard shape.
func TestFetchCodexResetCredits_RealUpstream(t *testing.T) {
	var gotAuth, gotAccount, gotBeta, gotOriginator string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-ID")
		gotBeta = r.Header.Get("OpenAI-Beta")
		gotOriginator = r.Header.Get("originator")
		_, _ = io.WriteString(w, `{"available_count":1,"credits":[{"status":"active","granted_at":"2026-07-01T00:00:00Z","expires_at":"2026-07-08T00:00:00Z"}]}`)
	}))
	defer srv.Close()
	prev := codexResetCreditsURL
	codexResetCreditsURL = srv.URL
	defer func() { codexResetCreditsURL = prev }()

	out, err := fetchCodexResetCredits(context.Background(), codexConn("c1", "tok-xyz", "acct-7"))
	if err != nil {
		t.Fatalf("fetch err: %v", err)
	}
	if gotAuth != "Bearer tok-xyz" {
		t.Errorf("Authorization = %q, want Bearer tok-xyz", gotAuth)
	}
	if gotAccount != "acct-7" {
		t.Errorf("ChatGPT-Account-ID = %q, want acct-7", gotAccount)
	}
	if gotBeta != "codex-1" {
		t.Errorf("OpenAI-Beta = %q, want codex-1", gotBeta)
	}
	if gotOriginator != "codex_cli_rs" {
		t.Errorf("originator = %q, want codex_cli_rs", gotOriginator)
	}
	if out["availableCount"] != 1 {
		t.Errorf("availableCount = %v, want 1", out["availableCount"])
	}
	credits, _ := out["credits"].([]any)
	if len(credits) != 1 {
		t.Fatalf("credits len = %d, want 1", len(credits))
	}
	if out["connectionId"] != "c1" {
		t.Errorf("connectionId = %v, want c1", out["connectionId"])
	}
}

// TestFetchCodexResetCredits_NoToken verifies a codex connection without an
// access token returns the safe empty payload with the re-authorize message
// instead of attempting the upstream call.
func TestFetchCodexResetCredits_NoToken(t *testing.T) {
	conn := codexConn("c2", "", "")
	out, err := fetchCodexResetCredits(context.Background(), conn)
	if err != nil {
		t.Fatalf("fetch err: %v", err)
	}
	msg, _ := out["message"].(string)
	if !strings.Contains(msg, "re-authorize") {
		t.Errorf("message = %q, want re-authorize guidance", msg)
	}
	credits, _ := out["credits"].([]any)
	if len(credits) != 0 {
		t.Errorf("credits len = %d, want 0", len(credits))
	}
}

// TestFetchCodexResetCredits_UpstreamError verifies a non-200 upstream body
// surfaces a user-facing message without crashing the dashboard.
func TestFetchCodexResetCredits_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"message":"rate limited, try later"}`)
	}))
	defer srv.Close()
	prev := codexResetCreditsURL
	codexResetCreditsURL = srv.URL
	defer func() { codexResetCreditsURL = prev }()

	out, err := fetchCodexResetCredits(context.Background(), codexConn("c3", "tok", "acct"))
	if err != nil {
		t.Fatalf("fetch err: %v", err)
	}
	msg, _ := out["message"].(string)
	if msg != "rate limited, try later" {
		t.Errorf("upstream error message = %q, want the upstream message", msg)
	}
	if out["connectionId"] != "c3" {
		t.Errorf("connectionId = %v, want c3", out["connectionId"])
	}
}

// TestCodexISODate covers the JS toIsoDate parity: ISO pass-through, numeric
// epoch (seconds and milliseconds), and garbage pass-through.
func TestCodexISODate(t *testing.T) {
	if got := codexISODate(""); got != nil {
		t.Errorf("empty → %v, want nil", got)
	}
	// ISO pass-through (normalized to RFC3339).
	if got := codexISODate("2026-07-01T10:00:00Z"); got != "2026-07-01T10:00:00Z" {
		t.Errorf("ISO → %v, want 2026-07-01T10:00:00Z", got)
	}
	// Numeric seconds epoch (< 1e12) → ms.
	if got := codexISODate("1751364000"); got == nil || !strings.HasPrefix(got.(string), "2025") {
		t.Errorf("seconds epoch → %v, want an ISO date", got)
	}
	// Numeric milliseconds epoch (>= 1e12) → ms directly.
	if got := codexISODate("1751364000000"); got == nil || !strings.HasPrefix(got.(string), "2025") {
		t.Errorf("ms epoch → %v, want an ISO date", got)
	}
	// Garbage passes through unchanged.
	if got := codexISODate("not-a-date"); got != "not-a-date" {
		t.Errorf("garbage → %v, want passthrough", got)
	}
}
