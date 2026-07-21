package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
)

// TestKeys_GetAndErrorPaths covers GET /api/keys/{id}, missing-key 404 paths
// on get/update/delete, and the create/update validation branches.
func TestKeys_GetAndErrorPaths(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterKeys(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Seed.
	req := httptest.NewRequest("POST", "/api/keys", strings.NewReader(`{"name":"test key"}`))
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

	// GET existing.
	req = httptest.NewRequest("GET", "/api/keys/"+id, nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// GET missing → 404.
	req = httptest.NewRequest("GET", "/api/keys/nope", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing get status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// Create: missing name → 400.
	req = httptest.NewRequest("POST", "/api/keys", strings.NewReader(`{}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing name status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Update: missing id → 404.
	req = httptest.NewRequest("PUT", "/api/keys/nope", strings.NewReader(`{"isActive":false}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update missing status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// Update: existing key without isActive (no-op body still 200).
	req = httptest.NewRequest("PUT", "/api/keys/"+id, strings.NewReader(`{}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update no-op status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Update: invalid body → 400.
	req = httptest.NewRequest("PUT", "/api/keys/"+id, strings.NewReader(`{bad`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Delete: missing id → still 200 (repo delete is idempotent on missing rows).
	req = httptest.NewRequest("DELETE", "/api/keys/nope", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete missing status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}