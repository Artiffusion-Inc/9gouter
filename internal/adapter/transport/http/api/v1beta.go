package api

import "net/http"

// RegisterV1Beta mounts the Gemini-compatible v1beta models endpoint.
func RegisterV1Beta(mux *http.ServeMux, deps Deps) {
	h := &v1betaHandler{deps: deps}
	mux.HandleFunc("GET /api/v1beta/models", h.list)
	mux.HandleFunc("GET /api/v1beta/models/{path...}", h.list)
}

type v1betaHandler struct {
	deps Deps
}

func (h *v1betaHandler) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": []any{}})
}
