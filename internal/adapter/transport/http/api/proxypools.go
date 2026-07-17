package api

import (
	"encoding/json"
	"net/http"

	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
	"github.com/Artiffusion-Inc/9router/internal/usecase/managedashboard"
)

// RegisterProxyPools mounts proxy pool management routes.
func RegisterProxyPools(mux *http.ServeMux, deps Deps) {
	h := &proxyPoolsHandler{
		deps: deps,
		svc:  &managedashboard.ProxyPoolService{Repo: deps.ProxyPools, ConnectionRepo: deps.Connections},
	}
	mux.HandleFunc("GET /api/proxy-pools", h.list)
	mux.HandleFunc("POST /api/proxy-pools", h.create)
	mux.HandleFunc("GET /api/proxy-pools/{id}", h.get)
	mux.HandleFunc("PUT /api/proxy-pools/{id}", h.update)
	mux.HandleFunc("DELETE /api/proxy-pools/{id}", h.delete)
}

type proxyPoolsHandler struct {
	deps Deps
	svc  *managedashboard.ProxyPoolService
}

func (h *proxyPoolsHandler) list(w http.ResponseWriter, r *http.Request) {
	isActive := queryOptionalBool(r, "isActive")
	includeUsage := r.URL.Query().Get("includeUsage") == "true"
	pools, err := h.svc.List(r.Context(), isActive, includeUsage)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch proxy pools")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxyPools": pools})
}

func (h *proxyPoolsHandler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pool, err := h.svc.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch proxy pool")
		return
	}
	if pool == nil {
		writeError(w, http.StatusNotFound, "Proxy pool not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxyPool": pool})
}

type proxyPoolInput struct {
	Name        string `json:"name"`
	ProxyURL    string `json:"proxyUrl"`
	NoProxy     string `json:"noProxy"`
	IsActive    *bool  `json:"isActive,omitempty"`
	StrictProxy bool   `json:"strictProxy"`
	Type        string `json:"type"`
}

func (h *proxyPoolsHandler) create(w http.ResponseWriter, r *http.Request) {
	var req proxyPoolInput
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.Name = stringsTrim(req.Name); req.Name == "" {
		writeError(w, http.StatusBadRequest, "Name is required")
		return
	}
	if req.ProxyURL = stringsTrim(req.ProxyURL); req.ProxyURL == "" {
		writeError(w, http.StatusBadRequest, "Proxy URL is required")
		return
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	if req.Type == "" {
		req.Type = "http"
	}
	id := generateID()
	data, _ := json.Marshal(map[string]any{
		"name":        req.Name,
		"proxyUrl":    req.ProxyURL,
		"noProxy":     req.NoProxy,
		"strictProxy": req.StrictProxy,
		"type":        req.Type,
	})
	pool := settings.ProxyPool{
		ID:         id,
		IsActive:   isActive,
		TestStatus: "unknown",
		Data:       data,
	}
	created, err := h.svc.Create(r.Context(), pool)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create proxy pool")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"proxyPool": created})
}

func (h *proxyPoolsHandler) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	prev, err := h.svc.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch proxy pool")
		return
	}
	if prev == nil {
		writeError(w, http.StatusNotFound, "Proxy pool not found")
		return
	}

	var req proxyPoolInput
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	var dataMap map[string]any
	_ = json.Unmarshal(prev.Data, &dataMap)
	if dataMap == nil {
		dataMap = map[string]any{}
	}
	if hasField(r, "name") {
		if req.Name = stringsTrim(req.Name); req.Name == "" {
			writeError(w, http.StatusBadRequest, "Name is required")
			return
		}
		dataMap["name"] = req.Name
	}
	if hasField(r, "proxyUrl") {
		if req.ProxyURL = stringsTrim(req.ProxyURL); req.ProxyURL == "" {
			writeError(w, http.StatusBadRequest, "Proxy URL is required")
			return
		}
		dataMap["proxyUrl"] = req.ProxyURL
	}
	if hasField(r, "noProxy") {
		dataMap["noProxy"] = stringsTrim(req.NoProxy)
	}
	if hasField(r, "isActive") && req.IsActive != nil {
		prev.IsActive = *req.IsActive
	}
	if hasField(r, "strictProxy") {
		dataMap["strictProxy"] = req.StrictProxy
	}
	if hasField(r, "type") {
		if !isValidProxyType(req.Type) {
			req.Type = "http"
		}
		dataMap["type"] = req.Type
	}
	data, _ := json.Marshal(dataMap)
	prev.Data = data

	updated, err := h.svc.Update(r.Context(), *prev)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update proxy pool")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxyPool": updated})
}

func (h *proxyPoolsHandler) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.svc.Delete(r.Context(), id); err != nil {
		if err == managedashboard.ErrProxyPoolInUse {
			writeError(w, http.StatusConflict, "Proxy pool is currently in use")
			return
		}
		if err == managedashboard.ErrProxyPoolNotFound {
			writeError(w, http.StatusNotFound, "Proxy pool not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to delete proxy pool")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func isValidProxyType(t string) bool {
	switch t {
	case "http", "vercel", "cloudflare", "deno":
		return true
	}
	return false
}
