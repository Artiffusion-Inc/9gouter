package api

import "net/http"

// RegisterV1Dashboard mounts the dashboard-side /api/v1 proxy pass-through routes.
// Note: the real /v1 chat pipeline is served by httptransport.RegisterV1; these
// dashboard routes simply expose the same endpoints for the static frontend to
// call through /api/v1/... so it is covered by the same handlers.
func RegisterV1Dashboard(mux *http.ServeMux, deps Deps) {
	h := &v1DashboardHandler{deps: deps}
	mux.HandleFunc("GET /api/v1", h.root)
	mux.HandleFunc("POST /api/v1/api/chat", h.proxy)
	mux.HandleFunc("POST /api/v1/chat/completions", h.proxy)
	mux.HandleFunc("POST /api/v1/messages", h.proxy)
	mux.HandleFunc("POST /api/v1/messages/count_tokens", h.proxy)
	mux.HandleFunc("POST /api/v1/responses", h.proxy)
	mux.HandleFunc("POST /api/v1/responses/compact", h.proxy)
	mux.HandleFunc("GET /api/v1/models", h.proxy)
	mux.HandleFunc("GET /api/v1/models/info", h.proxy)
	mux.HandleFunc("GET /api/v1/models/{kind}", h.proxy)
	mux.HandleFunc("POST /api/v1/audio/speech", h.proxy)
	mux.HandleFunc("POST /api/v1/audio/transcriptions", h.proxy)
	mux.HandleFunc("GET /api/v1/audio/voices", h.proxy)
	mux.HandleFunc("POST /api/v1/embeddings", h.proxy)
	mux.HandleFunc("POST /api/v1/images/generations", h.proxy)
	mux.HandleFunc("POST /api/v1/search", h.proxy)
	mux.HandleFunc("POST /api/v1/videos/generations", h.proxy)
	mux.HandleFunc("POST /api/v1/videos/edits", h.proxy)
	mux.HandleFunc("POST /api/v1/videos/extensions", h.proxy)
	mux.HandleFunc("GET /api/v1/videos/{id}", h.proxy)
	mux.HandleFunc("POST /api/v1/web/fetch", h.proxy)
}

type v1DashboardHandler struct {
	deps Deps
}

func (h *v1DashboardHandler) root(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": "v1"})
}

func (h *v1DashboardHandler) proxy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success": false,
		"message": "Dashboard /api/v1 proxy passthrough not available in Go build; use /v1 directly",
	})
}
