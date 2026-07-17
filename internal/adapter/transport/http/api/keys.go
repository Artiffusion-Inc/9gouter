package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
)

// RegisterKeys mounts API-key management routes.
func RegisterKeys(mux *http.ServeMux, deps Deps) {
	h := &keysHandler{deps: deps}
	mux.HandleFunc("GET /api/keys", h.list)
	mux.HandleFunc("POST /api/keys", h.create)
	mux.HandleFunc("GET /api/keys/{id}", h.get)
	mux.HandleFunc("PUT /api/keys/{id}", h.update)
	mux.HandleFunc("DELETE /api/keys/{id}", h.delete)
}

type keysHandler struct {
	deps Deps
}

func (h *keysHandler) list(w http.ResponseWriter, r *http.Request) {
	keys, err := h.deps.APIKeys.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch keys")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (h *keysHandler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key, err := h.deps.APIKeys.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch key")
		return
	}
	if key == nil {
		writeError(w, http.StatusNotFound, "Key not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key})
}

type createKeyRequest struct {
	Name string `json:"name"`
}

func (h *keysHandler) create(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if err := parseJSON(r, &req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "Name is required")
		return
	}

	id := generateID()
	key := settings.APIKey{
		ID:        id,
		Key:       "9r-" + id,
		Name:      req.Name,
		MachineID: "server",
		IsActive:  true,
	}
	if err := h.deps.APIKeys.Create(r.Context(), key); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create key")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":        key.ID,
		"key":       key.Key,
		"name":      key.Name,
		"machineId": key.MachineID,
	})
}

type updateKeyRequest struct {
	IsActive *bool `json:"isActive,omitempty"`
}

func (h *keysHandler) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := h.deps.APIKeys.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch key")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "Key not found")
		return
	}

	var req updateKeyRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.IsActive != nil {
		if err := h.deps.APIKeys.SetActive(r.Context(), id, *req.IsActive); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to update key")
			return
		}
	}

	updated, err := h.deps.APIKeys.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": updated})
}

func (h *keysHandler) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := h.deps.APIKeys.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch key")
		return
	}
	if err := h.deps.APIKeys.Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "Key deleted successfully"})
}

var _ = context.Background
var _ = json.Marshal
