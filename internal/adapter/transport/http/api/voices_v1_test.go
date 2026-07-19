package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleV1AudioVoices_EdgeTTS verifies the OpenAI-style normalization:
// edge-tts voices self-fetched from the internal endpoint come back as
// {object:"list", data:[{id,name,lang,gender,model:"edge-tts/<id>"}]}.
func TestHandleV1AudioVoices_EdgeTTS(t *testing.T) {
	dispatch := func(w http.ResponseWriter, r *http.Request) {
		// Internal edge-tts voices endpoint returns byLang (no lang filter).
		writeJSON(w, http.StatusOK, map[string]any{
			"byLang": map[string]any{
				"en": map[string]any{
					"code":    "en",
					"name":    "en",
					"voices": []any{
						map[string]any{"id": "en-US-AriaNeural", "name": "Aria", "lang": "en", "gender": "Female"},
					},
				},
			},
		})
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/audio/voices?provider=edge-tts", nil)
	rec := httptest.NewRecorder()
	HandleV1AudioVoices(rec, req, dispatch)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out["object"] != "list" {
		t.Errorf("object = %v, want list", out["object"])
	}
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len = %d, want 1", len(data))
	}
	v, _ := data[0].(map[string]any)
	if v["model"] != "edge-tts/en-US-AriaNeural" {
		t.Errorf("model = %v, want edge-tts/en-US-AriaNeural", v["model"])
	}
	if v["id"] != "en-US-AriaNeural" {
		t.Errorf("id = %v", v["id"])
	}
}

// TestHandleV1AudioVoices_ElevenlabsAlias verifies the "el" alias prefix
// (AI_PROVIDERS.elevenlabs.alias = "el") and the {voices:[...]} shape used
// when a lang filter is applied.
func TestHandleV1AudioVoices_ElevenlabsAlias(t *testing.T) {
	dispatch := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"voices": []any{
				map[string]any{"id": "21m00Tj4", "name": "Rachel", "lang": "en", "gender": "Female"},
			},
		})
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/audio/voices?provider=elevenlabs&lang=en", nil)
	rec := httptest.NewRecorder()
	HandleV1AudioVoices(rec, req, dispatch)

	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len = %d, want 1; body=%s", len(data), rec.Body.String())
	}
	v, _ := data[0].(map[string]any)
	if v["model"] != "el/21m00Tj4" {
		t.Errorf("model = %v, want el/21m00Tj4", v["model"])
	}
}

// TestHandleV1AudioVoices_UnknownProvider verifies a missing/unknown provider
// returns 400 with an invalid_request_error.
func TestHandleV1AudioVoices_UnknownProvider(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/audio/voices?provider=foo", nil)
	rec := httptest.NewRecorder()
	HandleV1AudioVoices(rec, req, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	errObj, _ := out["error"].(map[string]any)
	if errObj["type"] != "invalid_request_error" {
		t.Errorf("error.type = %v, want invalid_request_error", errObj["type"])
	}
}

// TestHandleV1AudioVoices_MissingProvider returns 400 when provider is absent.
func TestHandleV1AudioVoices_MissingProvider(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/audio/voices", nil)
	rec := httptest.NewRecorder()
	HandleV1AudioVoices(rec, req, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestHandleV1AudioVoices_UpstreamFailure returns 502 when the internal
// voice-list dispatch fails (non-2xx).
func TestHandleV1AudioVoices_UpstreamFailure(t *testing.T) {
	dispatch := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"oops"}`))
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/audio/voices?provider=deepgram", nil)
	rec := httptest.NewRecorder()
	HandleV1AudioVoices(rec, req, dispatch)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}