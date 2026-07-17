package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
	"github.com/Artiffusion-Inc/9router/internal/usecase/managedashboard"
)

// RegisterProviderNodes mounts provider node CRUD and validation routes.
func RegisterProviderNodes(mux *http.ServeMux, deps Deps) {
	h := &nodesHandler{
		deps: deps,
		svc: &managedashboard.NodeService{
			Repo:     deps.Nodes,
			ConnRepo: deps.Connections,
		},
	}
	mux.HandleFunc("GET /api/provider-nodes", h.list)
	mux.HandleFunc("POST /api/provider-nodes", h.create)
	mux.HandleFunc("PUT /api/provider-nodes/{id}", h.update)
	mux.HandleFunc("DELETE /api/provider-nodes/{id}", h.delete)
	mux.HandleFunc("POST /api/provider-nodes/validate", h.validate)
}

type nodesHandler struct {
	deps Deps
	svc  *managedashboard.NodeService
}

func (h *nodesHandler) list(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.svc.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch provider nodes")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

func (h *nodesHandler) create(w http.ResponseWriter, r *http.Request) {
	var req managedashboard.NodeCreateRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	node, err := h.svc.Create(r.Context(), req)
	if err != nil {
		if isValidationErr(err) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to create provider node")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"node": nodeToMap(*node)})
}

func (h *nodesHandler) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req managedashboard.NodeUpdateRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	node, err := h.svc.Update(r.Context(), id, req)
	if err != nil {
		if err.Error() == "provider node not found" {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if isValidationErr(err) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to update provider node")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": nodeToMap(*node)})
}

func (h *nodesHandler) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.svc.Delete(r.Context(), id); err != nil {
		if err.Error() == "provider node not found" {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to delete provider node")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *nodesHandler) validate(w http.ResponseWriter, r *http.Request) {
	var req managedashboard.ValidateRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	result, err := h.svc.Validate(r.Context(), req, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to validate provider node")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func isValidationErr(err error) bool {
	if err == nil {
		return false
	}
	switch err.Error() {
	case "name is required", "prefix is required", "invalid OpenAI compatible API type",
		"invalid provider node type", "base URL is required", "Base URL and API key required":
		return true
	}
	return false
}

func nodeToMap(n settings.ProviderNode) map[string]any {
	m := map[string]any{
		"id":        n.ID,
		"type":      n.Type,
		"name":      n.Name,
		"data":      n.Data,
		"createdAt": n.CreatedAt,
		"updatedAt": n.UpdatedAt,
	}
	var data map[string]any
	_ = json.Unmarshal(n.Data, &data)
	for _, k := range []string{"prefix", "apiType", "baseUrl"} {
		if v, ok := data[k]; ok {
			m[k] = v
		}
	}
	return m
}

var _ = fmt.Sprintf
