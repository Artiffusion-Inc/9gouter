package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dispatchRecorder captures the rewritten path a passthrough delegates to.
type dispatchRecorder struct {
	gotPath string
}

func (d *dispatchRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.gotPath = r.URL.Path
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
}

// newDashboardMux builds a ServeMux with only RegisterV1Dashboard wired,
// optionally injecting V1Dispatch. It reuses the shared mustOpenDB helper.
func newDashboardMux(t *testing.T, dispatch http.HandlerFunc) *http.ServeMux {
	t.Helper()
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	deps := buildDeps(t, db)
	if dispatch != nil {
		deps.V1Dispatch = dispatch
	}
	mux := http.NewServeMux()
	RegisterV1Dashboard(mux, deps)
	return mux
}

func TestV1Dashboard_Passthrough_RewritesPath(t *testing.T) {
	rec := &dispatchRecorder{}
	mux := newDashboardMux(t, rec.ServeHTTP)

	req := httptest.NewRequest("GET", "/api/v1/models", nil)
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rec.gotPath != "/v1/models" {
		t.Fatalf("dispatch path = %q, want /v1/models", rec.gotPath)
	}
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
}

func TestV1Dashboard_Passthrough_KindSubstituted(t *testing.T) {
	rec := &dispatchRecorder{}
	mux := newDashboardMux(t, rec.ServeHTTP)

	req := httptest.NewRequest("GET", "/api/v1/models/image", nil)
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rec.gotPath != "/v1/models/image" {
		t.Fatalf("dispatch path = %q, want /v1/models/image", rec.gotPath)
	}
}

func TestV1Dashboard_Passthrough_NoDispatch_DegradesToStub(t *testing.T) {
	mux := newDashboardMux(t, nil)

	req := httptest.NewRequest("GET", "/api/v1/models", nil)
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if !containsSubstr(rw.Body.String(), "not yet available") {
		t.Fatalf("expected not-available stub body, got: %s", rw.Body.String())
	}
}

func TestV1Dashboard_NotAvailable_ForUnimplemented(t *testing.T) {
	rec := &dispatchRecorder{}
	mux := newDashboardMux(t, rec.ServeHTTP)

	// No route is registered as a direct notAvailable stub anymore (every
	// /api/v1/* route is a passthrough). This test now asserts that an
	// unregistered /api/v1 path yields 404 rather than dispatching — keeping
	// the guard that unknown surfaces do not silently dispatch.
	req := httptest.NewRequest("POST", "/api/v1/does-not-exist", nil)
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rec.gotPath != "" {
		t.Fatalf("unknown /api/v1 path dispatched to %q — must not dispatch", rec.gotPath)
	}
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unregistered /api/v1 path, got %d", rw.Code)
	}
}

// TestV1Dashboard_Passthrough_Search verifies /api/v1/search now rewrites to
// /v1/search and dispatches (it was a notAvailable stub before the T033b-1
// search pipeline landed).
func TestV1Dashboard_Passthrough_Search(t *testing.T) {
	rec := &dispatchRecorder{}
	mux := newDashboardMux(t, rec.ServeHTTP)

	req := httptest.NewRequest("POST", "/api/v1/search", strings.NewReader(`{"query":"x"}`))
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rec.gotPath != "/v1/search" {
		t.Fatalf("dispatch path = %q, want /v1/search", rec.gotPath)
	}
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
}

// TestV1Dashboard_Passthrough_ResponsesGet verifies GET /api/v1/responses/{id}
// rewrites to /v1/responses/{id} and forwards the id path value.
func TestV1Dashboard_Passthrough_ResponsesGet(t *testing.T) {
	rec := &dispatchRecorder{}
	mux := newDashboardMux(t, rec.ServeHTTP)

	req := httptest.NewRequest("GET", "/api/v1/responses/resp_123", nil)
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rec.gotPath != "/v1/responses/resp_123" {
		t.Fatalf("dispatch path = %q, want /v1/responses/resp_123", rec.gotPath)
	}
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (dispatch stub)", rw.Code)
	}
}

// TestV1Dashboard_Passthrough_Embeddings verifies /api/v1/embeddings now
// rewrites to /v1/embeddings and dispatches (it was a notAvailable stub before
// the T031b embeddings pipeline landed).
func TestV1Dashboard_Passthrough_Embeddings(t *testing.T) {
	rec := &dispatchRecorder{}
	mux := newDashboardMux(t, rec.ServeHTTP)

	req := httptest.NewRequest("POST", "/api/v1/embeddings", nil)
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rec.gotPath != "/v1/embeddings" {
		t.Fatalf("dispatch path = %q, want /v1/embeddings", rec.gotPath)
	}
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
}

// TestV1Dashboard_Passthrough_ModelsInfo verifies /api/v1/models/info now
// rewrites to /v1/models/info and dispatches (it was a notAvailable stub before
// the T033b models/info endpoint landed). Query string is preserved.
func TestV1Dashboard_Passthrough_ModelsInfo(t *testing.T) {
	rec := &dispatchRecorder{}
	mux := newDashboardMux(t, rec.ServeHTTP)

	req := httptest.NewRequest("GET", "/api/v1/models/info?id=openai/dall-e-3&kind=image", nil)
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rec.gotPath != "/v1/models/info" {
		t.Fatalf("dispatch path = %q, want /v1/models/info", rec.gotPath)
	}
	if q := req.URL.RawQuery; q != "id=openai/dall-e-3&kind=image" {
		t.Fatalf("query not preserved: %q", q)
	}
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
}

func containsSubstr(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}