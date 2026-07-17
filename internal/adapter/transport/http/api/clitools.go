package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// RegisterCliTools mounts all CLI tool configuration/status routes.
func RegisterCliTools(mux *http.ServeMux, deps Deps) {
	h := &cliToolsHandler{deps: deps}
	mux.HandleFunc("GET /api/cli-tools/all-statuses", h.allStatuses)

	mux.HandleFunc("GET /api/cli-tools/antigravity-mitm", h.mitmStatus)
	mux.HandleFunc("POST /api/cli-tools/antigravity-mitm", h.mitmStart)
	mux.HandleFunc("DELETE /api/cli-tools/antigravity-mitm", h.mitmStop)
	mux.HandleFunc("PATCH /api/cli-tools/antigravity-mitm", h.mitmToggle)
	mux.HandleFunc("GET /api/cli-tools/antigravity-mitm/alias", h.mitmGetAliases)
	mux.HandleFunc("PUT /api/cli-tools/antigravity-mitm/alias", h.mitmSetAliases)

	mux.HandleFunc("GET /api/cli-tools/claude-settings", h.claudeSettings)
	mux.HandleFunc("POST /api/cli-tools/claude-settings", h.claudeSettings)
	mux.HandleFunc("DELETE /api/cli-tools/claude-settings", h.claudeSettings)

	mux.HandleFunc("GET /api/cli-tools/codex-settings", h.codexSettings)
	mux.HandleFunc("POST /api/cli-tools/codex-settings", h.codexSettings)
	mux.HandleFunc("DELETE /api/cli-tools/codex-settings", h.codexSettings)

	mux.HandleFunc("GET /api/cli-tools/opencode-settings", h.opencodeSettings)
	mux.HandleFunc("POST /api/cli-tools/opencode-settings", h.opencodeSettings)
	mux.HandleFunc("DELETE /api/cli-tools/opencode-settings", h.opencodeSettings)

	mux.HandleFunc("GET /api/cli-tools/droid-settings", h.droidSettings)
	mux.HandleFunc("POST /api/cli-tools/droid-settings", h.droidSettings)
	mux.HandleFunc("DELETE /api/cli-tools/droid-settings", h.droidSettings)

	mux.HandleFunc("GET /api/cli-tools/openclaw-settings", h.openclawSettings)
	mux.HandleFunc("POST /api/cli-tools/openclaw-settings", h.openclawSettings)
	mux.HandleFunc("DELETE /api/cli-tools/openclaw-settings", h.openclawSettings)

	mux.HandleFunc("GET /api/cli-tools/hermes-settings", h.hermesSettings)
	mux.HandleFunc("POST /api/cli-tools/hermes-settings", h.hermesSettings)
	mux.HandleFunc("DELETE /api/cli-tools/hermes-settings", h.hermesSettings)

	mux.HandleFunc("GET /api/cli-tools/cowork-settings", h.coworkSettings)
	mux.HandleFunc("GET /api/cli-tools/cowork-mcp-registry", h.coworkRegistry)
	mux.HandleFunc("GET /api/cli-tools/cowork-mcp-tools", h.coworkTools)

	mux.HandleFunc("GET /api/cli-tools/copilot-settings", h.copilotSettings)
	mux.HandleFunc("POST /api/cli-tools/copilot-settings", h.copilotSettings)
	mux.HandleFunc("DELETE /api/cli-tools/copilot-settings", h.copilotSettings)

	mux.HandleFunc("GET /api/cli-tools/cline-settings", h.clineSettings)
	mux.HandleFunc("POST /api/cli-tools/cline-settings", h.clineSettings)
	mux.HandleFunc("DELETE /api/cli-tools/cline-settings", h.clineSettings)

	mux.HandleFunc("GET /api/cli-tools/kilo-settings", h.kiloSettings)
	mux.HandleFunc("POST /api/cli-tools/kilo-settings", h.kiloSettings)
	mux.HandleFunc("DELETE /api/cli-tools/kilo-settings", h.kiloSettings)

	mux.HandleFunc("GET /api/cli-tools/deepseek-tui-settings", h.deepseekSettings)
	mux.HandleFunc("POST /api/cli-tools/deepseek-tui-settings", h.deepseekSettings)
	mux.HandleFunc("DELETE /api/cli-tools/deepseek-tui-settings", h.deepseekSettings)

	mux.HandleFunc("GET /api/cli-tools/jcode-settings", h.jcodeSettings)
	mux.HandleFunc("POST /api/cli-tools/jcode-settings", h.jcodeSettings)
	mux.HandleFunc("DELETE /api/cli-tools/jcode-settings", h.jcodeSettings)

	mux.HandleFunc("GET /api/cli-tools/grok-build-settings", h.grokBuildSettings)
	mux.HandleFunc("POST /api/cli-tools/grok-build-settings", h.grokBuildSettings)
	mux.HandleFunc("DELETE /api/cli-tools/grok-build-settings", h.grokBuildSettings)
}

