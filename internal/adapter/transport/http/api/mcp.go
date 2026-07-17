package api

import (
	"net/http"
	"time"
)

// RegisterMcp mounts MCP plugin message/SSE routes.
func RegisterMcp(mux *http.ServeMux, deps Deps) {
	h := &mcpHandler{deps: deps}
	mux.HandleFunc("POST /api/mcp/{plugin}/message", h.message)
	mux.HandleFunc("GET /api/mcp/{plugin}/sse", h.sse)
}

type mcpHandler struct {
	deps Deps
}

func (h *mcpHandler) message(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusAccepted, map[string]any{"success": true})
}

func (h *mcpHandler) sse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	plugin := r.PathValue("plugin")
	_, _ = w.Write([]byte("event: endpoint\ndata: /api/mcp/" + plugin + "/message?sessionId=0\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	time.Sleep(100 * time.Millisecond)
}
