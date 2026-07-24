package api

// codex_account_info_test.go pins #155 — the codex bulk-import JWT identity
// backfill (c73c419d full wiring). extractCodexAccountInfo reads email,
// chatgptAccountId, chatgptPlanType from a codex JWT (idToken preferred, else
// accessToken); codexExpiresAtFromExpiresIn computes expiresAt; the
// codexBulkImportAccounts handler persists the backfilled identity into the
// connection data blob so the cb0135b6 cross-IdP dedup rule (codex → match only
// if BOTH rows expose an equal chatgptAccountId) fires on a re-import instead
// of creating a duplicate. Real sqlite ConnectionRepo + real auth-gated mux +
// httptest.NewRequest — no mock.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
)

// codexJWT builds an unsigned JWT carrying the given claims (base64url
// header.payload.sig). The signature is irrelevant — extractCodexAccountInfo
// does not verify it, mirroring DecodeJWTPayload.
func codexJWT(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payloadBytes, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return header + "." + payload + ".sig"
}

// TestExtractCodexAccountInfo_Namespace covers the happy path: a codex JWT
// with the https://api.openai.com/auth namespace exposes chatgpt_account_id +
// chatgpt_plan_type + email.
func TestExtractCodexAccountInfo_Namespace(t *testing.T) {
	jwt := codexJWT(map[string]any{
		"email": "a@b.c",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-1",
			"chatgpt_plan_type":  "plus",
		},
	})
	info := extractCodexAccountInfo(jwt)
	if info.Email != "a@b.c" {
		t.Errorf("Email = %q, want a@b.c", info.Email)
	}
	if info.ChatGPTAccountID != "acct-1" {
		t.Errorf("ChatGPTAccountID = %q, want acct-1", info.ChatGPTAccountID)
	}
	if info.ChatGPTPlanType != "plus" {
		t.Errorf("ChatGPTPlanType = %q, want plus", info.ChatGPTPlanType)
	}
}

// TestExtractCodexAccountInfo_AltNamespaceKeys covers the alternate namespace
// keys the JS helper accepts: chatgpt_id and chatgptPlanType.
func TestExtractCodexAccountInfo_AltNamespaceKeys(t *testing.T) {
	jwt := codexJWT(map[string]any{
		"preferred_username": "p@q.r",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_id":      "acct-2",
			"chatgptPlanType": "pro",
		},
	})
	info := extractCodexAccountInfo(jwt)
	if info.Email != "p@q.r" {
		t.Errorf("Email = %q, want p@q.r (preferred_username fallback)", info.Email)
	}
	if info.ChatGPTAccountID != "acct-2" {
		t.Errorf("ChatGPTAccountID = %q, want acct-2 (chatgpt_id)", info.ChatGPTAccountID)
	}
	if info.ChatGPTPlanType != "pro" {
		t.Errorf("ChatGPTPlanType = %q, want pro (chatgptPlanType)", info.ChatGPTPlanType)
	}
}

// TestExtractCodexAccountInfo_TopLevelFallback covers the top-level account_id
// + plan_type fallbacks used when the namespace is absent/empty, and the sub
// email fallback.
func TestExtractCodexAccountInfo_TopLevelFallback(t *testing.T) {
	jwt := codexJWT(map[string]any{
		"sub":        "user-sub-9",
		"account_id": "acct-3",
		"plan_type":  "team",
	})
	info := extractCodexAccountInfo(jwt)
	if info.Email != "user-sub-9" {
		t.Errorf("Email = %q, want user-sub-9 (sub fallback)", info.Email)
	}
	if info.ChatGPTAccountID != "acct-3" {
		t.Errorf("ChatGPTAccountID = %q, want acct-3 (top-level account_id)", info.ChatGPTAccountID)
	}
	if info.ChatGPTPlanType != "team" {
		t.Errorf("ChatGPTPlanType = %q, want team (top-level plan_type)", info.ChatGPTPlanType)
	}
}

// TestExtractCodexAccountInfo_EmptyGarbage covers empty / malformed JWTs →
// zero value (caller leaves incoming fields as-is).
func TestExtractCodexAccountInfo_EmptyGarbage(t *testing.T) {
	for _, jwt := range []string{"", "   ", "not-a-jwt", "onlyone", "a.b", "a.b.c.d"} {
		info := extractCodexAccountInfo(jwt)
		if info != (codexAccountInfo{}) {
			t.Errorf("extractCodexAccountInfo(%q) = %+v, want zero", jwt, info)
		}
	}
}

