package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/Artiffusion-Inc/9gouter/internal/usecase/managedashboard"
)

// RegisterSettings mounts settings management routes.
func RegisterSettings(mux *http.ServeMux, deps Deps) {
	h := &settingsHandler{
		deps: deps,
		svc:  &managedashboard.SettingsService{Repo: deps.Settings},
	}
	mux.HandleFunc("GET /api/settings", h.get)
	mux.HandleFunc("PATCH /api/settings", h.patch)
	mux.HandleFunc("GET /api/settings/require-login", h.requireLogin)
}

type settingsHandler struct {
	deps Deps
	svc  *managedashboard.SettingsService
}

func (h *settingsHandler) get(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc.Get(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch settings")
		return
	}
	m := map[string]any{}
	_ = json.Unmarshal(s.Data, &m)
	// Remove secrets from response.
	delete(m, "password")
	delete(m, "oidcClientSecret")
	delete(m, "mitmSudoEncrypted")
	m["oidcConfigured"] = h.svc.OidcConfigured(s.Data)
	m["enableRequestLogs"] = false
	m["enableTranslator"] = false
	m["hasPassword"] = h.svc.HasPassword(s.Data)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, m)
}

func (h *settingsHandler) patch(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	updates, err := h.svc.Merge(r.Context(), body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update settings")
		return
	}
	m := map[string]any{}
	_ = json.Unmarshal(updates.Data, &m)
	delete(m, "password")
	delete(m, "oidcClientSecret")
	delete(m, "mitmSudoEncrypted")
	m["oidcConfigured"] = h.svc.OidcConfigured(updates.Data)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, m)
}

func (h *settingsHandler) requireLogin(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc.Get(r.Context())
	requireLogin := true
	tunnelDashboardAccess := true
	tunnelURL := ""
	tailscaleURL := ""
	if err == nil {
		var m map[string]any
		_ = json.Unmarshal(s.Data, &m)
		requireLogin = jsonBool(m, "requireLogin", true)
		tunnelDashboardAccess = jsonBool(m, "tunnelDashboardAccess", true)
		tunnelURL = jsonString(m, "tunnelUrl", "")
		tailscaleURL = jsonString(m, "tailscaleUrl", "")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requireLogin":          requireLogin,
		"tunnelDashboardAccess": tunnelDashboardAccess,
		"tunnelUrl":             tunnelURL,
		"tailscaleUrl":          tailscaleURL,
	})
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return []byte{}, nil
	}
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}
