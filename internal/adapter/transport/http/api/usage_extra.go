package api

import (
	"net/http"
	"strings"
)

// RegisterUsageExtra mounts additional usage routes not covered by the initial
// usage handler.
func RegisterUsageExtra(mux *http.ServeMux, deps Deps) {
	h := &usageExtraHandler{deps: deps}
	mux.HandleFunc("GET /api/usage/stream", h.stream)
	mux.HandleFunc("GET /api/usage/request-logs", h.requestLogs)
	mux.HandleFunc("GET /api/usage/{connectionId}", h.byConnection)
	mux.HandleFunc("POST /api/usage/{connectionId}/codex-reset-credits", h.codexResetCredits)
}

type usageExtraHandler struct {
	deps Deps
}

func (h *usageExtraHandler) stream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("event: usage\ndata: {}\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *usageExtraHandler) requestLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"logs": []any{}})
}

func (h *usageExtraHandler) byConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("connectionId")
	writeJSON(w, http.StatusOK, map[string]any{"connectionId": id, "usage": []any{}})
}

func (h *usageExtraHandler) codexResetCredits(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("connectionId")
	writeJSON(w, http.StatusOK, map[string]any{
		"code":          "no_credit",
		"reset":         false,
		"windows_reset": 0,
		"message":       "No Codex reset credits available.",
		"connectionId":  strings.TrimPrefix(id, "/"),
	})
}
