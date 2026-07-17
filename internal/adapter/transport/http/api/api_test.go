package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	dbschema "github.com/Artiffusion-Inc/9router/internal/adapter/db"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/sqlite"
	domainauth "github.com/Artiffusion-Inc/9router/internal/domain/auth"
)

func mustOpenDB(t *testing.T) *sql.DB {
	t.Helper()
	dir, err := os.MkdirTemp("", "9router-api-test-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	db, err := sqlite.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := dbschema.SyncSchema(db); err != nil {
		t.Fatalf("sync schema: %v", err)
	}
	return db
}

func buildDeps(t *testing.T, db *sql.DB) Deps {
	t.Helper()
	store, err := auth.NewCookieStore("a-very-long-test-secret-12345678")
	if err != nil {
		t.Fatalf("cookie store: %v", err)
	}
	return Deps{
		APIKeys:        repo.NewAPIKeyRepo(db),
		Alias:          repo.NewAliasRepo(db),
		Combos:         repo.NewComboRepo(db),
		Connections:    repo.NewConnectionRepo(db),
		DisabledModels: repo.NewDisabledModelsRepo(db),
		Nodes:          repo.NewNodeRepo(db),
		Pricing:        repo.NewPricingRepo(db),
		ProxyPools:     repo.NewProxyPoolRepo(db),
		RequestDetails: repo.NewRequestDetailRepo(db),
		Settings:       repo.NewSettingsRepo(db),
		Usage:          repo.NewUsageRepo(db),
		SessionStore:   store,
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

func authMiddleware(deps Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") && !IsPublicRoute(r.URL.Path) {
				if _, err := deps.SessionStore.Get(r); err != nil {
					w.Header().Set("WWW-Authenticate", `Bearer`)
					http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func buildMux(t *testing.T, db *sql.DB) http.Handler {
	t.Helper()
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterHealth(mux)
	RegisterVersion(mux, "test")
	RegisterAuth(mux, deps, config.Config{})
	RegisterKeys(mux, deps)
	RegisterCombos(mux, deps)
	RegisterModels(mux, deps)
	RegisterProxyPools(mux, deps)
	RegisterProviders(mux, deps)
	RegisterSettings(mux, deps)
	RegisterPricing(mux, deps)
	RegisterUsage(mux, deps)
	RegisterProviderNodes(mux, deps)
	RegisterLocale(mux)
	RegisterTags(mux)
	RegisterShutdown(mux, deps)
	return authMiddleware(deps)(mux)
}

func authCookie(t *testing.T, store *auth.CookieStore) string {
	t.Helper()
	rec := httptest.NewRecorder()
	sess := domainauth.Session{
		ID:        "test-session",
		ExpiresAt: time.Now().Add(time.Hour),
		Principal: domainauth.Principal{ID: "admin", Name: "Admin"},
	}
	if err := store.Set(rec, sess); err != nil {
		t.Fatalf("set session: %v", err)
	}
	cks := rec.Result().Cookies()
	for _, c := range cks {
		if c.Name == "auth_token" {
			return c.Value
		}
	}
	t.Fatal("auth cookie not found")
	return ""
}

func TestHealth_OK(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	req := httptest.NewRequest("GET", "/api/health", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestKeys_AuthRequired(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	req := httptest.NewRequest("GET", "/api/keys", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestKeys_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterKeys(mux, deps)

	ck := authCookie(t, deps.SessionStore.(*auth.CookieStore))

	// Create
	body := `{"name":"test key"}`
	req := httptest.NewRequest("POST", "/api/keys", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var createResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	id, _ := createResp["id"].(string)
	if id == "" {
		t.Fatalf("expected id in response")
	}

	// List
	req = httptest.NewRequest("GET", "/api/keys", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listResp.Keys) != 1 {
		t.Fatalf("keys count = %d, want 1", len(listResp.Keys))
	}

	// Update (deactivate)
	body = `{"isActive":false}`
	req = httptest.NewRequest("PUT", "/api/keys/"+id, strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Delete
	req = httptest.NewRequest("DELETE", "/api/keys/"+id, nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestVersion_Public(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	req := httptest.NewRequest("GET", "/api/version", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestCombos_AuthRequired(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	req := httptest.NewRequest("GET", "/api/combos", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestCombos_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterCombos(mux, deps)

	ck := authCookie(t, deps.SessionStore.(*auth.CookieStore))

	// Create
	body := `{"name":"combo-a","models":["openai/gpt-4"],"kind":"fallback"}`
	req := httptest.NewRequest("POST", "/api/combos", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("expected id in create response")
	}

	// List
	req = httptest.NewRequest("GET", "/api/combos", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Combos []map[string]any `json:"combos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listResp.Combos) != 1 {
		t.Fatalf("combos count = %d, want 1", len(listResp.Combos))
	}

	// Update
	body = `{"name":"combo-renamed"}`
	req = httptest.NewRequest("PUT", "/api/combos/"+id, strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Delete
	req = httptest.NewRequest("DELETE", "/api/combos/"+id, nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestModels_AuthRequired(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	for _, path := range []string{"/api/models", "/api/models/alias", "/api/models/custom", "/api/models/disabled"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", path, rec.Code)
		}
	}
}

func TestModels_AliasHappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterModels(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*auth.CookieStore))

	// Set alias
	body := `{"model":"openai/gpt-4","alias":"gpt4"}`
	req := httptest.NewRequest("PUT", "/api/models/alias", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set alias status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// List
	req = httptest.NewRequest("GET", "/api/models/alias", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list alias status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Aliases map[string]string `json:"aliases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if listResp.Aliases["gpt4"] != "openai/gpt-4" {
		t.Fatalf("alias not stored: %v", listResp.Aliases)
	}

	// Delete
	req = httptest.NewRequest("DELETE", "/api/models/alias?alias=gpt4", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete alias status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestModels_DisabledHappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterModels(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*auth.CookieStore))

	// Disable
	body := `{"providerAlias":"openai","ids":["gpt-4","gpt-3.5-turbo"]}`
	req := httptest.NewRequest("POST", "/api/models/disabled", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// List by provider
	req = httptest.NewRequest("GET", "/api/models/disabled?providerAlias=openai", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list disabled status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.IDs) != 2 {
		t.Fatalf("disabled count = %d, want 2", len(resp.IDs))
	}

	// Enable one
	req = httptest.NewRequest("DELETE", "/api/models/disabled?providerAlias=openai&id=gpt-4", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestProxyPools_AuthRequired(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	req := httptest.NewRequest("GET", "/api/proxy-pools", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestProxyPools_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterProxyPools(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*auth.CookieStore))

	body := `{"name":"pool-1","proxyUrl":"http://localhost:8080","type":"http"}`
	req := httptest.NewRequest("POST", "/api/proxy-pools", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ProxyPool map[string]any `json:"proxyPool"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	id, _ := created.ProxyPool["id"].(string)
	if id == "" {
		t.Fatal("expected id in create response")
	}

	// List
	req = httptest.NewRequest("GET", "/api/proxy-pools", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Update
	body = `{"name":"pool-renamed"}`
	req = httptest.NewRequest("PUT", "/api/proxy-pools/"+id, strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Delete
	req = httptest.NewRequest("DELETE", "/api/proxy-pools/"+id, nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestProviders_AuthRequired(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	for _, path := range []string{"/api/providers", "/api/providers/client"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", path, rec.Code)
		}
	}
}

func TestProviders_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterProviders(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*auth.CookieStore))

	body := `{"provider":"openai","apiKey":"sk-test","name":"Test OpenAI","priority":1,"testStatus":"active"}`
	req := httptest.NewRequest("POST", "/api/providers", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Connection map[string]any `json:"connection"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	id, _ := created.Connection["id"].(string)
	if id == "" {
		t.Fatal("expected id in create response")
	}

	// List
	req = httptest.NewRequest("GET", "/api/providers", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Connections []map[string]any `json:"connections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listResp.Connections) != 1 {
		t.Fatalf("connections count = %d, want 1", len(listResp.Connections))
	}
}

func TestSettings_Public(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	req := httptest.NewRequest("GET", "/api/settings/require-login", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := resp["requireLogin"]; !ok {
		t.Fatal("expected requireLogin in response")
	}
}

func TestSettings_AuthRequired(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	for _, path := range []string{"/api/settings"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", path, rec.Code)
		}
	}
}

func TestSettings_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterSettings(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*auth.CookieStore))

	req := httptest.NewRequest("GET", "/api/settings", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var getResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if getResp["requireLogin"] != true {
		t.Fatalf("expected requireLogin true, got %v", getResp["requireLogin"])
	}

	body := `{"comboStrategy":"round-robin"}`
	req = httptest.NewRequest("PATCH", "/api/settings", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var patchResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &patchResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if patchResp["comboStrategy"] != "round-robin" {
		t.Fatalf("expected comboStrategy round-robin, got %v", patchResp["comboStrategy"])
	}
}

func TestPricing_AuthRequired(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	req := httptest.NewRequest("GET", "/api/pricing", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestPricing_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterPricing(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*auth.CookieStore))

	body := `{"openai":{"gpt-4":{"input":0.001,"output":0.002}}}`
	req := httptest.NewRequest("PATCH", "/api/pricing", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var patchResp map[string]map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &patchResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := patchResp["openai"]; !ok {
		t.Fatal("expected openai in response")
	}

	// Get
	req = httptest.NewRequest("GET", "/api/pricing", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Delete provider/model
	req = httptest.NewRequest("DELETE", "/api/pricing?provider=openai&model=gpt-4", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestUsage_AuthRequired(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	for _, path := range []string{"/api/usage/history", "/api/usage/stats", "/api/usage/chart", "/api/usage/logs", "/api/usage/request-details", "/api/usage/providers"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", path, rec.Code)
		}
	}
}

func TestUsage_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterUsage(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*auth.CookieStore))

	for _, path := range []string{"/api/usage/history", "/api/usage/stats?period=7d", "/api/usage/chart?period=7d", "/api/usage/logs", "/api/usage/request-details", "/api/usage/providers"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestProviderNodes_AuthRequired(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	for _, path := range []string{"/api/provider-nodes", "/api/provider-nodes/validate"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", path, rec.Code)
		}
	}
}

func TestProviderNodes_HappyPath(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterProviderNodes(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*auth.CookieStore))

	// Create
	body := `{"name":"Node","prefix":"openai-compat","type":"openai-compatible","apiType":"chat"}`
	req := httptest.NewRequest("POST", "/api/provider-nodes", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Node map[string]any `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	id, _ := created.Node["id"].(string)
	if id == "" {
		t.Fatal("expected id in create response")
	}

	// List
	req = httptest.NewRequest("GET", "/api/provider-nodes", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listResp.Nodes) != 1 {
		t.Fatalf("nodes count = %d, want 1", len(listResp.Nodes))
	}

	// Update
	body = `{"name":"Node Renamed","prefix":"renamed","baseUrl":"https://api.openai.com/v1","apiType":"responses"}`
	req = httptest.NewRequest("PUT", "/api/provider-nodes/"+id, strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Delete
	req = httptest.NewRequest("DELETE", "/api/provider-nodes/"+id, nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestProviderNodes_Validate(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterProviderNodes(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*auth.CookieStore))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer server.Close()

	body := fmt.Sprintf(`{"baseUrl":"%s","apiKey":"sk-test","type":"openai-compatible","modelId":"gpt-4"}`, server.URL)
	req := httptest.NewRequest("POST", "/api/provider-nodes/validate", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["valid"] != true {
		t.Fatalf("expected valid true, got %v", resp["valid"])
	}
}

func TestLocale_Public(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	req := httptest.NewRequest("POST", "/api/locale", strings.NewReader(`{"locale":"es"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["locale"] != "es" {
		t.Fatalf("expected locale es, got %v", resp["locale"])
	}
}

func TestTags_Public(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	req := httptest.NewRequest("GET", "/api/tags", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestShutdown_AuthRequired(t *testing.T) {
	mux := buildMux(t, mustOpenDB(t))
	req := httptest.NewRequest("POST", "/api/shutdown", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

var _ = context.Background
