package http

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// proxypoolrotate_test.go ports the regression coverage for decolua/9router
// #2409 (e1f3399b): pickProxyPoolId + the no-auth provider rotation wired into
// resolveCredentialsWithOpts. Tests use the real repo.ProxyPoolRepo +
// repo.SettingsRepo over an on-disk sqlite DB (no mock).

// resetRotateState clears the package-level round-robin counter so tests are
// independent of execution order.
func resetRotateState() {
	rotateStateKeyed.Lock()
	rotateStateKeyed.m = make(map[string]int)
	rotateStateKeyed.Unlock()
}

func TestPickProxyPoolId_EmptyAndSingle(t *testing.T) {
	if got := pickProxyPoolId(nil, "round-robin", "p"); got != "" {
		t.Errorf("empty pool → %q, want empty", got)
	}
	if got := pickProxyPoolId([]string{}, "round-robin", "p"); got != "" {
		t.Errorf("zero pool → %q, want empty", got)
	}
	if got := pickProxyPoolId([]string{"only"}, "round-robin", "p"); got != "only" {
		t.Errorf("single pool → %q, want only", got)
	}
}

func TestPickProxyPoolId_NoneAndUnknownReturnsFirst(t *testing.T) {
	pools := []string{"a", "b", "c"}
	if got := pickProxyPoolId(pools, "none", "p"); got != "a" {
		t.Errorf("none strategy → %q, want a (first)", got)
	}
	if got := pickProxyPoolId(pools, "bogus", "p"); got != "a" {
		t.Errorf("unknown strategy → %q, want a (first)", got)
	}
}

func TestPickProxyPoolId_RoundRobinAdvancesSequentially(t *testing.T) {
	resetRotateState()
	pools := []string{"a", "b", "c"}
	var seq []string
	for i := 0; i < 7; i++ {
		seq = append(seq, pickProxyPoolId(pools, "round-robin", "opencode"))
	}
	// 1st call → index 0 (counter -1 → 0), then 1, 2, wrap 0, 1, 2, 0.
	want := []string{"a", "b", "c", "a", "b", "c", "a"}
	if !equalStrings(seq, want) {
		t.Errorf("round-robin sequence = %v, want %v", seq, want)
	}
}

func TestPickProxyPoolId_RoundRobinStateIsPerProvider(t *testing.T) {
	resetRotateState()
	pools := []string{"x", "y"}
	a1 := pickProxyPoolId(pools, "round-robin", "provider-a")
	b1 := pickProxyPoolId(pools, "round-robin", "provider-b")
	a2 := pickProxyPoolId(pools, "round-robin", "provider-a")
	if a1 != "x" || b1 != "x" || a2 != "y" {
		t.Errorf("per-provider state: a1=%q b1=%q a2=%q, want x x y", a1, b1, a2)
	}
}

func TestPickProxyPoolId_RandomReturnsAMember(t *testing.T) {
	pools := []string{"a", "b", "c", "d"}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		got := pickProxyPoolId(pools, "random", "p")
		if !seen[got] {
			seen[got] = true
		}
		if !contains(pools, got) {
			t.Fatalf("random pick %q not in pool set %v", got, pools)
		}
	}
	// Over 200 draws from 4 entries the chance of missing any is negligible.
	if len(seen) != 4 {
		t.Errorf("random only produced %d distinct pools, want 4 (set=%v)", len(seen), seen)
	}
}

