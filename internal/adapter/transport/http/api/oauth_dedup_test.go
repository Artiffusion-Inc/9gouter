package api

// oauth_dedup_test.go pins the cb0135b6 cross-IdP dedup wired into the OAuth
// import paths via ConnectionRepo.Create (which now calls
// FindExistingForImport + mergeConnection). A second import of the same
// identity updates the existing row in place instead of creating a duplicate,
// and the handler surfaces the REAL (existing) id in its response — so a
// dashboard read-back by the returned id hits the persisted row.
//
// The grok-cli device-code poll path is the cleanest E2E vehicle: grok-cli
// connections carry providerSpecificData (authMethod=device_code) but no
// chatgptAccountId and no username → the non-workspace bare-email fallback
// match, exactly the cross-IdP path the upstream fix targets. Real sqlite
// ConnectionRepo, real httptest mux + upstream — no mock repo.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
)

// pollGrokCli drives the grok-cli /poll endpoint against a host-swapped
// upstream that always returns a successful token + profile for the given
// email. Returns the connection id from the response. The id_token carries the
// email so mapTokens resolves it the same way the real flow does.
func pollGrokCli(t *testing.T, mux *http.ServeMux, ck, email string) string {
	t.Helper()
	idTok := makeJWT(t, map[string]any{"email": email})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at",
				"refresh_token": "rt",
				"expires_in":    7200,
				"id_token":      idTok,
				"scope":         grokCliScope,
			})
		case "/v1/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": email, "userId": "u-1"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	prev := grokCliHTTPClient
	grokCliHTTPClient = grokCliSwapClient(srv)
	t.Cleanup(func() { grokCliHTTPClient = prev })

	req := httptest.NewRequest(http.MethodPost, "/api/oauth/grok-cli/poll", strings.NewReader(`{"deviceCode":"dc"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	conn, _ := resp["connection"].(map[string]any)
	id, _ := conn["id"].(string)
	if id == "" {
		t.Fatal("missing connection.id in poll response")
	}
	return id
}

// TestGrokCliPoll_DedupesSameEmail verifies cb0135b6 end-to-end: polling for
// the same email twice merges onto the first row → same connection id, and
// exactly one persisted row for that email.
func TestGrokCliPoll_DedupesSameEmail(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	first := pollGrokCli(t, mux, ck, "dup@x.com")
	second := pollGrokCli(t, mux, ck, "dup@x.com")

	if first != second {
		t.Errorf("second poll id = %q, want %q (dedup onto existing row)", second, first)
	}

	conns, err := deps.Connections.List(context.Background(), repo.ConnectionFilter{Provider: "grok-cli"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	count := 0
	for _, c := range conns {
		if c.Email == "dup@x.com" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("rows for dup@x.com = %d, want 1 (dedup must not create a duplicate)", count)
	}
}

// TestGrokCliPoll_DistinctEmailsSeparate verifies the dedup does NOT collapse
// distinct emails — two different emails produce two rows / two ids.
func TestGrokCliPoll_DistinctEmailsSeparate(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterOAuth(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	a := pollGrokCli(t, mux, ck, "a@x.com")
	b := pollGrokCli(t, mux, ck, "b@x.com")
	if a == b {
		t.Errorf("distinct emails collapsed to same id %q", a)
	}
	conns, err := deps.Connections.List(context.Background(), repo.ConnectionFilter{Provider: "grok-cli"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(conns) != 2 {
		t.Errorf("grok-cli rows = %d, want 2 (distinct emails kept separate)", len(conns))
	}
}

var _ = adapterauth.NewCookieStore