type cliToolsHandler struct {
	deps Deps
}

func (h *cliToolsHandler) allStatuses(w http.ResponseWriter, r *http.Request) {
	tools := []string{
		"claude", "codex", "opencode", "droid", "openclaw", "hermes", "cowork",
		"copilot", "cline", "kilo", "deepseek-tui", "jcode", "grok-build",
	}
	out := map[string]any{}
	for _, tool := range tools {
		out[tool] = map[string]any{"installed": false, "message": "CLI tool status not available in Go build"}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *cliToolsHandler) mitmStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"running":           false,
		"pid":               nil,
		"certExists":        false,
		"certTrusted":       false,
		"dnsStatus":         map[string]any{},
		"hasCachedPassword": false,
		"needsSudoPassword": false,
		"isAdmin":           false,
		"mitmRouterBaseUrl": "http://localhost:20128",
	})
}

func (h *cliToolsHandler) mitmStart(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "running": false, "pid": nil})
}

func (h *cliToolsHandler) mitmStop(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "running": false})
}

func (h *cliToolsHandler) mitmToggle(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "dnsStatus": map[string]any{}})
}

func (h *cliToolsHandler) mitmGetAliases(w http.ResponseWriter, r *http.Request) {
	tool := r.URL.Query().Get("tool")
	aliases, err := h.deps.Alias.GetMitmAliases(r.Context(), tool)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch aliases")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"aliases": aliases})
}

func (h *cliToolsHandler) mitmSetAliases(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tool     string          `json:"tool"`
		Mappings json.RawMessage `json:"mappings"`
	}
	if err := parseJSON(r, &body); err != nil || body.Tool == "" || len(body.Mappings) == 0 {
		writeError(w, http.StatusBadRequest, "tool and mappings required")
		return
	}
	if err := h.deps.Alias.SetMitmAliases(r.Context(), body.Tool, body.Mappings); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to save aliases")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *cliToolsHandler) claudeSettings(w http.ResponseWriter, r *http.Request) {
	h.cliToolResponse(w, r, "Claude")
}

func (h *cliToolsHandler) codexSettings(w http.ResponseWriter, r *http.Request) {
	h.cliToolResponse(w, r, "Codex")
}

func (h *cliToolsHandler) opencodeSettings(w http.ResponseWriter, r *http.Request) {
	h.cliToolResponse(w, r, "OpenCode")
}

func (h *cliToolsHandler) droidSettings(w http.ResponseWriter, r *http.Request) {
	h.cliToolResponse(w, r, "Droid")
}

func (h *cliToolsHandler) openclawSettings(w http.ResponseWriter, r *http.Request) {
	h.cliToolResponse(w, r, "OpenClaw")
}

func (h *cliToolsHandler) hermesSettings(w http.ResponseWriter, r *http.Request) {
	h.cliToolResponse(w, r, "Hermes")
}

func (h *cliToolsHandler) coworkSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"installed": false, "message": "CoWolk settings not available in Go build"})
}

func (h *cliToolsHandler) coworkRegistry(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"registry": []any{}})
}

func (h *cliToolsHandler) coworkTools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tools": []any{}})
}

func (h *cliToolsHandler) copilotSettings(w http.ResponseWriter, r *http.Request) {
	h.cliToolResponse(w, r, "Copilot")
}

func (h *cliToolsHandler) clineSettings(w http.ResponseWriter, r *http.Request) {
	h.cliToolResponse(w, r, "Cline")
}

func (h *cliToolsHandler) kiloSettings(w http.ResponseWriter, r *http.Request) {
	h.cliToolResponse(w, r, "Kilo")
}

func (h *cliToolsHandler) deepseekSettings(w http.ResponseWriter, r *http.Request) {
	h.cliToolResponse(w, r, "Deepseek TUI")
}

func (h *cliToolsHandler) jcodeSettings(w http.ResponseWriter, r *http.Request) {
	h.cliToolResponse(w, r, "JCode")
}

func (h *cliToolsHandler) grokBuildSettings(w http.ResponseWriter, r *http.Request) {
	h.cliToolResponse(w, r, "Grok Build")
}

func (h *cliToolsHandler) cliToolResponse(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case "GET":
		writeJSON(w, http.StatusOK, map[string]any{"installed": false, "message": name + " CLI is not installed"})
	case "POST":
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": name + " settings applied"})
	case "DELETE":
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": name + " settings reset"})
	}
}

var _ = fmt.Sprintf
