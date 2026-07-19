package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
)

// ttsVoiceProviderAPI mirrors the legacy JS PROVIDER_API map in
// src/app/api/v1/audio/voices/route.js: each TTS voice-capable provider maps
// to the internal /api/media-providers/tts path that lists its voices.
//
// "edge-tts" and "local-device" share the generic voices endpoint; the other
// providers have per-provider sub-routes.
var ttsVoiceProviderAPI = map[string]string{
	"elevenlabs":   "/api/media-providers/tts/elevenlabs/voices",
	"deepgram":     "/api/media-providers/tts/deepgram/voices",
	"inworld":      "/api/media-providers/tts/inworld/voices",
	"edge-tts":     "/api/media-providers/tts/voices?provider=edge-tts",
	"local-device": "/api/media-providers/tts/voices?provider=local-device",
}

// ttsVoiceProviderAlias mirrors the legacy JS AI_PROVIDERS[provider].alias.
// The voice's OpenAI-style `model` field is "<alias>/<voiceId>" so /v1/audio/
// speech clients can request it directly.
var ttsVoiceProviderAlias = map[string]string{
	"elevenlabs":   "el",
	"deepgram":     "deepgram",
	"inworld":      "inworld",
	"edge-tts":     "edge-tts",
	"local-device": "local-device",
}

// HandleV1AudioVoices implements GET /v1/audio/voices?provider={p}[&lang=xx].
// It mirrors the legacy JS src/app/api/v1/audio/voices/route.js: validate the
// provider, fetch the internal voice list, normalize to the OpenAI-style
// {object:"list", data:[{id,name,lang,gender,model}]} shape where model is
// "<alias>/<voiceId>". Returns 400 for an unknown/missing provider.
//
// dispatch lets the caller route the internal fetch back through the same
// mux (so the per-provider handlers in RegisterMediaProviders serve it),
// matching the JS pattern of self-fetching the internal voices API.
func HandleV1AudioVoices(w http.ResponseWriter, r *http.Request, dispatch http.HandlerFunc) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	allowed := ttsVoiceProviderKeys()
	if _, ok := ttsVoiceProviderAPI[provider]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": "provider must be one of: " + strings.Join(allowed, ", "),
				"type":    "invalid_request_error",
			},
		})
		return
	}
	lang := r.URL.Query().Get("lang")
	internalPath := ttsVoiceProviderAPI[provider]

	// Fetch the internal voice list by re-dispatching through the same mux.
	rec := httptest.NewRecorder()
	intReq := httptest.NewRequest(http.MethodGet, internalPath, nil)
	if lang != "" {
		q := intReq.URL.Query()
		q.Set("lang", lang)
		intReq.URL.RawQuery = q.Encode()
	}
	if dispatch != nil {
		dispatch(rec, intReq)
	}
	var data map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil || rec.Code >= 400 {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{
				"message": "Upstream voice list failed",
				"type":    "server_error",
			},
		})
		return
	}
	rawVoices := extractRawVoices(data, lang)
	alias := ttsVoiceProviderAlias[provider]
	if alias == "" {
		alias = provider
	}
	out := make([]map[string]any, 0, len(rawVoices))
	for _, v := range rawVoices {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		id, _ := vm["id"].(string)
		if id == "" {
			continue
		}
		out = append(out, map[string]any{
			"id":     id,
			"name":   vm["name"],
			"lang":   vm["lang"],
			"gender": vm["gender"],
			"model":  alias + "/" + id,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   out,
	})
}

// extractRawVoices mirrors the JS rawVoices extraction: when a lang filter is
// applied the internal API returns {voices:[...]}; otherwise it returns
// {byLang:{...}, languages:[...]} and the voices are flattened from byLang.
func extractRawVoices(data map[string]any, lang string) []any {
	if lang != "" {
		if v, ok := data["voices"].([]any); ok {
			return v
		}
	}
	if byLang, ok := data["byLang"].(map[string]any); ok {
		var out []any
		for _, l := range byLang {
			if lm, ok := l.(map[string]any); ok {
				if vs, ok := lm["voices"].([]any); ok {
					out = append(out, vs...)
				}
			}
		}
		return out
	}
	if v, ok := data["voices"].([]any); ok {
		return v
	}
	return nil
}

func ttsVoiceProviderKeys() []string {
	keys := make([]string, 0, len(ttsVoiceProviderAPI))
	for k := range ttsVoiceProviderAPI {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}