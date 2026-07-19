package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider"
)

// newModelsInfoHandler wires a v1Handler with minimal deps for handleModelsInfo
// (which only reads the static provider catalog — no DB rows needed).
func newModelsInfoHandler(t *testing.T) *v1Handler {
	t.Helper()
	h, _ := newModelsHandler(t)
	return h
}

func doModelsInfo(t *testing.T, h *v1Handler, query string) (*httptest.ResponseRecorder, modelInfoResponse, []byte) {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/models/info?"+query, nil)
	rw := httptest.NewRecorder()
	h.handleModelsInfo(rw, req)
	body := rw.Body.Bytes()
	var info modelInfoResponse
	_ = json.Unmarshal(body, &info)
	return rw, info, body
}

// findCatalogModel scans AllCatalogs for an alias+modelID so tests can target a
// real static entry instead of hardcoding one that may drift.
func findCatalogModel(t *testing.T, wantKind string) (alias, modelID, kind string) {
	t.Helper()
	for _, cat := range provider.AllCatalogs() {
		for _, m := range cat.Models {
			mk := m.Kind
			if mk == "" {
				mk = "llm"
			}
			if mk == wantKind {
				return cat.Alias, m.ID, mk
			}
		}
	}
	t.Skipf("no static catalog model of kind %q registered", wantKind)
	return
}

// TestModelsInfo_MissingID returns 400 when ?id is absent.
func TestModelsInfo_MissingID(t *testing.T) {
	h := newModelsInfoHandler(t)
	rw, _, _ := doModelsInfo(t, h, "")
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rw.Code)
	}
}

// TestModelsInfo_NotFound returns 404 for an unknown alias/model.
func TestModelsInfo_NotFound(t *testing.T) {
	h := newModelsInfoHandler(t)
	rw, _, _ := doModelsInfo(t, h, "id=does-not-exist/no-such-model")
	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rw.Code)
	}
}

// TestModelsInfo_StaticLLM returns 200 + {id,name,kind,owned_by,endpoint} for a
// real static catalog LLM model, endpoint = /v1/chat/completions.
func TestModelsInfo_StaticLLM(t *testing.T) {
	h := newModelsInfoHandler(t)
	alias, modelID, _ := findCatalogModel(t, "llm")
	rw, info, _ := doModelsInfo(t, h, "id="+alias+"/"+modelID)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if info.ID != alias+"/"+modelID {
		t.Errorf("id = %q, want %q", info.ID, alias+"/"+modelID)
	}
	if info.Kind != "llm" {
		t.Errorf("kind = %q, want llm", info.Kind)
	}
	if info.OwnedBy != alias {
		t.Errorf("owned_by = %q, want %q", info.OwnedBy, alias)
	}
	if info.Endpoint != "/v1/chat/completions" {
		t.Errorf("endpoint = %q, want /v1/chat/completions", info.Endpoint)
	}
	if info.Name == "" {
		t.Error("name empty")
	}
}

// TestModelsInfo_KindDisambiguation: when the same model id exists under
// multiple kinds, ?kind= selects the right one. We can't guarantee a dup in the
// static catalog, so this asserts the kind filter never returns a mismatched
// kind and returns 404 when the requested kind does not match any entry.
func TestModelsInfo_KindDisambiguation(t *testing.T) {
	h := newModelsInfoHandler(t)
	alias, modelID, kind := findCatalogModel(t, "llm")
	_ = kind
	// Requesting a non-matching kind -> 404 (no entry of that kind for this id).
	rw, _, _ := doModelsInfo(t, h, "id="+alias+"/"+modelID+"&kind=image")
	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for non-matching kind", rw.Code)
	}
	// Requesting the matching kind -> 200.
	rw, info, _ := doModelsInfo(t, h, "id="+alias+"/"+modelID+"&kind="+kind)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for matching kind", rw.Code)
	}
	if info.Kind != kind {
		t.Errorf("kind = %q, want %q", info.Kind, kind)
	}
}

// TestModelsInfo_BadIDFormat returns 404 for an id with no slash.
func TestModelsInfo_BadIDFormat(t *testing.T) {
	h := newModelsInfoHandler(t)
	rw, _, _ := doModelsInfo(t, h, "id=no-slash-here")
	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for id without slash", rw.Code)
	}
}

// TestModelsInfo_CORSHeader verifies the Access-Control-Allow-Origin header is
// set, matching the rest of the /v1 surface.
func TestModelsInfo_CORSHeader(t *testing.T) {
	h := newModelsInfoHandler(t)
	alias, modelID, _ := findCatalogModel(t, "llm")
	rw, _, _ := doModelsInfo(t, h, "id="+alias+"/"+modelID)
	if got := rw.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("CORS = %q, want *", got)
	}
	if ct := rw.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// TestKindEndpointMap verifies the kind->endpoint table covers every service
// kind the catalog / JS reference declares.
func TestKindEndpointMap(t *testing.T) {
	want := map[string]string{
		"llm":         "/v1/chat/completions",
		"image":       "/v1/images/generations",
		"tts":         "/v1/audio/speech",
		"stt":         "/v1/audio/transcriptions",
		"embedding":   "/v1/embeddings",
		"imageToText": "/v1/chat/completions",
		"webSearch":   "/v1/search",
		"webFetch":    "/v1/web/fetch",
	}
	for k, v := range want {
		if kindEndpoint[k] != v {
			t.Errorf("kindEndpoint[%q] = %q, want %q", k, kindEndpoint[k], v)
		}
	}
}