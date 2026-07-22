package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
)

// TestProviders_CreateValidation covers the create error branches (invalid
// provider, missing API key, displayName fallback, providerSpecificData merge,
// testStatus injection) and the GET /api/providers/client surface.
func TestProviders_CreateValidation(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterProviders(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Invalid body.
	req := httptest.NewRequest("POST", "/api/providers", strings.NewReader(`{bad`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Missing provider.
	req = httptest.NewRequest("POST", "/api/providers", strings.NewReader(`{"apiKey":"k"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing provider status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Missing API key.
	req = httptest.NewRequest("POST", "/api/providers", strings.NewReader(`{"provider":"openai"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing api key status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Happy path with displayName fallback + proxyPoolId + testStatus +
	// providerSpecificData. Verifies the merge branch.
	body := `{"provider":"openai","apiKey":"sk-test","displayName":"My OpenAI","priority":3,"testStatus":"active","proxyPoolId":"pp-1","providerSpecificData":{"baseUrl":"http://x"}}`
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
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.Connection["name"] != "My OpenAI" {
		t.Fatalf("name = %v, want My OpenAI (displayName fallback)", created.Connection["name"])
	}
	if created.Connection["apiKey"] != nil {
		t.Fatalf("apiKey must be stripped from response, got %v", created.Connection["apiKey"])
	}
	if created.Connection["proxyPoolId"] != "pp-1" {
		t.Fatalf("proxyPoolId = %v, want pp-1", created.Connection["proxyPoolId"])
	}
}

// TestProviders_Client covers GET /api/providers/client — query param parsing
// and pagination defaults.
func TestProviders_Client(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterProviders(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	for _, q := range []string{
		"",
		"?provider=openai",
		"?accountStatus=active",
		"?sort=name",
		"?page=2&pageSize=5",
		"?page=0&pageSize=0", // defaults applied
		"?pageSize=1000",     // clamped to 20
	} {
		req := httptest.NewRequest("GET", "/api/providers/client"+q, nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("client %s status = %d, want 200; body=%s", q, rec.Code, rec.Body.String())
		}
	}
}

// TestProvidersExtra_CRUD covers the GET/PUT/DELETE/test/testBatch/validate
// routes registered by RegisterProvidersExtra.
func TestProvidersExtra_CRUD(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterProviders(mux, deps)
	RegisterProvidersExtra(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Seed a connection.
	body := `{"provider":"openai","apiKey":"sk-test","name":"P","priority":1}`
	req := httptest.NewRequest("POST", "/api/providers", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Connection map[string]any `json:"connection"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created.Connection["id"].(string)

	// GET /api/providers/{id}.
	req = httptest.NewRequest("GET", "/api/providers/"+id, nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// GET /api/providers/{id} missing → 404.
	req = httptest.NewRequest("GET", "/api/providers/nope", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get missing status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// POST /api/providers/{id}/test — connection found, valid=true (active).
	req = httptest.NewRequest("POST", "/api/providers/"+id+"/test", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("test status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var testResp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &testResp)
	if testResp["valid"] != true {
		t.Fatalf("test valid = %v, want true (active connection)", testResp["valid"])
	}

	// POST /api/providers/{id}/test missing → 404.
	req = httptest.NewRequest("POST", "/api/providers/nope/test", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("test missing status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// GET /test-models is still a stub → 200.
	req = httptest.NewRequest("GET", "/api/providers/"+id+"/test-models", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("test-models status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// GET /models for openai: no live resolver, no static catalog → 400.
	req = httptest.NewRequest("GET", "/api/providers/"+id+"/models", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("openai models status = %d, want 400 (no resolver/catalog); body=%s", rec.Code, rec.Body.String())
	}

	// PUT /api/providers/{id} — priority update.
	req = httptest.NewRequest("PUT", "/api/providers/"+id, strings.NewReader(`{"priority":7}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update priority status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// PUT /api/providers/{id} — isActive update.
	req = httptest.NewRequest("PUT", "/api/providers/"+id, strings.NewReader(`{"isActive":false}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update isActive status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// PUT /api/providers/{id} — isActive not a bool → 400.
	req = httptest.NewRequest("PUT", "/api/providers/"+id, strings.NewReader(`{"isActive":"yes"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update isActive not bool status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// PUT /api/providers/{id} — blob patch (extra fields merged into data).
	req = httptest.NewRequest("PUT", "/api/providers/"+id, strings.NewReader(`{"baseUrl":"http://new"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update blob patch status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// PUT /api/providers/{id} missing → 404.
	req = httptest.NewRequest("PUT", "/api/providers/nope", strings.NewReader(`{"priority":7}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update missing status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// DELETE /api/providers/{id}.
	req = httptest.NewRequest("DELETE", "/api/providers/"+id, nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// DELETE missing — idempotent: still 200.
	req = httptest.NewRequest("DELETE", "/api/providers/nope", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete missing status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestProvidersExtra_ModelsEndpoint covers GET /api/providers/{id}/models —
// the v0.5.40 live model catalog handler. A provider with a static catalog
// (gemini) and no live resolver returns the static list with 200; a missing
// connection returns 404; a provider with neither resolver nor catalog returns
// 400. (The live-resolver path itself — codex client_version gate, refresh on
// 401 — is covered end-to-end by resolver/codex_test.go against an httptest
// server.)
func TestProvidersExtra_ModelsEndpoint(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterProviders(mux, deps)
	RegisterProvidersExtra(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Seed a gemini connection (gemini has a static catalog, no live resolver).
	body := `{"provider":"gemini","apiKey":"sk-gem","name":"G","priority":1}`
	req := httptest.NewRequest("POST", "/api/providers", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Connection map[string]any `json:"connection"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created.Connection["id"].(string)

	// GET /models → 200 + static catalog.
	req = httptest.NewRequest("GET", "/api/providers/"+id+"/models", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("gemini models status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Provider     string              `json:"provider"`
		ConnectionID string              `json:"connectionId"`
		Models       []map[string]any    `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if resp.Provider != "gemini" || resp.ConnectionID != id {
		t.Errorf("resp = %+v, want provider=gemini connectionId=%s", resp, id)
	}
	if len(resp.Models) == 0 {
		t.Fatalf("expected static gemini models, got empty list; body=%s", rec.Body.String())
	}
	if _, ok := resp.Models[0]["id"].(string); !ok {
		t.Errorf("first model missing id: %+v", resp.Models[0])
	}

	// Missing connection → 404.
	req = httptest.NewRequest("GET", "/api/providers/nope/models", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing connection models status = %d, want 404", rec.Code)
	}
}

func TestProvidersExtra_StaticAndBatch(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterProviders(mux, deps)
	RegisterProvidersExtra(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	for _, p := range []string{"/api/providers/suggested-models", "/api/providers/kilo/free-models"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("Cookie", "auth_token="+ck)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", p, rec.Code, rec.Body.String())
		}
	}

	// validate stub.
	req := httptest.NewRequest("POST", "/api/providers/validate", strings.NewReader(`{}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// testBatch with mode=provider.
	req = httptest.NewRequest("POST", "/api/providers/test-batch", strings.NewReader(`{"mode":"provider","providerId":"openai"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("testBatch provider status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var batchResp struct {
		Summary map[string]int `json:"summary"`
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &batchResp); err != nil {
		t.Fatalf("unmarshal testBatch: %v", err)
	}

	// testBatch with mode=all (default).
	req = httptest.NewRequest("POST", "/api/providers/test-batch", strings.NewReader(`{"mode":"all"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("testBatch all status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// testBatch with legacy explicit ids.
	req = httptest.NewRequest("POST", "/api/providers/test-batch", strings.NewReader(`{"ids":["nope-1","nope-2"]}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("testBatch ids status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &batchResp); err != nil {
		t.Fatalf("unmarshal testBatch ids: %v", err)
	}
	if batchResp.Summary["total"] != 2 {
		t.Fatalf("testBatch ids total = %d, want 2", batchResp.Summary["total"])
	}

	// testBatch with legacy connections list shape.
	req = httptest.NewRequest("POST", "/api/providers/test-batch", strings.NewReader(`{"connections":[{"id":"nope-3"}]}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("testBatch connections status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}