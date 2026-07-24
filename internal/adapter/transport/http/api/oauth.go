package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/resolver/tokenrefresh"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
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
	mux.HandleFunc("POST /api/oauth/grok-cli/device-code", h.grokCliDeviceCode)
	mux.HandleFunc("POST /api/oauth/grok-cli/poll", h.grokCliPoll)
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
	h.codexBulkImportAccounts(w, r)
}

// codexBulkImportAccounts implements POST /api/oauth/codex/bulk-import.
//
// Frontend contract (BulkImportCodexModal.js):
//
//	body: { accounts: [{ accessToken, refreshToken, idToken, email }, ...] }
//
//	resp: {
//	  success: <int>  // count successfully added (0 if all failed),
//	  failed:  <int>, // count that failed validation/persistence,
//	  results: [{ index, ok: bool, error?: string, id?: string }, ...],
//	}
//
// Each account becomes a new providerConnections row (provider="codex",
// authType="oauth"). On persistence failure the row is skipped and the entry
// is added to results with ok=false. The endpoint is intentionally permissive
// (does NOT 4xx on partial failure) so the modal can render the per-row list.
func (h *oauthHandler) codexBulkImportAccounts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := parseJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if h.deps.Connections == nil {
		writeError(w, http.StatusServiceUnavailable, "Connections repo unavailable")
		return
	}

	results := make([]map[string]any, 0, len(body.Accounts))
	success := 0
	failed := 0
	now := time.Now().UTC()
	for i, raw := range body.Accounts {
		if raw == nil {
			results = append(results, map[string]any{"index": i, "ok": false, "error": "empty account"})
			failed++
			continue
		}
		accessToken, _ := raw["accessToken"].(string)
		refreshToken, _ := raw["refreshToken"].(string)
		idToken, _ := raw["idToken"].(string)
		email, _ := raw["email"].(string)
		if strings.TrimSpace(accessToken) == "" {
			results = append(results, map[string]any{"index": i, "ok": false, "error": "accessToken is required"})
			failed++
			continue
		}

		// Backfill missing identity fields from the JWT (idToken preferred,
		// else the accessToken) the same way the JS bulk-import route does
		// (extractCodexAccountInfo): email, chatgptAccountId, chatgptPlanType.
		// The chatgptAccountId is what the c73c419d dedup rule keys on — without
		// it a re-import of the same account would create a duplicate row.
		psd, _ := raw["providerSpecificData"].(map[string]any)
		if psd == nil {
			psd = map[string]any{}
		}
		info := extractCodexAccountInfo(idToken)
		if info.Email == "" {
			info = extractCodexAccountInfo(accessToken)
		}
		if email == "" {
			email = info.Email
		}
		if _, has := psd["chatgptAccountId"]; !has && info.ChatGPTAccountID != "" {
			psd["chatgptAccountId"] = info.ChatGPTAccountID
		}
		if _, has := psd["chatgptPlanType"]; !has && info.ChatGPTPlanType != "" {
			psd["chatgptPlanType"] = info.ChatGPTPlanType
		}

		// Compute expiresAt from expiresIn when absent (JS:
		// new Date(Date.now()+expiresIn*1000).toISOString()).
		expiresAt, _ := raw["expiresAt"].(string)
		if expiresAt == "" {
			if expiresIn, ok := raw["expiresIn"].(float64); ok {
				expiresAt = codexExpiresAtFromExpiresIn(expiresIn, now)
			}
		}

		data := map[string]any{
			"accessToken":   accessToken,
			"refreshToken":  refreshToken,
			"idToken":       idToken,
			"email":         email,
			"testStatus":    "active",
			"lastRefreshAt": now.UTC().Format(time.RFC3339),
		}
		if expiresAt != "" {
			data["expiresAt"] = expiresAt
		}
		if len(psd) > 0 {
			data["providerSpecificData"] = psd
		}
		dataJSON, err := json.Marshal(data)
		if err != nil {
			results = append(results, map[string]any{"index": i, "ok": false, "error": "failed to encode data: " + err.Error()})
			failed++
			continue
		}

		conn := settings.ProviderConnection{
			ID:        fmt.Sprintf("codex-%d", now.UnixNano()+int64(i)),
			Provider:  "codex",
			AuthType:  "oauth",
			Name:      email,
			Email:     email,
			Priority:  0,
			IsActive:  true,
			Data:      dataJSON,
			CreatedAt: now,
			UpdatedAt: now,
		}
		resolved, err := h.deps.Connections.Create(r.Context(), conn)
		if err != nil {
			results = append(results, map[string]any{"index": i, "ok": false, "error": err.Error()})
			failed++
			continue
		}
		// resolved.ID may differ from conn.ID when Create merged onto an
		// existing same-identity row (cb0135b6 dedup) — surface the real id.
		results = append(results, map[string]any{"index": i, "ok": true, "id": resolved.ID})
		success++
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": success,
		"failed":  failed,
		"results": results,
	})
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
	// POST /api/oauth/kiro/import-cli-proxy — import a CLIProxyAPI auth JSON
	// (Microsoft external_idp) as a kiro connection. Mirrors
	// src/app/api/oauth/kiro/import-cli-proxy/route.js: parse the auth blob
	// (cliProxyAuth | auth | json | body), normalize via
	// tokenrefresh.NormalizeKiroExternalIDPAuth, persist via
	// Connections.Create (which runs the cb0135b6 cross-IdP dedup so a re-import
	// of the same identity merges onto the existing row).
	var body map[string]any
	// tolerate empty/invalid body — Normalize returns a clear error below.
	_ = parseJSON(r, &body)
	if body == nil {
		body = map[string]any{}
	}
	raw := firstNonNil(body["cliProxyAuth"], body["auth"], body["json"], body)

	normalized, err := tokenrefresh.NormalizeKiroExternalIDPAuth(raw, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.deps.Connections == nil {
		writeError(w, http.StatusServiceUnavailable, "Connections repo unavailable")
		return
	}

	data := map[string]any{
		"accessToken":  normalized.AccessToken,
		"refreshToken": normalized.RefreshToken,
		"email":        normalized.Email,
		"testStatus":   "active",
	}
	if normalized.ExpiresAt != "" {
		data["expiresAt"] = normalized.ExpiresAt
	}
	for k, v := range normalized.ProviderSpecificData {
		data[k] = v
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode connection data: "+err.Error())
		return
	}

	now := time.Now()
	conn := settings.ProviderConnection{
		ID:        fmt.Sprintf("kiro-%d", now.UnixNano()),
		Provider:  "kiro",
		AuthType:  "oauth",
		Name:      normalized.Email,
		Email:     normalized.Email,
		Priority:  0,
		IsActive:  true,
		Data:      dataJSON,
		CreatedAt: now,
		UpdatedAt: now,
	}
	resolved, err := h.deps.Connections.Create(r.Context(), conn)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// resolved.ID may differ from conn.ID when Create merged onto an existing
	// same-identity row (cb0135b6 dedup) — surface the real id.
	resolvedID := conn.ID
	resolvedEmail := normalized.Email
	if resolved != nil {
		resolvedID = resolved.ID
		if resolved.Email != "" {
			resolvedEmail = resolved.Email
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"connection": map[string]any{
			"id":       resolvedID,
			"provider": "kiro",
			"email":    resolvedEmail,
		},
	})
}

// firstNonNil returns the first non-nil argument, or nil if all are nil.
func firstNonNil(vs ...any) any {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
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
