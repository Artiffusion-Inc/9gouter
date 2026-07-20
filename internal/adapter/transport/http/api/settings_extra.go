package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
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

// proxyTestRequest is the request body for POST /api/settings/proxy-test.
type proxyTestRequest struct {
	ProxyURL string `json:"proxyUrl"`
}

// proxyTest performs a TCP reachability check against the supplied proxy URL
// and reports the result. The frontend expects the contract
//
//	{ ok: bool, status: string, elapsedMs: int }
//
// `status` is a human-readable label ("ok" on success, the dial error string
// on failure) and `elapsedMs` is the wall-clock duration of the probe.
func (h *settingsExtraHandler) proxyTest(w http.ResponseWriter, r *http.Request) {
	var req proxyTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ProxyURL == "" {
		writeError(w, http.StatusBadRequest, "proxyUrl is required")
		return
	}

	// Use the package's TCP fast-fail probe so the check is identical to
	// the one used during real fetches. We give the probe a generous
	// timeout (5s) so the user sees a clear success/failure rather than
	// the 2s default.
	opts := proxy.Options{
		ProxyFastFailTimeout: 5 * time.Second,
	}

	start := time.Now()
	err := proxy.FastFail(r.Context(), opts, req.ProxyURL)
	elapsed := time.Since(start)

	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"status":    "ok",
			"elapsedMs": elapsed.Milliseconds(),
			// Keep `success`/`latencyMs` for any older clients that still
			// read them; the frontend will switch to the new contract.
			"success":   true,
			"latencyMs": elapsed.Milliseconds(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        false,
		"status":    err.Error(),
		"elapsedMs": elapsed.Milliseconds(),
		"success":   false,
		"latencyMs": elapsed.Milliseconds(),
		"error":     err.Error(),
	})
}
