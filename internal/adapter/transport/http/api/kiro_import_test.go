package api

// kiro_import_test.go pins the cb0135b6 p2 wiring of the CLIProxyAPI import
// path (POST /api/oauth/kiro/import-cli-proxy): a CLIProxyAPI auth blob is
// normalized via tokenrefresh.NormalizeKiroExternalIDPAuth and persisted as a
// kiro connection via Connections.Create (which runs the cross-IdP dedup).
// Real sqlite ConnectionRepo + real auth-gated mux + httptest.NewRequest — no
// mock repo.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// validCliProxyAuth builds a minimal valid external_idp CLIProxy auth blob
// (access_token is a JWT carrying the email so Normalize extracts it).
func validCliProxyAuth(email string) map[string]any {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"` + email + `","exp":1750000000}`))
	at := header + "." + payload + ".sig"
	return map[string]any{
		"auth_method":    "external_idp",
		"access_token":   at,
		"refresh_token":  "rt-xyz",
		"client_id":      "cid-1",
		"token_endpoint": "https://login.microsoftonline.com/t/oauth2/v2.0/token",
		"profile_arn":    "arn:aws:codewhisperer:us-east-1:123:profile/P",
		"scopes":         []any{"offline_access", " https://api/.default "},
		"region":         "eu-west-1",
	}
}

// postCliProxy posts the auth blob under the given wrapper key (or bare) to the
// import-cli-proxy route and returns the response connection id.
func postCliProxy(t *testing.T, mux http.Handler, ck string, payload map[string]any) (string, map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/kiro/import-cli-proxy", strings.NewReader(string(body)))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("import-cli-proxy status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	conn, _ := resp["connection"].(map[string]any)
	id, _ := conn["id"].(string)
	return id, resp
}

// TestKiroImportCliProxy_CreatesConnection verifies a CLIProxyAPI auth blob is
// normalized + persisted as a real kiro connection row with the normalized
// providerSpecificData fields.
func TestKiroImportCliProxy_CreatesConnection(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	id, resp := postCliProxy(t, mux, ck, map[string]any{"cliProxyAuth": validCliProxyAuth("joe@contoso.com")})
	if id == "" {
		t.Fatal("missing connection.id in import response")
	}
	if resp["success"] != true {
		t.Errorf("success=%v want true", resp["success"])
	}
	conn, _ := resp["connection"].(map[string]any)
	if conn["provider"] != "kiro" {
		t.Errorf("provider=%v want kiro", conn["provider"])
	}
	if conn["email"] != "joe@contoso.com" {
		t.Errorf("email=%v want joe@contoso.com", conn["email"])
	}

	// One row persisted, with the normalized psd fields inside the data blob.
	conns, err := deps.Connections.List(context.Background(), repo.ConnectionFilter{Provider: "kiro"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("kiro rows = %d, want 1", len(conns))
	}
	var data map[string]any
	_ = json.Unmarshal(conns[0].Data, &data)
	if data["accessToken"] == "" {
		t.Error("data.accessToken missing")
	}
	if data["refreshToken"] != "rt-xyz" {
		t.Errorf("data.refreshToken=%v want rt-xyz", data["refreshToken"])
	}
	if data["profileArn"] != "arn:aws:codewhisperer:us-east-1:123:profile/P" {
		t.Errorf("data.profileArn=%v", data["profileArn"])
	}
	if data["authMethod"] != "external_idp" {
		t.Errorf("data.authMethod=%v want external_idp", data["authMethod"])
	}
	if data["region"] != "eu-west-1" {
		t.Errorf("data.region=%v want eu-west-1", data["region"])
	}
	if data["scope"] != "offline_access https://api/.default" {
		t.Errorf("data.scope=%v want normalized space-joined", data["scope"])
	}
}

// TestKiroImportCliProxy_DedupesSameEmail verifies the cb0135b6 cross-IdP
// dedup: re-importing the same email merges onto the existing row (same id,
// one row), instead of creating a duplicate.
func TestKiroImportCliProxy_DedupesSameEmail(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	first, _ := postCliProxy(t, mux, ck, map[string]any{"cliProxyAuth": validCliProxyAuth("dup@contoso.com")})
	second, _ := postCliProxy(t, mux, ck, map[string]any{"cliProxyAuth": validCliProxyAuth("dup@contoso.com")})
	if first != second {
		t.Errorf("re-import id = %q, want %q (dedup onto existing row)", second, first)
	}
	conns, err := deps.Connections.List(context.Background(), repo.ConnectionFilter{Provider: "kiro"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	count := 0
	for _, c := range conns {
		if c.Email == "dup@contoso.com" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("rows for dup@contoso.com = %d, want 1 (dedup must not duplicate)", count)
	}
}

// TestKiroImportCliProxy_DistinctEmails verifies distinct emails produce distinct
// connections.
func TestKiroImportCliProxy_DistinctEmails(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	a, _ := postCliProxy(t, mux, ck, map[string]any{"cliProxyAuth": validCliProxyAuth("a@contoso.com")})
	b, _ := postCliProxy(t, mux, ck, map[string]any{"cliProxyAuth": validCliProxyAuth("b@contoso.com")})
	if a == b {
		t.Errorf("distinct emails collapsed to same id %q", a)
	}
	conns, _ := deps.Connections.List(context.Background(), repo.ConnectionFilter{Provider: "kiro"})
	if len(conns) != 2 {
		t.Errorf("kiro rows = %d, want 2 (distinct emails kept separate)", len(conns))
	}
}

// TestKiroImportCliProxy_BareBody verifies the auth blob is accepted as the body
// itself (JS `?? body`), not only under cliProxyAuth.
func TestKiroImportCliProxy_BareBody(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	id, _ := postCliProxy(t, mux, ck, validCliProxyAuth("bare@contoso.com"))
	if id == "" {
		t.Fatal("bare-body import returned no id")
	}
	conns, _ := deps.Connections.List(context.Background(), repo.ConnectionFilter{Provider: "kiro"})
	if len(conns) != 1 || conns[0].Email != "bare@contoso.com" {
		t.Errorf("bare-body import not persisted: %+v", conns)
	}
}

// TestKiroImportCliProxy_InvalidBlob verifies a missing-required-field blob
// surfaces a 400 (Normalize error), not imported:0.
func TestKiroImportCliProxy_InvalidBlob(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// missing refresh_token
	bad := validCliProxyAuth("x@y.com")
	delete(bad, "refresh_token")
	body, _ := json.Marshal(map[string]any{"cliProxyAuth": bad})
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/kiro/import-cli-proxy", strings.NewReader(string(body)))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid blob status = %d, want 400", rec.Code)
	}
	var errResp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &errResp)
	if msg, _ := errResp["error"].(string); !strings.Contains(msg, "refresh_token") {
		t.Errorf("error=%q, want refresh_token mention", msg)
	}
	conns, _ := deps.Connections.List(context.Background(), repo.ConnectionFilter{Provider: "kiro"})
	if len(conns) != 0 {
		t.Errorf("invalid blob should not persist, got %d rows", len(conns))
	}
}

// TestKiroImportCliProxy_NonExternalIDP verifies an auth_method other than
// external_idp is rejected.
func TestKiroImportCliProxy_NonExternalIDP(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	bad := validCliProxyAuth("x@y.com")
	bad["auth_method"] = "apiKey"
	body, _ := json.Marshal(map[string]any{"cliProxyAuth": bad})
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/kiro/import-cli-proxy", strings.NewReader(string(body)))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-external_idp status = %d, want 400", rec.Code)
	}
}

// keep settings import referenced (ProviderConnection read-back used below).
var _ = settings.ProviderConnection{}
