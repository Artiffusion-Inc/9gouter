package api

import (
	"encoding/json"
	"net/http"
)

// RegisterSettingsExtra mounts additional settings routes.
func RegisterSettingsExtra(mux *http.ServeMux, deps Deps) {
	h := &settingsExtraHandler{deps: deps}
	mux.HandleFunc("GET /api/settings/database", h.databaseExport)
	mux.HandleFunc("POST /api/settings/database", h.databaseImport)
	mux.HandleFunc("POST /api/settings/proxy-test", h.proxyTest)
}

type settingsExtraHandler struct {
	deps Deps
}

// databaseExport mirrors the legacy ExportDb() (GET /api/settings/database):
// returns the full configuration as a JSON payload (settings, connections,
// nodes, proxy pools, api keys, combos, and kv-backed modelAliases /
// customModels / mitmAlias / pricing). Auth is enforced by the session
// middleware; this handler is not in publicRoutes.
func (h *settingsExtraHandler) databaseExport(w http.ResponseWriter, r *http.Request) {
	if h.deps.DB == nil {
		writeError(w, http.StatusInternalServerError, "database not available")
		return
	}
	out, err := ExportDb(r, h.deps.DB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to export database")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// databaseImport mirrors the legacy ImportDb() (POST /api/settings/database):
// wipes settings/connections/nodes/proxyPools/apiKeys/combos and the four
// kv scopes, then inserts the payload. Auth is enforced by the session
// middleware. The request body is the backup payload (optionally carrying a
// legacy "password" field which is ignored — auth is via the session cookie).
func (h *settingsExtraHandler) databaseImport(w http.ResponseWriter, r *http.Request) {
	if h.deps.DB == nil {
		writeError(w, http.StatusInternalServerError, "database not available")
		return
	}
	var payload BackupPayload
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if err := ImportDb(r, h.deps.DB, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "Failed to import database")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *settingsExtraHandler) proxyTest(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "latencyMs": 0})
}