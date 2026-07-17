package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// RegisterOAuth mounts all OAuth helper routes.
func RegisterOAuth(mux *http.ServeMux, deps Deps) {
	h := &oauthHandler{deps: deps}
	mux.HandleFunc("GET /api/oauth/{provider}/{action}", h.providerAction)
	mux.HandleFunc("POST /api/oauth/{provider}/{action}", h.providerAction)
	mux.HandleFunc("POST /api/oauth/codex/bulk-import", h.codexBulkImport)
	mux.HandleFunc("POST /api/oauth/codex/import-token", h.codexImportToken)
	mux.HandleFunc("POST /api/oauth/cursor/auto-import", h.cursorAutoImport)
	mux.HandleFunc("POST /api/oauth/cursor/import", h.cursorImport)
	mux.HandleFunc("POST /api/oauth/gitlab/pat", h.gitlabPat)
	mux.HandleFunc("POST /api/oauth/iflow/cookie", h.iflowCookie)
	mux.HandleFunc("POST /api/oauth/kiro/api-key", h.kiroAPIKey)
	mux.HandleFunc("POST /api/oauth/kiro/auto-import", h.kiroAutoImport)
	mux.HandleFunc("POST /api/oauth/kiro/import", h.kiroImport)
	mux.HandleFunc("POST /api/oauth/kiro/import-cli-proxy", h.kiroImportCliProxy)
	mux.HandleFunc("POST /api/oauth/kiro/social-authorize", h.kiroSocialAuthorize)
	mux.HandleFunc("POST /api/oauth/kiro/social-exchange", h.kiroSocialExchange)
}

type oauthHandler struct {
	deps Deps
}

func (h *oauthHandler) providerAction(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	action := r.PathValue("action")
	writeJSON(w, http.StatusOK, map[string]any{
		"success":  false,
		"message":  "OAuth provider flow stubbed in Go build",
		"provider": provider,
		"action":   action,
	})
}

func (h *oauthHandler) codexBulkImport(w http.ResponseWriter, r *http.Request) {
	h.importTokens(w, r, "codex")
}

func (h *oauthHandler) codexImportToken(w http.ResponseWriter, r *http.Request) {
	h.importTokens(w, r, "codex")
}

func (h *oauthHandler) cursorAutoImport(w http.ResponseWriter, r *http.Request) {
	h.importTokens(w, r, "cursor")
}

func (h *oauthHandler) cursorImport(w http.ResponseWriter, r *http.Request) {
	h.importTokens(w, r, "cursor")
}

func (h *oauthHandler) gitlabPat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	_ = parseJSON(r, &body)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "tokenSaved": strings.TrimSpace(body.Token) != ""})
}

func (h *oauthHandler) iflowCookie(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cookie string `json:"cookie"`
	}
	_ = parseJSON(r, &body)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "cookieSaved": strings.TrimSpace(body.Cookie) != ""})
}

func (h *oauthHandler) kiroAPIKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey string `json:"apiKey"`
	}
	_ = parseJSON(r, &body)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "apiKeySaved": strings.TrimSpace(body.APIKey) != ""})
}

func (h *oauthHandler) kiroAutoImport(w http.ResponseWriter, r *http.Request) {
	h.importTokens(w, r, "kiro")
}

func (h *oauthHandler) kiroImport(w http.ResponseWriter, r *http.Request) {
	h.importTokens(w, r, "kiro")
}

func (h *oauthHandler) kiroImportCliProxy(w http.ResponseWriter, r *http.Request) {
	h.importTokens(w, r, "kiro")
}

func (h *oauthHandler) kiroSocialAuthorize(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "authUrl": "https://kiro.example.com/oauth/authorize"})
}

func (h *oauthHandler) kiroSocialExchange(w http.ResponseWriter, r *http.Request) {
	h.importTokens(w, r, "kiro")
}

func (h *oauthHandler) importTokens(w http.ResponseWriter, r *http.Request, provider string) {
	var body struct {
		Tokens string `json:"tokens"`
	}
	_ = parseJSON(r, &body)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "imported": 0, "provider": provider})
}

var _ = json.Marshal
