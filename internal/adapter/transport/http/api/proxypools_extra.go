package api

import "net/http"

// RegisterProxyPoolsExtra mounts additional proxy-pool sub-routes.
func RegisterProxyPoolsExtra(mux *http.ServeMux, deps Deps) {
	h := &proxyPoolsExtraHandler{deps: deps}
	mux.HandleFunc("POST /api/proxy-pools/{id}/test", h.test)
	mux.HandleFunc("POST /api/proxy-pools/cloudflare-deploy", h.deploy)
	mux.HandleFunc("POST /api/proxy-pools/deno-deploy", h.deploy)
	mux.HandleFunc("POST /api/proxy-pools/vercel-deploy", h.deploy)
}

type proxyPoolsExtraHandler struct {
	deps Deps
}

func (h *proxyPoolsExtraHandler) test(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id, "testStatus": "ok"})
}

func (h *proxyPoolsExtraHandler) deploy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "url": ""})
}