// TestCodexExpiresAtFromExpiresIn covers the expiresIn → ISO-8601 conversion.
func TestCodexExpiresAtFromExpiresIn(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	got := codexExpiresAtFromExpiresIn(3600, now)
	want := "2026-07-24T13:00:00Z"
	if got != want {
		t.Errorf("expiresAt(3600) = %q, want %q", got, want)
	}
	if codexExpiresAtFromExpiresIn(0, now) != "" {
		t.Error("expiresAt(0) must be empty")
	}
	if codexExpiresAtFromExpiresIn(-5, now) != "" {
		t.Error("expiresAt(-5) must be empty")
	}
}

// postCodexBulkImport posts an accounts array to the bulk-import route and
// returns the parsed response.
func postCodexBulkImport(t *testing.T, mux http.Handler, ck string, accounts []map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"accounts": accounts})
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/codex/bulk-import", strings.NewReader(string(body)))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk-import status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp
}

// TestCodexBulkImport_BackfillsIdentity verifies the handler persists a codex
// connection with the identity backfilled from the JWT into the data blob
// (email, providerSpecificData.chatgptAccountId, chatgptPlanType, testStatus,
// lastRefreshAt).
func TestCodexBulkImport_BackfillsIdentity(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	idToken := codexJWT(map[string]any{
		"email": "bulk@openai.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-bulk-1",
			"chatgpt_plan_type":  "plus",
		},
	})
	accessToken := codexJWT(map[string]any{"sub": "should-not-win"})
	resp := postCodexBulkImport(t, mux, ck, []map[string]any{{
		"accessToken":  accessToken,
		"refreshToken": "rt-1",
		"idToken":      idToken,
	}})

	if resp["success"] != float64(1) {
		t.Fatalf("success = %v, want 1", resp["success"])
	}
	if resp["failed"] != float64(0) {
		t.Errorf("failed = %v, want 0", resp["failed"])
	}
	results, _ := resp["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	row, _ := results[0].(map[string]any)
	if row["ok"] != true {
		t.Fatalf("results[0].ok = %v, want true", row["ok"])
	}
	id, _ := row["id"].(string)
	if id == "" {
		t.Fatal("results[0].id missing")
	}

	// Read back the persisted row and assert the backfilled identity.
	conn, err := deps.Connections.GetByID(context.Background(), id)
	if err != nil || conn == nil {
		t.Fatalf("GetByID(%q): %v", id, err)
	}
	if conn.Provider != "codex" {
		t.Errorf("Provider = %q, want codex", conn.Provider)
	}
	if conn.AuthType != "oauth" {
		t.Errorf("AuthType = %q, want oauth", conn.AuthType)
	}
	if conn.Email != "bulk@openai.com" {
		t.Errorf("Email = %q, want bulk@openai.com (idToken email wins)", conn.Email)
	}
	var data map[string]any
	_ = json.Unmarshal(conn.Data, &data)
	if data["email"] != "bulk@openai.com" {
		t.Errorf("data.email = %v, want bulk@openai.com", data["email"])
	}
	if data["accessToken"] != accessToken {
		t.Errorf("data.accessToken not preserved")
	}
	if data["refreshToken"] != "rt-1" {
		t.Errorf("data.refreshToken = %v, want rt-1", data["refreshToken"])
	}
	if data["testStatus"] != "active" {
		t.Errorf("data.testStatus = %v, want active", data["testStatus"])
	}
	if data["lastRefreshAt"] == nil || data["lastRefreshAt"] == "" {
		t.Error("data.lastRefreshAt missing")
	}
	psd, _ := data["providerSpecificData"].(map[string]any)
	if psd == nil {
		t.Fatal("data.providerSpecificData missing — backfill did not run")
	}
	if psd["chatgptAccountId"] != "acct-bulk-1" {
		t.Errorf("psd.chatgptAccountId = %v, want acct-bulk-1", psd["chatgptAccountId"])
	}
	if psd["chatgptPlanType"] != "plus" {
		t.Errorf("psd.chatgptPlanType = %v, want plus", psd["chatgptPlanType"])
	}
}

