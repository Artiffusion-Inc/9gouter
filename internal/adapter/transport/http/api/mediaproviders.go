package api

import (
	"net/http"
	"strings"
)

// RegisterMediaProviders mounts media provider routes.
func RegisterMediaProviders(mux *http.ServeMux, deps Deps) {
	h := &mediaHandler{deps: deps}
	mux.HandleFunc("GET /api/media-providers/tts/voices", h.voices)
	mux.HandleFunc("GET /api/media-providers/tts/deepgram/voices", h.deepgramVoices)
	mux.HandleFunc("GET /api/media-providers/tts/elevenlabs/voices", h.elevenlabsVoices)
	mux.HandleFunc("GET /api/media-providers/tts/inworld/voices", h.inworldVoices)
	mux.HandleFunc("GET /api/media-providers/tts/minimax/voices", h.minimaxVoices)
}

type mediaHandler struct {
	deps Deps
}

func (h *mediaHandler) voices(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	if provider == "" {
		provider = "edge-tts"
	}
	lang := r.URL.Query().Get("lang")

	raw := edgeTTSVoices()
	voices := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		parts := strings.SplitN(v.Locale, "-", 2)
		langCode := parts[0]
		country := ""
		if len(parts) > 1 {
			country = parts[1]
		}
		voice := map[string]any{
			"id":      v.ShortName,
			"name":    strings.ReplaceAll(strings.ReplaceAll(v.FriendlyName, "Microsoft ", ""), " Online (Natural) - ", " ("),
			"locale":  v.Locale,
			"lang":    langCode,
			"country": country,
			"gender":  v.Gender,
		}
		voices = append(voices, voice)
	}
	if lang != "" {
		filtered := make([]map[string]any, 0, len(voices))
		for _, v := range voices {
			if v["lang"] == lang {
				filtered = append(filtered, v)
			}
		}
		voices = filtered
	}
	byLang := map[string]map[string]any{}
	for _, v := range voices {
		key := v["lang"].(string)
		if _, ok := byLang[key]; !ok {
			byLang[key] = map[string]any{"code": key, "name": v["lang"], "voices": []any{}}
		}
		byLang[key]["voices"] = append(byLang[key]["voices"].([]any), v)
	}
	languages := make([]map[string]any, 0, len(byLang))
	for _, l := range byLang {
		languages = append(languages, l)
	}

	if provider == "elevenlabs" || provider == "local-device" {
		voices = []map[string]any{}
		languages = []map[string]any{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"voices":    voices,
		"languages": languages,
		"byLang":    byLang,
	})
}

func (h *mediaHandler) deepgramVoices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"voices": []any{}})
}

func (h *mediaHandler) elevenlabsVoices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"voices": []any{}})
}

func (h *mediaHandler) inworldVoices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"voices": []any{}})
}

func (h *mediaHandler) minimaxVoices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"voices": []any{}})
}

type edgeVoice struct {
	Locale       string
	ShortName    string
	FriendlyName string
	Gender       string
}

func edgeTTSVoices() []edgeVoice {
	return []edgeVoice{
		{Locale: "en-US", ShortName: "en-US-AriaNeural", FriendlyName: "Microsoft Aria Online (Natural) - English (United States)", Gender: "Female"},
		{Locale: "en-GB", ShortName: "en-GB-SoniaNeural", FriendlyName: "Microsoft Sonia Online (Natural) - English (United Kingdom)", Gender: "Female"},
		{Locale: "zh-CN", ShortName: "zh-CN-XiaoxiaoNeural", FriendlyName: "Microsoft Xiaoxiao Online (Natural) - Chinese (Mainland)", Gender: "Female"},
	}
}
