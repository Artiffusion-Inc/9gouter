package api

import "net/http"

// RegisterTunnel mounts tunnel and Tailscale management routes.
func RegisterTunnel(mux *http.ServeMux, deps Deps) {
	h := &tunnelHandler{deps: deps}
	mux.HandleFunc("GET /api/tunnel/status", h.status)
	mux.HandleFunc("POST /api/tunnel/enable", h.enable)
	mux.HandleFunc("POST /api/tunnel/disable", h.disable)
	mux.HandleFunc("GET /api/tunnel/tailscale-check", h.tailscaleCheck)
	mux.HandleFunc("POST /api/tunnel/tailscale-enable", h.tailscaleEnable)
	mux.HandleFunc("POST /api/tunnel/tailscale-disable", h.tailscaleDisable)
	mux.HandleFunc("POST /api/tunnel/tailscale-install", h.tailscaleInstall)
}

type tunnelHandler struct {
	deps Deps
}

func (h *tunnelHandler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"tunnel":     map[string]any{"enabled": false},
		"tailscale":  map[string]any{"enabled": false},
		"download":   map[string]any{},
	})
}

func (h *tunnelHandler) enable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "enabled": false})
}

func (h *tunnelHandler) disable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "enabled": false})
}

func (h *tunnelHandler) tailscaleCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"installed": false, "running": false})
}

func (h *tunnelHandler) tailscaleEnable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "enabled": false})
}

func (h *tunnelHandler) tailscaleDisable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "enabled": false})
}

func (h *tunnelHandler) tailscaleInstall(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "installed": false})
}
