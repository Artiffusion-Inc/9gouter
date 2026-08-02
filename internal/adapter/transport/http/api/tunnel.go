package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/tunnel"
	"time"
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
	if h.deps.CloudflareTunnel != nil {
		cfStatus := h.deps.CloudflareTunnel.Status()
		snap.Tunnel.Enabled = cfStatus.Enabled
		snap.Tunnel.TunnelURL = cfStatus.TunnelURL
		snap.Tunnel.PublicURL = cfStatus.TunnelURL
	}
	if h.deps.TailscaleTunnel != nil {
		tsStatus := h.deps.TailscaleTunnel.Status()
		snap.Tailscale.Enabled = tsStatus.Enabled
		snap.Tailscale.TunnelURL = tsStatus.TunnelURL
	}
	snap.Tunnel.SettingsEnabled = snap.Tunnel.Enabled
	snap.Tailscale.SettingsEnabled = snap.Tailscale.Enabled
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
	if h.deps.CloudflareTunnel == nil {
		writeError(w, http.StatusServiceUnavailable, "Tunnel manager not configured")
		return
	}
	localPort := h.localPort()

	// Ensure cloudflared is available.
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	dataDir := h.dataDir()
	binPath, err := tunnel.EnsureCloudflared(ctx, dataDir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":   false,
			"enabled":   false,
			"tunnelUrl": "",
			"publicUrl": "",
			"error":     "Failed to obtain cloudflared binary: " + err.Error(),
		})
		return
	}

	url, err := h.deps.CloudflareTunnel.StartQuickTunnel(ctx, binPath, localPort)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":   false,
			"enabled":   false,
			"tunnelUrl": "",
			"publicUrl": "",
			"error":     err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"enabled":   true,
		"tunnelUrl": url,
		"publicUrl": url,
	})
}

func (h *tunnelHandler) disable(w http.ResponseWriter, r *http.Request) {
	if h.deps.CloudflareTunnel != nil {
		h.deps.CloudflareTunnel.Stop()
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "enabled": false})
}

// tailscaleCheck reports Tailscale install / login state. The frontend reads
// `installed` and `hasCachedPassword` (handleOpenTsModal), and `loggedIn`
// (the login-polling loop in handleConnectTailscale).
func (h *tunnelHandler) tailscaleCheck(w http.ResponseWriter, r *http.Request) {
	installed := false
	running := false
	if h.deps.TailscaleTunnel != nil {
		installed = h.deps.TailscaleTunnel.IsInstalled()
		running = h.deps.TailscaleTunnel.IsRunning()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installed":         installed,
		"running":           running,
		"loggedIn":          running, // running implies logged in
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
	if h.deps.TailscaleTunnel == nil {
		writeError(w, http.StatusServiceUnavailable, "Tailscale manager not configured")
		return
	}
	localPort := h.localPort()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	url, err := h.deps.TailscaleTunnel.StartFunnel(ctx, localPort)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":          false,
			"enabled":          false,
			"needsLogin":       false,
			"funnelNotEnabled": false,
			"error":            err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"enabled":    true,
		"tunnelUrl":  url,
		"needsLogin": false,
	})
}

func (h *tunnelHandler) tailscaleDisable(w http.ResponseWriter, r *http.Request) {
	if h.deps.TailscaleTunnel != nil {
		_ = h.deps.TailscaleTunnel.StopFunnel()
	}
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
// connect flow.
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

	if h.deps.TailscaleTunnel != nil && h.deps.TailscaleTunnel.IsInstalled() {
		writeSSEFrame("progress", map[string]any{"message": "Tailscale is already installed."})
		writeSSEFrame("done", map[string]any{"installed": true})
		return
	}

	installCmd := tailscaleInstallCommand()
	if installCmd == nil {
		writeSSEFrame("progress", map[string]any{"message": "Auto-install not supported. Install from https://tailscale.com/download"})
		writeSSEFrame("done", map[string]any{"installed": false})
		return
	}

	writeSSEFrame("progress", map[string]any{"message": "Running: " + installCmd.String()})
	if output, err := installCmd.CombinedOutput(); err != nil {
		writeSSEFrame("progress", map[string]any{"message": "Install failed: " + err.Error()})
		writeSSEFrame("done", map[string]any{"installed": false, "message": string(output)})
		return
	}

	writeSSEFrame("progress", map[string]any{"message": "Tailscale installed successfully."})
	writeSSEFrame("done", map[string]any{"installed": true})
}

func tailscaleInstallCommand() *exec.Cmd {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("brew", "install", "tailscale")
	case "linux":
		return exec.Command("sh", "-c", "curl -fsSL https://tailscale.com/install.sh | sh")
	default:
		return nil
	}
}

// dataDir returns the data directory for storing downloaded binaries.
// localPort returns the server's local port for tunnel targeting.
func (h *tunnelHandler) localPort() int {
	return 20127 // default Go backend port
}

// dataDir returns the data directory for storing downloaded binaries.
func (h *tunnelHandler) dataDir() string {
	return filepath.Join(".", ".9router")
}