// TestCodexBulkImport_AccessTokenFallback verifies that when idToken is absent,
// the identity is backfilled from the accessToken instead.
func TestCodexBulkImport_AccessTokenFallback(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	accessToken := codexJWT(map[string]any{
		"email": "atfallback@openai.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-at",
		},
	})
	resp := postCodexBulkImport(t, mux, ck, []map[string]any{{
		"accessToken":  accessToken,
		"refreshToken": "rt-2",
	}})
	id, _ := resp["results"].([]any)[0].(map[string]any)["id"].(string)
	conn, _ := deps.Connections.GetByID(context.Background(), id)
	if conn.Email != "atfallback@openai.com" {
		t.Errorf("Email = %q, want atfallback@openai.com (accessToken fallback)", conn.Email)
	}
	var data map[string]any
	_ = json.Unmarshal(conn.Data, &data)
	psd, _ := data["providerSpecificData"].(map[string]any)
	if psd["chatgptAccountId"] != "acct-at" {
		t.Errorf("psd.chatgptAccountId = %v, want acct-at (accessToken fallback)", psd["chatgptAccountId"])
	}
}

// TestCodexBulkImport_DedupesSameAccount verifies the c73c419d dedup rule fires
// because chatgptAccountId is backfilled: re-importing the same account merges
// onto the existing row (same id, one row in the DB).
func TestCodexBulkImport_DedupesSameAccount(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	makeAccount := func() map[string]any {
		jwt := codexJWT(map[string]any{
			"email": "dup@openai.com",
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id": "acct-dup",
			},
		})
		return map[string]any{"accessToken": jwt, "refreshToken": "rt-dup", "idToken": jwt}
	}

	first := postCodexBulkImport(t, mux, ck, []map[string]any{makeAccount()})
	second := postCodexBulkImport(t, mux, ck, []map[string]any{makeAccount()})

	firstID, _ := first["results"].([]any)[0].(map[string]any)["id"].(string)
	secondID, _ := second["results"].([]any)[0].(map[string]any)["id"].(string)
	if firstID == "" || secondID == "" {
		t.Fatalf("missing ids: first=%q second=%q", firstID, secondID)
	}
	if firstID != secondID {
		t.Errorf("re-import id = %q, want %q (dedup onto existing row via chatgptAccountId)", secondID, firstID)
	}
	conns, err := deps.Connections.List(context.Background(), repo.ConnectionFilter{Provider: "codex"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	count := 0
	for _, c := range conns {
		var d map[string]any
		_ = json.Unmarshal(c.Data, &d)
		if psd, _ := d["providerSpecificData"].(map[string]any); psd != nil && psd["chatgptAccountId"] == "acct-dup" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("rows with chatgptAccountId=acct-dup = %d, want 1 (dedup must not duplicate)", count)
	}
}

// TestCodexBulkImport_MissingAccessToken verifies a row without an accessToken
// is skipped with ok=false + an explicit error (not a 4xx — the endpoint is
// permissive so the modal can render the per-row list).
func TestCodexBulkImport_MissingAccessToken(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	resp := postCodexBulkImport(t, mux, ck, []map[string]any{{
		"refreshToken": "rt-only",
	}})
	if resp["success"] != float64(0) {
		t.Errorf("success = %v, want 0", resp["success"])
	}
	if resp["failed"] != float64(1) {
		t.Errorf("failed = %v, want 1", resp["failed"])
	}
	row, _ := resp["results"].([]any)[0].(map[string]any)
	if row["ok"] != false {
		t.Errorf("results[0].ok = %v, want false", row["ok"])
	}
	if msg, _ := row["error"].(string); !strings.Contains(msg, "accessToken") {
		t.Errorf("results[0].error = %q, want accessToken mention", msg)
	}
	conns, _ := deps.Connections.List(context.Background(), repo.ConnectionFilter{Provider: "codex"})
	if len(conns) != 0 {
		t.Errorf("codex rows = %d, want 0 (failed row must not persist)", len(conns))
	}
}

// keep imports referenced.
var _ = adapterauth.CookieStore{}
