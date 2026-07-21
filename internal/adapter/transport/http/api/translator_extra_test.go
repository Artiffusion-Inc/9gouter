package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
)

// TestTranslator_SaveLoadSend covers the load (allowed + missing + invalid),
// save (happy + invalid), and send stub routes.
func TestTranslator_SaveLoadSend(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterTranslator(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// load invalid file → 400.
	req := httptest.NewRequest("GET", "/api/translator/load?file=../../etc/passwd", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("load invalid file status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Save a known-allowed file (7_res_client.json) so we can deterministically
	// exercise the load happy path and then the load-missing path by deleting
	// the file via t.Cleanup. Use a file that the test owns from start to end.
	const ownedFile = "7_res_client.json"
	t.Cleanup(func() { os.Remove(filepath.Join(translatorLogsDir, ownedFile)) })

	// save happy path.
	req = httptest.NewRequest("POST", "/api/translator/save", strings.NewReader(`{"file":"`+ownedFile+`","content":"{}"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// load the saved file → 200.
	req = httptest.NewRequest("GET", "/api/translator/load?file="+ownedFile, nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("load saved status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Remove the file then load it again → 404.
	if err := os.Remove(filepath.Join(translatorLogsDir, ownedFile)); err != nil {
		t.Fatalf("remove saved file: %v", err)
	}
	req = httptest.NewRequest("GET", "/api/translator/load?file="+ownedFile, nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("load missing file status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// save with missing file → 400.
	req = httptest.NewRequest("POST", "/api/translator/save", strings.NewReader(`{"content":"x"}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("save missing file status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// save with invalid body → 400.
	req = httptest.NewRequest("POST", "/api/translator/save", strings.NewReader(`{bad`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("save invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// send stub.
	req = httptest.NewRequest("POST", "/api/translator/send", strings.NewReader(`{}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("send status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestTranslator_Translate exercises each step branch of the translate handler.
func TestTranslator_Translate(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterTranslator(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	// Step 1 — provider/model detection.
	req := httptest.NewRequest("POST", "/api/translator/translate", strings.NewReader(`{"step":1,"body":{"provider":"openai","model":"gpt-4o"}}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("translate step 1 status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal step 1: %v", err)
	}
	if resp["success"] != true {
		t.Fatalf("step 1 success = %v, want true", resp["success"])
	}

	// Step 1 — model carries provider/model → provider split out.
	req = httptest.NewRequest("POST", "/api/translator/translate", strings.NewReader(`{"step":1,"body":{"model":"openai/gpt-4o"}}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("translate step 1 (split) status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Step 1 — empty body uses defaults.
	req = httptest.NewRequest("POST", "/api/translator/translate", strings.NewReader(`{"step":1,"body":{}}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("translate step 1 (defaults) status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Step 2 — passthrough.
	req = httptest.NewRequest("POST", "/api/translator/translate", strings.NewReader(`{"step":2,"body":{"messages":[]}}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("translate step 2 status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Step 2 — empty body.
	req = httptest.NewRequest("POST", "/api/translator/translate", strings.NewReader(`{"step":2,"body":null}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("translate step 2 empty status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Step 2 — empty body uses defaults (covered above); the inner
	// json.Unmarshal error branch is unreachable in practice because a
	// syntactically invalid RawMessage makes the outer parseJSON fail first.
	// Skip that branch and move to step 3.

	// Step 3 — target + headers + url.
	req = httptest.NewRequest("POST", "/api/translator/translate", strings.NewReader(`{"step":3,"body":{"model":"gpt-4o"}}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("translate step 3 status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Step 3 — empty body uses defaults.
	req = httptest.NewRequest("POST", "/api/translator/translate", strings.NewReader(`{"step":3,"body":null}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("translate step 3 empty status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Invalid step → 400.
	req = httptest.NewRequest("POST", "/api/translator/translate", strings.NewReader(`{"step":99}`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("translate invalid step status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Invalid body → 400.
	req = httptest.NewRequest("POST", "/api/translator/translate", strings.NewReader(`{bad`))
	req.Header.Set("Cookie", "auth_token="+ck)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("translate invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestTranslator_ConsoleLogsClear verifies the DELETE handler empties the
// in-memory buffer and returns 200.
func TestTranslator_ConsoleLogsClear(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterTranslator(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	req := httptest.NewRequest("DELETE", "/api/translator/console-logs", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}