func TestReadRotateStrategy(t *testing.T) {
	cases := []struct {
		name string
		sd   map[string]any
		pid  string
		want string
	}{
		{"nil settings", nil, "opencode", "none"},
		{"no strategies key", map[string]any{}, "opencode", "none"},
		{"missing provider", map[string]any{"providerStrategies": map[string]any{"grok-web": map[string]any{"rotateStrategy": "round-robin"}}}, "opencode", "none"},
		{"empty strategy string", map[string]any{"providerStrategies": map[string]any{"opencode": map[string]any{"rotateStrategy": ""}}}, "opencode", "none"},
		{"round-robin override", map[string]any{"providerStrategies": map[string]any{"opencode": map[string]any{"rotateStrategy": "round-robin"}}}, "opencode", "round-robin"},
		{"random override", map[string]any{"providerStrategies": map[string]any{"opencode": map[string]any{"rotateStrategy": "random"}}}, "opencode", "random"},
	}
	for _, c := range cases {
		if got := readRotateStrategy(c.sd, c.pid); got != c.want {
			t.Errorf("%s: readRotateStrategy = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestResolveNoAuthProxyRotation_NoneLeavesPSDUntouched verifies the default
// strategy leaves the no-auth virtual connection direct (no proxyPoolId, no
// inherited proxyUrl).
func TestResolveNoAuthProxyRotation_NoneLeavesPSDUntouched(t *testing.T) {
	db := mustOpenDB(t)
	defer db.Close()
	poolRepo := repo.NewProxyPoolRepo(db)
	seedPool(t, db, "pool-a", "socks5://egress-a:1080", true)
	seedPool(t, db, "pool-b", "socks5://egress-b:1080", true)

	h := &v1Handler{deps: V1Deps{ProxyPoolRepo: poolRepo}}
	psd := map[string]any{"connectionProxyEnabled": false}
	sd := map[string]any{"providerStrategies": map[string]any{"opencode": map[string]any{"rotateStrategy": "none"}}}
	strategy := h.resolveNoAuthProxyRotation(context.Background(), psd, sd, "opencode")
	if strategy != "none" {
		t.Fatalf("strategy = %q, want none", strategy)
	}
	if _, has := psd["proxyPoolId"]; has {
		t.Error("none strategy must not set proxyPoolId")
	}
	if v, _ := psd["connectionProxyUrl"].(string); v != "" {
		t.Errorf("none strategy must not inherit proxyUrl, got %q", v)
	}
}

// TestResolveNoAuthProxyRotation_RoundRobinPicksPoolAndInheritsURL verifies the
// rotation picks an active pool with a proxyUrl and merges its proxyUrl into
// psd, and that successive calls cycle through the active pools.
func TestResolveNoAuthProxyRotation_RoundRobinPicksPoolAndInheritsURL(t *testing.T) {
	resetRotateState()
	db := mustOpenDB(t)
	defer db.Close()
	poolRepo := repo.NewProxyPoolRepo(db)
	seedPool(t, db, "pool-a", "socks5://egress-a:1080", true)
	seedPool(t, db, "pool-b", "socks5://egress-b:1080", true)
	// Inactive pool must be skipped even though it has a proxyUrl.
	seedPool(t, db, "pool-c", "socks5://egress-c:1080", false)

	h := &v1Handler{deps: V1Deps{ProxyPoolRepo: poolRepo}}
	sd := map[string]any{"providerStrategies": map[string]any{"opencode": map[string]any{"rotateStrategy": "round-robin"}}}

	var picked []string
	for i := 0; i < 4; i++ {
		psd := map[string]any{"connectionProxyEnabled": false}
		h.resolveNoAuthProxyRotation(context.Background(), psd, sd, "opencode")
		id, _ := psd["proxyPoolId"].(string)
		picked = append(picked, id)
		url, _ := psd["connectionProxyUrl"].(string)
		if url == "" {
			t.Errorf("call %d: proxyUrl not inherited for pool %q", i, id)
		}
		if id == "pool-a" && url != "socks5://egress-a:1080" {
			t.Errorf("pool-a inherited url %q, want socks5://egress-a:1080", url)
		}
		if id == "pool-b" && url != "socks5://egress-b:1080" {
			t.Errorf("pool-b inherited url %q, want socks5://egress-b:1080", url)
		}
	}
	// pool-c is inactive → only a/b are eligible. Round-robin over the 2
	// active pools must (a) never pick pool-c, and (b) alternate (no two
	// consecutive picks equal), independent of the DB's row order.
	for _, id := range picked {
		if id != "pool-a" && id != "pool-b" {
			t.Errorf("picked inactive pool %q (only pool-a/pool-b eligible)", id)
		}
	}
	for i := 1; i < len(picked); i++ {
		if picked[i] == picked[i-1] {
			t.Errorf("round-robin did not alternate: picked = %v", picked)
			break
		}
	}
	if len(picked) == 4 && picked[0] == picked[2] && picked[1] == picked[3] && picked[0] != picked[1] {
		// healthy 2-cycle a/b/a/b or b/a/b/a
	} else {
		t.Errorf("expected a 2-cycle over the active pools, got %v", picked)
	}
}

// TestResolveNoAuthProxyRotation_SkipsPoolsWithoutProxyUrl verifies pools
// without a proxyUrl are excluded from rotation (they cannot route traffic).
func TestResolveNoAuthProxyRotation_SkipsPoolsWithoutProxyUrl(t *testing.T) {
	resetRotateState()
	db := mustOpenDB(t)
	defer db.Close()
	poolRepo := repo.NewProxyPoolRepo(db)
	seedPool(t, db, "pool-no-url", "", true)
	seedPool(t, db, "pool-with-url", "socks5://egress:1080", true)

	h := &v1Handler{deps: V1Deps{ProxyPoolRepo: poolRepo}}
	sd := map[string]any{"providerStrategies": map[string]any{"opencode": map[string]any{"rotateStrategy": "round-robin"}}}
	for i := 0; i < 5; i++ {
		psd := map[string]any{"connectionProxyEnabled": false}
		h.resolveNoAuthProxyRotation(context.Background(), psd, sd, "opencode")
		if id, _ := psd["proxyPoolId"].(string); id != "pool-with-url" {
			t.Errorf("call %d: picked %q, want pool-with-url (no-url pool excluded)", i, id)
		}
	}
}

// TestV1_NoAuthProxyRotation_Integration runs the full v1 handler for a no-auth
// provider (opencode) with rotateStrategy=round-robin and two active pools, and
// verifies the chat handler receives Credentials.ProviderSpecificData carrying
// a rotated proxyPoolId + the inherited proxyUrl.
func TestV1_NoAuthProxyRotation_Integration(t *testing.T) {
	resetRotateState()
	db := mustOpenDB(t)
	defer db.Close()
	poolRepo := repo.NewProxyPoolRepo(db)
	seedPool(t, db, "pool-a", "socks5://egress-a:1080", true)
	seedPool(t, db, "pool-b", "socks5://egress-b:1080", true)

	stub := &stubChatHandler{streamed: true}
	deps := V1Deps{
		APIKeysRepo:    repo.NewAPIKeyRepo(db),
		SettingsRepo:   repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db),
		ComboRepo:      repo.NewComboRepo(db),
		AliasRepo:      repo.NewAliasRepo(db),
		NodeRepo:       repo.NewNodeRepo(db),
		ProxyPoolRepo:  poolRepo,
		Chat:           stub,
		Config:         config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	settingsJSON := `{"providerStrategies":{"opencode":{"rotateStrategy":"round-robin"}}}`
	if _, err := deps.SettingsRepo.Update(context.Background(), json.RawMessage(settingsJSON)); err != nil {
		t.Fatalf("settings update: %v", err)
	}

	mux := http.NewServeMux()
	RegisterV1(mux, deps)

	ids := map[string]bool{}
	for i := 0; i < 2; i++ {
		body := `{"model":"opencode/qwen3","messages":[{"role":"user","content":"hi"}],"stream":true}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, body=%s", i, rec.Code, rec.Body.String())
		}
		if stub.got == nil {
			t.Fatalf("request %d: chat handler did not fire", i)
		}
		psd := stub.got.Credentials.ProviderSpecificData
		id, _ := psd["proxyPoolId"].(string)
		url, _ := psd["connectionProxyUrl"].(string)
		if id != "pool-a" && id != "pool-b" {
			t.Errorf("request %d: proxyPoolId = %q, want pool-a or pool-b", i, id)
		}
		if url == "" {
			t.Errorf("request %d: proxyUrl not inherited into psd", i)
		}
		ids[id] = true
		stub.got = nil
	}
	// Two requests with round-robin over 2 pools → both should be seen.
	if len(ids) != 2 {
		t.Errorf("expected rotation across both pools, saw %v", ids)
	}
}

// TestV1_NoAuthProxyRotation_NoneIntegration verifies the default (no rotate
// strategy) leaves a no-auth provider direct: no proxyPoolId, no proxyUrl.
func TestV1_NoAuthProxyRotation_NoneIntegration(t *testing.T) {
	db := mustOpenDB(t)
	defer db.Close()
	poolRepo := repo.NewProxyPoolRepo(db)
	seedPool(t, db, "pool-a", "socks5://egress-a:1080", true)

	stub := &stubChatHandler{streamed: true}
	deps := V1Deps{
		APIKeysRepo:    repo.NewAPIKeyRepo(db),
		SettingsRepo:   repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db),
		ComboRepo:      repo.NewComboRepo(db),
		AliasRepo:      repo.NewAliasRepo(db),
		NodeRepo:       repo.NewNodeRepo(db),
		ProxyPoolRepo:  poolRepo,
		Chat:           stub,
		Config:         config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	// Default settings → no providerStrategies.opencode.rotateStrategy.

	mux := http.NewServeMux()
	RegisterV1(mux, deps)

	body := `{"model":"opencode/qwen3","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if stub.got == nil {
		t.Fatal("chat handler did not fire")
	}
	psd := stub.got.Credentials.ProviderSpecificData
	if id, has := psd["proxyPoolId"]; has {
		t.Errorf("no rotate strategy must not set proxyPoolId, got %v", id)
	}
	if v, _ := psd["connectionProxyUrl"].(string); v != "" {
		t.Errorf("no rotate strategy must not inherit proxyUrl, got %q", v)
	}
}

func seedPool(t *testing.T, db *sql.DB, id, proxyURL string, isActive bool) {
	t.Helper()
	// Build the pool data blob; pools without a proxyUrl omit the field.
	dataMap := map[string]any{"name": id, "type": "socks5"}
	if proxyURL != "" {
		dataMap["proxyUrl"] = proxyURL
	}
	data, _ := json.Marshal(dataMap)
	poolRepo := repo.NewProxyPoolRepo(db)
	if err := poolRepo.Create(context.Background(), settings.ProxyPool{
		ID:       id,
		IsActive: isActive,
		Data:     data,
	}); err != nil {
		t.Fatalf("create pool %s: %v", id, err)
	}
}

// --- small helpers ---

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
