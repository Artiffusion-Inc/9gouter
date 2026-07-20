package api

import (
	"encoding/json"
	"net/http"
)

// RegisterTunnel mounts tunnel and Tailscale management routes.
func RegisterTunnel(mux *http.ServeMux, deps Deps) {
	h := &tunnelHandler{deps: deps}
	mux.HandleFunc("GET /api/tunnel/status", h.status)
	mux.HandleFunc("POST /api/tunnel/enable", h.enable)
	mux.HandleFunc("POST /api/tunnel/disable", h.disable)
	mux.HandleFunc("GET /api/tunnel/tailscale-check", h.tailscaleCheck)
	mux.HandleFunc("POST /api/tunnel/tailscale-enable", h.tailscaleEnable)
	mux.HandleFunc("POST /api/tunnel/tailscale-disable", h.tailscaleDisable)
	mux.HandleFunc("POST /api/tunnel/tailscale-install", h.tailscaleInstall)
}

type tunnelHandler struct {
	deps Deps
}

// tunnelStatusSnapshot is the response shape consumed by the dashboard
// EndpointPageClient. Field names must stay in sync with
// src/app/(dashboard)/dashboard/endpoint/EndpointPageClient.js
// (syncTunnelStatus / loadSettings).
type tunnelStatusSnapshot struct {
	Tunnel struct {
		Enabled         bool   `json:"enabled"`
		SettingsEnabled bool   `json:"settingsEnabled"`
		TunnelURL       string `json:"tunnelUrl"`
		PublicURL       string `json:"publicUrl"`
	} `json:"tunnel"`
	Tailscale struct {
		Enabled         bool   `json:"enabled"`
		SettingsEnabled bool   `json:"settingsEnabled"`
		TunnelURL       string `json:"tunnelUrl"`
	} `json:"tailscale"`
	Download struct {
		Downloading bool `json:"downloading"`
		Progress    int  `json:"progress"`
	} `json:"download"`
}

func (h *tunnelHandler) status(w http.ResponseWriter, r *http.Request) {
	var snap tunnelStatusSnapshot
	snap.Tunnel.Enabled = false
	snap.Tunnel.SettingsEnabled = false
	snap.Tailscale.Enabled = false
	snap.Tailscale.SettingsEnabled = false
	writeJSON(w, http.StatusOK, snap)
}

// enable handles POST /api/tunnel/enable.
//
// Frontend contract (EndpointPageClient.handleEnableTunnel, ~line 339-355):
//   - On HTTP 200: reads `data.tunnelUrl` (required) and `data.publicUrl` (optional).
//   - On non-OK: reads `data.error` to surface the message.
//   - The legacy JS tunnel manager returns `{ success, tunnelUrl, publicUrl }`.
//
// The Go rewrite does not implement the actual Cloudflare quick-tunnel
// orchestration yet, so we surface a clear "not implemented" error in the
// exact shape the frontend parses, instead of a misleading 200 with an empty URL.
func (h *tunnelHandler) enable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"success":   false,
		"enabled":   false,
		"tunnelUrl": "",
		"publicUrl": "",
		"error":     "Cloudflare tunnel orchestration is not yet implemented in the Go backend. Stay on the legacy JS backend for tunnel support.",
	})
}

func (h *tunnelHandler) disable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "enabled": false})
}

// tailscaleCheck reports Tailscale install / login state. The frontend reads
// `installed` and `hasCachedPassword` (handleOpenTsModal), and `loggedIn`
// (the login-polling loop in handleConnectTailscale).
func (h *tunnelHandler) tailscaleCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"installed":         false,
		"running":           false,
		"loggedIn":          false,
		"hasCachedPassword": false,
	})
}

// tailscaleEnable handles POST /api/tunnel/tailscale-enable.
//
// Frontend contract (EndpointPageClient.handleConnectTailscale, ~line 481-547):
//
//   - { success: true,  tunnelUrl }                   — connected, frontend pings /api/health.
//   - { success: false, needsLogin: true, authUrl }   — user must visit auth URL; frontend
//     then polls /tailscale-check for loggedIn=true
//     and retries enable.
//   - { success: false, funnelNotEnabled: true, enableUrl } — Funnel toggle in admin console
//     required; frontend polls enableUrl-style flow.
//   - { error: "..." }                                — fatal; frontend surfaces to UI.
//
// The Go rewrite does not implement the actual Tailscale CLI orchestration yet,
// so we report a clear "not implemented" error. The shape of the error response
// matches what the frontend's catch-all `data.error || "Failed to connect"` path
// already handles, so the UI degrades to an error banner.
func (h *tunnelHandler) tailscaleEnable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"success":          false,
		"enabled":          false,
		"needsLogin":       false,
		"funnelNotEnabled": false,
		"error":            "Tailscale Funnel orchestration is not yet implemented in the Go backend. Stay on the legacy JS backend for Tailscale support.",
	})
}

func (h *tunnelHandler) tailscaleDisable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "enabled": false})
}

// tailscaleInstall handles POST /api/tunnel/tailscale-install.
//
// Frontend contract (EndpointPageClient.handleInstallTailscale, ~line 401-452):
// The frontend opens the response as a ReadableStream, splits on "\n\n", and
// for each frame parses:
//
//   - `event: progress` + `data: { "message": "..." }`  → append message to install log
//   - `event: done`     + `data: { "installed": true }`  → mark installed, trigger connect
//   - `event: error`    + `data: { "error": "..." }`     → surface to UI
//
// Until the actual Tailscale install orchestration is implemented, we
// stream a single progress frame, then a done frame, and close — so the
// install UI completes gracefully and the user can attempt the (also
// stubbed) connect flow.
func (h *tunnelHandler) tailscaleInstall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	writeSSEFrame := func(event string, payload any) bool {
		body, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := w.Write([]byte("event: " + event + "\ndata: ")); err != nil {
			return false
		}
		if _, err := w.Write(body); err != nil {
			return false
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	// Best-effort: log the body so users can confirm the request reached us.
	// We don't act on sudoPassword yet — install is unimplemented.
	var body struct {
		SudoPassword string `json:"sudoPassword"`
	}
	_ = parseOptionalJSON(r, &body)

	if !writeSSEFrame("progress", map[string]any{
		"message": "Tailscale install is not yet implemented in the Go backend. Use the legacy JS backend for installer support.",
	}) {
		return
	}
	if !writeSSEFrame("done", map[string]any{
		"installed": false,
		"message":   "Install orchestration pending. Returning to dashboard.",
	}) {
		return
	}
}
