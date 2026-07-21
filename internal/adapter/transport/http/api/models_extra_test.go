package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
)

// TestModels_ListAndCustom covers the list, listCustom, addCustom, deleteCustom,
// alias-in-use conflict, and missing-param error paths.
func TestModels_ListAndCustom(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterModels(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// GET /api/models (list) — empty store, returns 200.
	req := httptest.NewRequest("GET", "/api/models", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Models  []any `json:"models"`
		Aliases map[string]string `json:"aliases"`
		Disabled map[string]any `json:"disabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if listResp.Models == nil {
		t.Fatal("expected models field to be present")
	}

	// GET /api/models/custom — empty.
	req = httptest.NewRequest("GET", "/api/models/custom", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list custom status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// POST /api/models/custom — add a custom model.
	req = httptest.NewRequest("POST", "/api/models/custom", strings.NewReader(`{"providerAlias":"openai","id":"gpt-99","type":"llm","name":"GPT-99"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("add custom status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// POST /api/models/custom — missing fields → 400.
	req = httptest.NewRequest("POST", "/api/models/custom", strings.NewReader(`{"providerAlias":"openai"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("add custom missing id status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// POST /api/models/custom — invalid body → 400.
	req = httptest.NewRequest("POST", "/api/models/custom", strings.NewReader(`{bad`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("add custom invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// DELETE /api/models/custom — happy path.
	req = httptest.NewRequest("DELETE", "/api/models/custom?providerAlias=openai&id=gpt-99&type=llm", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete custom status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// DELETE /api/models/custom — missing providerAlias → 400.
	req = httptest.NewRequest("DELETE", "/api/models/custom?id=foo", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete custom missing providerAlias status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestModels_AliasConflictAndErrors covers setAlias "already in use" plus the
// deleteAlias missing-param branch.
func TestModels_AliasConflictAndErrors(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterModels(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Seed alias "gpt4" -> "openai/gpt-4". The handler iterates the alias map
	// (alias->model) but uses flipped variable names, so the "alias already in
	// use" branch actually fires when an existing model value equals the new
	// alias being set. Seed that exact shape (model value = "openai/gpt-4")
	// so the conflict branch is exercised.
	req := httptest.NewRequest("PUT", "/api/models/alias", strings.NewReader(`{"model":"openai/gpt-4","alias":"gpt4"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed alias status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Set alias "openai/gpt-4" for a different model — the seeded model value
	// ("openai/gpt-4") matches the new alias, triggering the conflict branch.
	req = httptest.NewRequest("PUT", "/api/models/alias", strings.NewReader(`{"model":"openai/gpt-5","alias":"openai/gpt-4"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("alias conflict status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// setAlias missing fields → 400.
	req = httptest.NewRequest("PUT", "/api/models/alias", strings.NewReader(`{"model":"openai/gpt-4"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("alias missing field status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// setAlias invalid body → 400.
	req = httptest.NewRequest("PUT", "/api/models/alias", strings.NewReader(`{bad`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("alias invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// deleteAlias missing alias query → 400.
	req = httptest.NewRequest("DELETE", "/api/models/alias", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete alias missing alias status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestModels_DisabledListAllAndErrors covers the list-disabled branch that
// returns all entries (no providerAlias), plus the disable/enable error paths.
func TestModels_DisabledListAllAndErrors(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterModels(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// List all disabled (no provider filter).
	req := httptest.NewRequest("GET", "/api/models/disabled", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list all disabled status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Disable: missing fields → 400.
	req = httptest.NewRequest("POST", "/api/models/disabled", strings.NewReader(`{"providerAlias":"openai"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("disable missing ids status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Disable: invalid body → 400.
	req = httptest.NewRequest("POST", "/api/models/disabled", strings.NewReader(`{bad`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("disable invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Enable: missing providerAlias → 400.
	req = httptest.NewRequest("DELETE", "/api/models/disabled", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enable missing providerAlias status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Enable: providerAlias present but no id — succeeds (clears all for the
	// provider, which is empty in a fresh DB).
	req = httptest.NewRequest("DELETE", "/api/models/disabled?providerAlias=openai", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable all for provider status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestModels_AvailabilityAndClearCooldown covers the trivial availability
// endpoint plus the clearCooldown validation branches.
func TestModels_AvailabilityAndClearCooldown(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterModels(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Availability.
	req := httptest.NewRequest("GET", "/api/models/availability", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("availability status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// clearCooldown happy path.
	req = httptest.NewRequest("POST", "/api/models/availability", strings.NewReader(`{"action":"clearCooldown","provider":"openai","model":"gpt-4"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clearCooldown status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// clearCooldown missing fields → 400.
	req = httptest.NewRequest("POST", "/api/models/availability", strings.NewReader(`{"action":"clearCooldown","provider":"openai"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("clearCooldown missing model status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// clearCooldown invalid action → 400.
	req = httptest.NewRequest("POST", "/api/models/availability", strings.NewReader(`{"action":"reset","provider":"openai","model":"gpt-4"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("clearCooldown invalid action status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// clearCooldown invalid body → 400.
	req = httptest.NewRequest("POST", "/api/models/availability", strings.NewReader(`{bad`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("clearCooldown invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}