package api

import (
	"net/http"
	"os"
	"time"
)

// RegisterShutdown mounts the shutdown route used in development to restart the server.
func RegisterShutdown(mux *http.ServeMux, deps Deps) {
	h := &shutdownHandler{deps: deps}
	mux.HandleFunc("POST /api/shutdown", h.shutdown)
}

type shutdownHandler struct {
	deps Deps
}

func (h *shutdownHandler) shutdown(w http.ResponseWriter, r *http.Request) {
	// No production guard; this is intentionally a development-only route.
	secret := os.Getenv("SHUTDOWN_SECRET")
	auth := r.Header.Get("Authorization")
	if secret != "" && auth != "Bearer "+secret {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "message": "Unauthorized"})
		return
	}

	go func() {
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Shutting down..."})
}
