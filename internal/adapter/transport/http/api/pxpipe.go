package api

import "net/http"

// RegisterPxPipe mounts pxpipe management routes.
func RegisterPxPipe(mux *http.ServeMux, deps Deps) {
	h := &pxpipeHandler{deps: deps}
	mux.HandleFunc("GET /api/pxpipe/health", h.health)
	mux.HandleFunc("POST /api/pxpipe/health", h.health)
	mux.HandleFunc("GET /api/pxpipe/status", h.status)
	mux.HandleFunc("POST /api/pxpipe/start", h.start)
	mux.HandleFunc("POST /api/pxpipe/stop", h.stop)
	mux.HandleFunc("POST /api/pxpipe/restart", h.restart)
	mux.HandleFunc("GET /api/pxpipe/stats", h.stats)
	mux.HandleFunc("GET /api/pxpipe/logs", h.logs)
	mux.HandleFunc("POST /api/pxpipe/install", h.install)
}

type pxpipeHandler struct {
	deps Deps
}

func (h *pxpipeHandler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"healthy": true, "checks": []any{}})
}

func (h *pxpipeHandler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"running": false, "installed": false})
}

func (h *pxpipeHandler) start(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "running": false})
}

func (h *pxpipeHandler) stop(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "running": false})
}

func (h *pxpipeHandler) restart(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "running": false})
}

// stats returns pxpipe compression stats. The dashboard reads
// `stats.windows[id]`, `stats.timeline[]`, and `stats.recent[]` directly,
// so the response must NOT wrap the payload in an extra object.
// When no data is available we return the empty default shape so the
// frontend's optional-chaining renders placeholders instead of crashing.
func (h *pxpipeHandler) stats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"windows":  map[string]any{},
		"timeline": []any{},
		"recent":   []any{},
	})
}

// logs returns the pxpipe install log text. The dashboard reads
// `logs.installLog` (not `logs.logs`) and renders the placeholder when empty.
func (h *pxpipeHandler) logs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"installLog": ""})
}

func (h *pxpipeHandler) install(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "PxPipe install stubbed in Go build"})
}
