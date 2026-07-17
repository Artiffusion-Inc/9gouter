package api

import "net/http"

// RegisterSettingsExtra mounts additional settings routes.
func RegisterSettingsExtra(mux *http.ServeMux, deps Deps) {
	h := &settingsExtraHandler{deps: deps}
	mux.HandleFunc("GET /api/settings/database", h.database)
	mux.HandleFunc("POST /api/settings/proxy-test", h.proxyTest)
}

type settingsExtraHandler struct {
	deps Deps
}

func (h *settingsExtraHandler) database(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"path": "./data/9router.db", "size": 0})
}

func (h *settingsExtraHandler) proxyTest(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "latencyMs": 0})
}
