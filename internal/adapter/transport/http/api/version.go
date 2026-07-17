package api

import (
	"net/http"
)

// versionResponse matches the JS /api/version shape.
type versionResponse struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
}

// RegisterVersion mounts the public version route.
func RegisterVersion(mux *http.ServeMux, version string) {
	if version == "" {
		version = "dev"
	}
	h := &versionHandler{version: version}
	mux.HandleFunc("GET /api/version", h.get)
	mux.HandleFunc("POST /api/version/update", h.update)
	mux.HandleFunc("POST /api/version/shutdown", h.shutdownHandler)
}

type versionHandler struct {
	version string
}

func (h *versionHandler) get(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"currentVersion": h.version,
		"latestVersion":  nil,
		"hasUpdate":      false,
		"version":        h.version,
	})
}

func (h *versionHandler) update(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success": false,
		"message": "Update is only available in production build (9router CLI)",
	})
}

func (h *versionHandler) shutdownHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Shutting down for manual update...",
	})
}
