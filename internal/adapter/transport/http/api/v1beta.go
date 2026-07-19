package api

import (
	"net/http"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider"
)

// RegisterV1Beta mounts the Gemini-compatible v1beta models endpoint.
//
// Mirrors legacy JS src/app/api/v1beta/models/route.js: returns the static
// provider catalog in the Gemini API shape ({"models":[{name,displayName,
// description,supportedGenerationMethods,inputTokenLimit,outputTokenLimit}]}).
// The JS route iterated PROVIDER_MODELS and emitted `models/<provider>/<id>`
// for every provider, plus a bare `models/<id>` (with the stream-capable
// methods list) for gemini. No auth gate — the JS route had none.
func RegisterV1Beta(mux *http.ServeMux, deps Deps) {
	h := &v1betaHandler{deps: deps}
	mux.HandleFunc("GET /api/v1beta/models", h.list)
	mux.HandleFunc("GET /api/v1beta/models/{path...}", h.list)
}

type v1betaHandler struct {
	deps Deps
}

func (h *v1betaHandler) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"models": buildGeminiModelsList(),
	})
}

// geminiModel is the Gemini /v1beta/models object entry.
type geminiModel struct {
	Name             string   `json:"name"`
	DisplayName      string   `json:"displayName"`
	Description      string   `json:"description"`
	SupportedMethods []string `json:"supportedGenerationMethods"`
	InputTokenLimit  int      `json:"inputTokenLimit"`
	OutputTokenLimit int      `json:"outputTokenLimit"`
}

// buildGeminiModelsList assembles the Gemini-shaped catalog from the static
// provider catalogs. Providers without a static catalog (live-resolver
// providers — not yet ported) are absent, matching the JS PROVIDER_MODELS
// source. The Gemini double-entry is preserved: gemini models get both
// `models/gemini/<id>` and `models/<id>` with stream-capable methods.
func buildGeminiModelsList() []geminiModel {
	const (
		inputLimit  = 128000
		outputLimit = 8192
	)
	chatMethods := []string{"generateContent"}
	streamMethods := []string{"generateContent", "streamGenerateContent"}

	out := []geminiModel{}
	seen := map[string]bool{}
	add := func(name, displayName, description string, methods []string) {
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, geminiModel{
			Name:             name,
			DisplayName:      displayName,
			Description:      description,
			SupportedMethods: methods,
			InputTokenLimit:  inputLimit,
			OutputTokenLimit: outputLimit,
		})
	}

	for _, cat := range provider.AllCatalogs() {
		provName := cat.Alias
		if provName == "" {
			provName = cat.ID
		}
		isGemini := cat.ID == "gemini"
		for _, m := range cat.Models {
			display := m.Name
			if display == "" {
				display = m.ID
			}
			add(
				"models/"+provName+"/"+m.ID,
				display,
				provName+" model: "+display,
				chatMethods,
			)
			if isGemini {
				add(
					"models/"+m.ID,
					display,
					"Gemini model: "+display,
					streamMethods,
				)
			}
		}
	}
	return out
}