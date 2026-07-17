package api

import (
	"net/http"
	"strings"
)

// RegisterProvidersExtra mounts provider sub-routes that were missing from the
// initial providers handler.
func RegisterProvidersExtra(mux *http.ServeMux, deps Deps) {
	h := &providersExtraHandler{deps: deps}
	mux.HandleFunc("GET /api/providers/{id}/models", h.models)
	mux.HandleFunc("GET /api/providers/{id}/test-models", h.testModels)
	mux.HandleFunc("POST /api/providers/{id}/test", h.test)
	mux.HandleFunc("GET /api/providers/{id}", h.get)
	mux.HandleFunc("PUT /api/providers/{id}", h.update)
	mux.HandleFunc("DELETE /api/providers/{id}", h.delete)
	mux.HandleFunc("GET /api/providers/suggested-models", h.suggestedModels)
	mux.HandleFunc("GET /api/providers/kilo/free-models", h.kiloFreeModels)
	mux.HandleFunc("POST /api/providers/test-batch", h.testBatch)
	mux.HandleFunc("POST /api/providers/validate", h.validate)
}

type providersExtraHandler struct {
	deps Deps
}

func (h *providersExtraHandler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": []any{}})
}

func (h *providersExtraHandler) testModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": []any{}})
}

func (h *providersExtraHandler) test(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "testStatus": "unknown"})
}

func (h *providersExtraHandler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := h.deps.Connections.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch connection")
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound, "Connection not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": c})
}

func (h *providersExtraHandler) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id})
}

func (h *providersExtraHandler) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.deps.Connections.Delete(r.Context(), id); err != nil {
		if !strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusInternalServerError, "Failed to delete connection")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *providersExtraHandler) suggestedModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": []any{}})
}

func (h *providersExtraHandler) kiloFreeModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": []any{}})
}

func (h *providersExtraHandler) testBatch(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"results": []any{}})
}

func (h *providersExtraHandler) validate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
}
