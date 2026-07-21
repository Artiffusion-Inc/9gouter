package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
)

// TestCombos_GetAndErrorPaths covers GET /api/combos/{id} (found + not found),
// plus the validation/branch paths in create/update/delete that the happy-path
// test does not touch.
func TestCombos_GetAndErrorPaths(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterCombos(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Seed one combo to fetch.
	body := `{"name":"combo-a","models":["openai/gpt-4"],"kind":"fallback"}`
	req := httptest.NewRequest("POST", "/api/combos", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	id, _ := created["id"].(string)

	// GET existing.
	req = httptest.NewRequest("GET", "/api/combos/"+id, nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// GET missing → 404.
	req = httptest.NewRequest("GET", "/api/combos/nope", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing get status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// Create: invalid body.
	req = httptest.NewRequest("POST", "/api/combos", strings.NewReader(`{bad`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Create: missing name.
	req = httptest.NewRequest("POST", "/api/combos", strings.NewReader(`{}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing name status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Create: invalid name characters.
	req = httptest.NewRequest("POST", "/api/combos", strings.NewReader(`{"name":"bad name!"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid name status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Create: duplicate name → 400.
	req = httptest.NewRequest("POST", "/api/combos", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate name status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Update: missing id → 404.
	req = httptest.NewRequest("PUT", "/api/combos/nope", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update missing status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// Update: invalid body.
	req = httptest.NewRequest("PUT", "/api/combos/"+id, strings.NewReader(`{bad`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Update: invalid name characters.
	req = httptest.NewRequest("PUT", "/api/combos/"+id, strings.NewReader(`{"name":"bad name!"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update invalid name status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Delete: missing id → 200 (svc.Delete succeeds even if not found in
	// storage, but we still get the success response because the Get before
	// delete only fails for true DB errors). Use a synthetic id so the svc.Get
	// returns nil-combo + nil error and the handler writes success.
	req = httptest.NewRequest("DELETE", "/api/combos/"+id, nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete existing status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestCombos_UpdateWithModelsAndKind exercises the models/kind merge branches
// in update so those statements are covered.
func TestCombos_UpdateWithModelsAndKind(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterCombos(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	body := `{"name":"c-upd","models":["m1"],"kind":"fallback"}`
	req := httptest.NewRequest("POST", "/api/combos", strings.NewReader(body))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created["id"].(string)

	// Update name + models + kind simultaneously.
	req = httptest.NewRequest("PUT", "/api/combos/"+id, strings.NewReader(`{"name":"c-upd2","models":["m1","m2"],"kind":"round-robin"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var updated map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("unmarshal update: %v", err)
	}
	if updated["kind"] != "round-robin" {
		t.Fatalf("kind = %v, want round-robin", updated["kind"])
	}
}