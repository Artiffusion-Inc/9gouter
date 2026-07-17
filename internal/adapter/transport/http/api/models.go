package api

import (
	"encoding/json"
	"net/http"

	"github.com/Artiffusion-Inc/9router/internal/usecase/managedashboard"
)

// RegisterModels mounts model alias / custom / disabled / list routes.
func RegisterModels(mux *http.ServeMux, deps Deps) {
	h := &modelsHandler{
		deps: deps,
		svc:  &managedashboard.ModelService{AliasRepo: deps.Alias, DisabledRepo: deps.DisabledModels},
	}
	mux.HandleFunc("GET /api/models", h.list)
	mux.HandleFunc("PUT /api/models", h.setAlias)

	mux.HandleFunc("GET /api/models/alias", h.listAliases)
	mux.HandleFunc("PUT /api/models/alias", h.setAlias)
	mux.HandleFunc("DELETE /api/models/alias", h.deleteAlias)

	mux.HandleFunc("GET /api/models/custom", h.listCustom)
	mux.HandleFunc("POST /api/models/custom", h.addCustom)
	mux.HandleFunc("DELETE /api/models/custom", h.deleteCustom)

	mux.HandleFunc("GET /api/models/disabled", h.listDisabled)
	mux.HandleFunc("POST /api/models/disabled", h.disable)
	mux.HandleFunc("DELETE /api/models/disabled", h.enable)
}

type modelsHandler struct {
	deps Deps
	svc  *managedashboard.ModelService
}

func (h *modelsHandler) list(w http.ResponseWriter, r *http.Request) {
	aliases, err := h.svc.Aliases(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch aliases")
		return
	}
	disabled, err := h.svc.Disabled(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch disabled models")
		return
	}
	// Static model list is not ported; return empty list with aliases/disabled.
	writeJSON(w, http.StatusOK, map[string]any{
		"models":   []any{},
		"aliases":  aliases,
		"disabled": disabled,
	})
}

func (h *modelsHandler) listAliases(w http.ResponseWriter, r *http.Request) {
	aliases, err := h.svc.Aliases(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch aliases")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"aliases": aliases})
}

type aliasRequest struct {
	Model string `json:"model"`
	Alias string `json:"alias"`
}

func (h *modelsHandler) setAlias(w http.ResponseWriter, r *http.Request) {
	var req aliasRequest
	if err := parseJSON(r, &req); err != nil || req.Model == "" || req.Alias == "" {
		writeError(w, http.StatusBadRequest, "Model and alias required")
		return
	}
	aliases, err := h.svc.Aliases(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch aliases")
		return
	}
	for existingModel, existingAlias := range aliases {
		if existingAlias == req.Alias && existingModel != req.Model {
			writeError(w, http.StatusBadRequest, "Alias already in use")
			return
		}
	}
	if err := h.svc.SetAlias(r.Context(), req.Model, req.Alias); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update alias")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "model": req.Model, "alias": req.Alias})
}

func (h *modelsHandler) deleteAlias(w http.ResponseWriter, r *http.Request) {
	alias := r.URL.Query().Get("alias")
	if alias == "" {
		writeError(w, http.StatusBadRequest, "Alias required")
		return
	}
	if err := h.svc.DeleteAlias(r.Context(), alias); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete alias")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *modelsHandler) listCustom(w http.ResponseWriter, r *http.Request) {
	models, err := h.svc.CustomModels(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch custom models")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

type customModelRequest struct {
	ProviderAlias string `json:"providerAlias"`
	ID            string `json:"id"`
	Type          string `json:"type"`
	Name          string `json:"name"`
}

func (h *modelsHandler) addCustom(w http.ResponseWriter, r *http.Request) {
	var req customModelRequest
	if err := parseJSON(r, &req); err != nil || req.ProviderAlias == "" || req.ID == "" {
		writeError(w, http.StatusBadRequest, "providerAlias and id required")
		return
	}
	added, err := h.svc.AddCustom(r.Context(), req.ProviderAlias, req.ID, req.Type, req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to add custom model")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "added": added})
}

func (h *modelsHandler) deleteCustom(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	providerAlias := q.Get("providerAlias")
	id := q.Get("id")
	typ := q.Get("type")
	if providerAlias == "" || id == "" {
		writeError(w, http.StatusBadRequest, "providerAlias and id required")
		return
	}
	if err := h.svc.DeleteCustom(r.Context(), providerAlias, id, typ); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete custom model")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *modelsHandler) listDisabled(w http.ResponseWriter, r *http.Request) {
	providerAlias := r.URL.Query().Get("providerAlias")
	if providerAlias != "" {
		ids, err := h.svc.DisabledByProvider(r.Context(), providerAlias)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to fetch disabled models")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ids": ids})
		return
	}
	all, err := h.svc.Disabled(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch disabled models")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disabled": all})
}

type disableRequest struct {
	ProviderAlias string   `json:"providerAlias"`
	IDs           []string `json:"ids"`
}

func (h *modelsHandler) disable(w http.ResponseWriter, r *http.Request) {
	var req disableRequest
	if err := parseJSON(r, &req); err != nil || req.ProviderAlias == "" || len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "providerAlias and ids[] required")
		return
	}
	if err := h.svc.Disable(r.Context(), req.ProviderAlias, req.IDs); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to disable models")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *modelsHandler) enable(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	providerAlias := q.Get("providerAlias")
	id := q.Get("id")
	if providerAlias == "" {
		writeError(w, http.StatusBadRequest, "providerAlias required")
		return
	}
	var ids []string
	if id != "" {
		ids = []string{id}
	}
	if err := h.svc.Enable(r.Context(), providerAlias, ids); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to enable models")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

var _ = json.Marshal
