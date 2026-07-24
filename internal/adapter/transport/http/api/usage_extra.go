package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/managedashboard"
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
	// Real-time analytics SSE (#83). Mirrors legacy /api/usage/stream: on open,
	// push a live frame immediately, then subscribe to the live EventTracker
	// and push a lightweight frame (recentRequests + activeRequests +
	// errorProvider + pending) on every state change. A 25s keepalive ping
	// keeps proxies from closing the idle connection. If no tracker is wired
	// (UsageTracker nil), fall back to a single empty frame so the UI still
	// receives one event and settles instead of hanging the EventSource.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	writeFrame := func(payload map[string]any) {
		b, _ := json.Marshal(payload)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		flush()
	}

	tracker := h.deps.UsageTracker
	if tracker == nil {
		writeFrame(map[string]any{})
		return
	}

	// Connection display-name resolver (best-effort join over providerConnections).
	connName := h.connNameFunc(r.Context())
	buildFrame := func() map[string]any {
		return map[string]any{
			"activeRequests": tracker.ActiveRequests(r.Context(), connName),
			"recentRequests": tracker.RecentRequests(20),
			"errorProvider":  tracker.ErrorProvider(),
			"pending":        tracker.Snapshot(),
		}
	}

	// Immediate frame so the client sees current state without waiting.
	writeFrame(buildFrame())

	// Subscribe and pump on every notify until the client disconnects.
	notifyCh := make(chan struct{}, 1)
	unsub := tracker.Subscribe(func() {
		select {
		case notifyCh <- struct{}{}:
		default:
		}
	})
	defer unsub()

	stop := r.Context().Done()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-stop:
			return
		case <-keepalive.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flush()
		case <-notifyCh:
			writeFrame(buildFrame())
		}
	}
}

// connNameFunc returns a connection-id → display-name resolver backed by the
// ConnectionRepo. Best-effort: on repo error or absent repo, falls back to
// "Account <id[:8]>..." which matches the legacy JS default.
func (h *usageExtraHandler) connNameFunc(ctx context.Context) func(id string) string {
	if h.deps.Connections == nil {
		return func(id string) string { return "Account " + shortIDForConn(id) }
	}
	conns, err := h.deps.Connections.List(ctx, repo.ConnectionFilter{})
	cache := map[string]string{}
	if err == nil {
		for _, c := range conns {
			name := c.Name
			if name == "" {
				name = c.Email
			}
			if name == "" {
				name = c.ID
			}
			cache[c.ID] = name
		}
	}
	return func(id string) string {
		if n, ok := cache[id]; ok {
			return n
		}
		return "Account " + shortIDForConn(id)
	}
}

func shortIDForConn(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (h *usageExtraHandler) requestLogs(w http.ResponseWriter, r *http.Request) {
	// Frontend (RequestLogger.js) awaits res.json() and uses the result as an
	// array of formatted "date | model | provider | account | in | out | status"
	// log lines directly (not data.logs). The legacy /api/usage/request-logs
	// handler returned getRecentLogs(200) as a bare array — match that contract.
	svc := &managedashboard.UsageService{Repo: h.deps.Usage}
	logs, err := svc.RecentLogs(r.Context(), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch logs")
		return
	}
	writeJSON(w, http.StatusOK, logs)
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
	// GET (view-credits) — port 5cc4f222: fetch the live reset-credits
	// inventory from the Codex upstream for this connection. Falls back to a
	// parseable empty payload (with a user-facing message) when the connection
	// is missing, not codex, has no token, or the upstream errors — so the UI
	// renders the "no credits" state instead of crashing on `undefined`.
	conn, err := h.deps.Connections.GetByID(r.Context(), id)
	if err != nil || conn == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"credits":      []any{},
			"connectionId": strings.TrimPrefix(id, "/"),
			"message":      "Connection not found.",
		})
		return
	}
	payload, _ := fetchCodexResetCredits(r.Context(), conn)
	writeJSON(w, http.StatusOK, payload)
}
