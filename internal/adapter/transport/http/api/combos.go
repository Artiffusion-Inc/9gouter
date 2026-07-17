package api

import (
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
	"github.com/Artiffusion-Inc/9router/internal/usecase/managedashboard"
)

var comboNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_.\-]+$`)

// RegisterCombos mounts combo management routes.
func RegisterCombos(mux *http.ServeMux, deps Deps) {
	h := &combosHandler{
		deps: deps,
		svc:  &managedashboard.ComboService{Repo: deps.Combos},
	}
	mux.HandleFunc("GET /api/combos", h.list)
	mux.HandleFunc("POST /api/combos", h.create)
	mux.HandleFunc("GET /api/combos/{id}", h.get)
	mux.HandleFunc("PUT /api/combos/{id}", h.update)
	mux.HandleFunc("DELETE /api/combos/{id}", h.delete)
}

type combosHandler struct {
	deps Deps
	svc  *managedashboard.ComboService
}

func (h *combosHandler) list(w http.ResponseWriter, r *http.Request) {
	combos, err := h.svc.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch combos")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"combos": combos})
}

func (h *combosHandler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	combo, err := h.svc.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch combo")
		return
	}
	if combo == nil {
		writeError(w, http.StatusNotFound, "Combo not found")
		return
	}
	writeJSON(w, http.StatusOK, combo)
}

type comboRequest struct {
	Name   string          `json:"name"`
	Models json.RawMessage `json:"models"`
	Kind   string          `json:"kind"`
}

func (h *combosHandler) create(w http.ResponseWriter, r *http.Request) {
	var req comboRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "Name is required")
		return
	}
	if !comboNameRegex.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "Name can only contain letters, numbers, -, _ and .")
		return
	}
	existing, err := h.svc.GetByName(r.Context(), req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to check combo name")
		return
	}
	if existing != nil {
		writeError(w, http.StatusBadRequest, "Combo name already exists")
		return
	}
	if len(req.Models) == 0 {
		req.Models = []byte("[]")
	}
	id := generateID()
	combo := settings.Combo{
		ID:     id,
		Name:   req.Name,
		Kind:   req.Kind,
		Models: req.Models,
	}
	created, err := h.svc.Create(r.Context(), combo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create combo")
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *combosHandler) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	prev, err := h.svc.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch combo")
		return
	}
	if prev == nil {
		writeError(w, http.StatusNotFound, "Combo not found")
		return
	}

	var req comboRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	combo := *prev
	if req.Name != "" {
		if !comboNameRegex.MatchString(req.Name) {
			writeError(w, http.StatusBadRequest, "Name can only contain letters, numbers, -, _ and .")
			return
		}
		existing, err := h.svc.GetByName(r.Context(), req.Name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to check combo name")
			return
		}
		if existing != nil && existing.ID != id {
			writeError(w, http.StatusBadRequest, "Combo name already exists")
			return
		}
		combo.Name = req.Name
	}
	if req.Models != nil {
		combo.Models = req.Models
	}
	if req.Kind != "" {
		combo.Kind = req.Kind
	}

	updated, err := h.svc.Update(r.Context(), combo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update combo")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *combosHandler) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := h.svc.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch combo")
		return
	}
	if err := h.svc.Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete combo")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}
