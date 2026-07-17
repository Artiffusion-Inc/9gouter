package api

import (
	"net/http"
	"strings"
)

// RegisterHeadroom mounts headroom proxy management routes.
func RegisterHeadroom(mux *http.ServeMux, deps Deps) {
	h := &headroomHandler{deps: deps}
	mux.HandleFunc("GET /api/headroom/status", h.status)
	mux.HandleFunc("GET /api/headroom/extras", h.extras)
	mux.HandleFunc("POST /api/headroom/start", h.start)
	mux.HandleFunc("POST /api/headroom/stop", h.stop)
	mux.HandleFunc("POST /api/headroom/restart", h.restart)
	mux.HandleFunc("GET /api/headroom/proxy/{path...}", h.proxy)
	mux.HandleFunc("POST /api/headroom/proxy/{path...}", h.proxy)
	mux.HandleFunc("PUT /api/headroom/proxy/{path...}", h.proxy)
	mux.HandleFunc("DELETE /api/headroom/proxy/{path...}", h.proxy)
	mux.HandleFunc("PATCH /api/headroom/proxy/{path...}", h.proxy)

	// The JS route was headroom/proxy/[...path]; the Go equivalent is
	// "{path...}" covering any depth. No additional registration needed.
}

type headroomHandler struct {
	deps Deps
}

func (h *headroomHandler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"running":      false,
		"ready":        false,
		"url":          "",
		"pid":          nil,
		"download":     map[string]any{},
	})
}

func (h *headroomHandler) extras(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"extras": []any{}})
}

func (h *headroomHandler) start(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "running": false, "code": "NOT_INSTALLED"})
}

func (h *headroomHandler) stop(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "running": false})
}

func (h *headroomHandler) restart(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "running": false, "code": "NOT_INSTALLED"})
}

func (h *headroomHandler) proxy(w http.ResponseWriter, r *http.Request) {
	path := r.PathValue("path")
	writeJSON(w, http.StatusOK, map[string]any{
		"success": false,
		"message": "Headroom proxy passthrough not available in Go build",
		"path":    strings.TrimPrefix(path, "/"),
	})
}
