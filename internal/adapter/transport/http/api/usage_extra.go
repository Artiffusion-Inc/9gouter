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
	mux.HandleFunc("GET /api/usage/{connectionId}/codex-reset-credits", h.codexResetCredits)
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

	// Frontend (ProviderLimits → parseQuotaData) reads:
	//   data.quotas   → map[name]quota   (where quota has used/total/resetAt/remaining/etc.)
	//   data.plan     → optional plan object
	//   data.message  → optional status/error message
	//   response.status === 404 → silently skip
	// Match the legacy /api/usage/[connectionId] contract.

	c, err := h.deps.Connections.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch connection")
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound, "Connection not found")
		return
	}

	// OAuth connections and whitelisted apikey providers expose quota.
	// Other apikey/connections have no usage API implemented yet — return a
	// parseable empty payload so the frontend can render a "not available"
	// row instead of throwing on undefined.
	provider := ""
	if c != nil {
		provider = c.Provider
	}
	authType := ""
	if c != nil {
		authType = c.AuthType
	}
	if !isUsageEligible(provider, authType) {
		writeJSON(w, http.StatusOK, map[string]any{
			"quotas":  map[string]any{},
			"message": "Usage not available for this connection",
		})
		return
	}

	// Live quota fetching is provider-specific and not yet ported into the Go
	// rewrite. Return a parseable empty quotas map so the UI can keep the
	// "loading → loaded" state machine working without crashing on `null`.
	writeJSON(w, http.StatusOK, map[string]any{
		"connectionId": id,
		"provider":     provider,
		"quotas":       map[string]any{},
		"message":      "Quota fetch not implemented for this provider in the Go backend yet",
	})
}

// isUsageEligible mirrors the legacy JS whitelist:
// authType "oauth" → eligible; authType in {"apikey","api_key"} AND provider
// in the apikey-quota list → eligible. The apikey-quota list (glm, minimax,
// kiro, codebuddy-cn, grok-cli, qoder, vercel-ai-gateway) is taken from
// open-sse/shared/constants/providers.js → USAGE_APIKEY_PROVIDERS.
func isUsageEligible(provider, authType string) bool {
	if authType == "oauth" {
		return true
	}
	if authType != "apikey" && authType != "api_key" {
		return false
	}
	switch provider {
	case "glm", "glm-cn",
		"minimax", "minimax-cn",
		"kiro",
		"codebuddy-cn",
		"grok-cli",
		"qoder",
		"vercel-ai-gateway":
		return true
	}
	return false
}

func (h *usageExtraHandler) codexResetCredits(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("connectionId")
	// Frontend (ProviderLimits → handleViewCodexResetCredits) reads:
	//   result.credits  → array of { expiresAt, ... }
	//   result.message / result.error → user-facing error string
	// On error it throws `result.error || result.message || "Failed to load Codex reset credits"`.
	if r.Method == http.MethodPost {
		writeJSON(w, http.StatusOK, map[string]any{
			"code":          "no_credit",
			"reset":         false,
			"windows_reset": 0,
			"message":       "No Codex reset credits available.",
			"connectionId":  strings.TrimPrefix(id, "/"),
		})
		return
	}
	// GET (view-credits) → return an empty credits array so the UI renders the
	// "no credits" state instead of crashing on `undefined`.
	writeJSON(w, http.StatusOK, map[string]any{
		"credits":      []any{},
		"connectionId": strings.TrimPrefix(id, "/"),
		"message":      "No Codex reset credits available.",
	})
}
