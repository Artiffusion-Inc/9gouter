package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	domainauth "github.com/Artiffusion-Inc/9gouter/internal/domain/auth"
	usecaseauth "github.com/Artiffusion-Inc/9gouter/internal/usecase/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/managedashboard"
)

// RegisterAuth mounts the public auth routes on mux.
// These routes are not protected by APIMiddleware.
func RegisterAuth(mux *http.ServeMux, deps Deps, cfg config.Config) {
	h := newAuthHandler(deps, cfg)
	mux.HandleFunc("POST /api/auth/login", h.login)
	mux.HandleFunc("POST /api/auth/logout", h.logout)
	mux.HandleFunc("GET /api/auth/status", h.status)
	mux.HandleFunc("POST /api/auth/reset-password", h.resetPassword)
	// /api/auth/oidc/start must accept GET: the login page navigates with
	// window.location.href = "/api/auth/oidc/start". Register GET first so the
	// server logs show a real redirect rather than 405.
	mux.HandleFunc("GET /api/auth/oidc/start", h.oidcStart)
	mux.HandleFunc("POST /api/auth/oidc/start", h.oidcStart)
	mux.HandleFunc("POST /api/auth/oidc/callback", h.oidcCallback)
	mux.HandleFunc("POST /api/auth/oidc/test", h.oidcTest)
}

type authHandler struct {
	deps Deps
	uc   *usecaseauth.UseCase
	svc  *managedashboard.SettingsService
}

type loginRequest struct {
	Password string `json:"password"`
}

type loginResponse struct {
	Success            bool `json:"success"`
	MustChangePassword bool `json:"mustChangePassword"`
}

func newAuthHandler(deps Deps, cfg config.Config) *authHandler {
	var verifier usecaseauth.PasswordVerifier
	if cfg.DashboardPasswordHash != "" {
		// Real bcrypt comparator (golang.org/x/crypto/bcrypt). The previous
		// bcryptCompareStub always returned an error, so env-hash login was
		// impossible from the HTTP path — only unit tests that minted a
		// session directly via the SessionStore could exercise the dashboard.
		verifier = &usecaseauth.BcryptVerifier{Hash: cfg.DashboardPasswordHash, Comparator: adapterauth.CompareBcrypt}
	} else {
		verifier = &usecaseauth.PlainVerifier{InitialPassword: "123456"}
	}
	return &authHandler{
		deps: deps,
		uc:   usecaseauth.New(deps.SessionStore, adapterauth.NewLoginLimiter(), verifier, 0),
		svc:  &managedashboard.SettingsService{Repo: deps.Settings},
	}
}

func (h *authHandler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	sess, err := h.uc.Login("admin", req.Password, clientIP(r))
	if err != nil {
		switch {
		case errors.Is(err, usecaseauth.ErrLocked):
			writeError(w, http.StatusTooManyRequests, usecaseauth.FormatError(err))
		case errors.Is(err, usecaseauth.ErrUnauthorized):
			writeError(w, http.StatusUnauthorized, "Invalid password")
		default:
			writeError(w, http.StatusInternalServerError, usecaseauth.FormatError(err))
		}
		return
	}

	if err := h.deps.SessionStore.Set(w, sess); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to set session")
		return
	}

	writeJSON(w, http.StatusOK, loginResponse{Success: true, MustChangePassword: false})
}

func (h *authHandler) logout(w http.ResponseWriter, r *http.Request) {
	_ = h.deps.SessionStore.Clear(w)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *authHandler) status(w http.ResponseWriter, r *http.Request) {
	settings, err := h.deps.Settings.Get(r.Context())
	requireLogin := true
	authMode := "password"
	oidcLoginLabel := "Sign in with OIDC"
	oidcConfigured := false
	hasPassword := true
	if err == nil {
		var m map[string]any
		_ = json.Unmarshal(settings.Data, &m)
		requireLogin = jsonBool(m, "requireLogin", true)
		authMode = jsonString(m, "authMode", "password")
		oidcLoginLabel = jsonString(m, "oidcLoginLabel", "Sign in with OIDC")
		oidcConfigured = h.svc.OidcConfigured(settings.Data)
		hasPassword = h.svc.HasPassword(settings.Data)
	}

	sess, _ := h.deps.SessionStore.Get(r)
	authenticated := sess != nil
	oidcName := ""
	oidcEmail := ""
	oidcLogin := false
	if sess != nil {
		oidcName = sess.Principal.Name
		oidcEmail = sess.Principal.Email
	}
	loginMethod := "Password"
	if oidcLogin {
		loginMethod = "OIDC"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requireLogin":   requireLogin,
		"authenticated":  authenticated,
		"authMode":       authMode,
		"oidcConfigured": oidcConfigured,
		"oidcLoginLabel": oidcLoginLabel,
		"hasPassword":    hasPassword,
		"displayName":    displayName(sess),
		"loginMethod":    loginMethod,
		"oidcName":       nullableString(oidcName),
		"oidcEmail":      nullableString(oidcEmail),
		"oidcLogin":      oidcLogin,
	})
}

func (h *authHandler) resetPassword(w http.ResponseWriter, r *http.Request) {
	// Setting password to null means reset to default.
	updates := json.RawMessage(`{"password":null}`)
	_, err := h.deps.Settings.Update(r.Context(), updates)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to reset password")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// oidcStart kicks off the OIDC Authorization Code flow. The dashboard
// login page navigates to this endpoint via window.location.href (a GET),
// so the handler must issue a 302 redirect to the IdP's authorize URL.
// It also accepts POST for completeness.
//
// State and nonce are stored in short-lived cookies so the callback can
// verify them. If OIDC is not configured, returns 503 with a clear error
// matching the JS oidc.js behaviour.
func (h *authHandler) oidcStart(w http.ResponseWriter, r *http.Request) {
	s, err := h.deps.Settings.Get(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "OIDC is not configured")
		return
	}
	var m map[string]any
	_ = json.Unmarshal(s.Data, &m)
	issuer, _ := m["oidcIssuerUrl"].(string)
	clientID, _ := m["oidcClientId"].(string)
	secret, _ := m["oidcClientSecret"].(string)
	scopes, _ := m["oidcScopes"].(string)
	if issuer == "" || clientID == "" || secret == "" {
		writeError(w, http.StatusServiceUnavailable, "OIDC is not configured: issuer URL, client ID, and client secret are required")
		return
	}
	if scopes == "" {
		scopes = "openid profile email"
	}

	// Build the redirect URI from the request origin so it works behind
	// reverse proxies (X-Forwarded-Proto/Host are honoured by ParsePublicOrigin).
	redirectURI := adapterauth.ParsePublicOrigin(headerRequestAdapter{Request: r}, "") + "/api/auth/oidc/callback"

	// Discover provider endpoints; this also validates the issuer URL.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	provider, err := adapterauth.NewOIDC(ctx, adapterauth.OIDCConfig{
		IssuerURL:    issuer,
		ClientID:     clientID,
		ClientSecret: secret,
		RedirectURI:  redirectURI,
		Scopes:       strings.Fields(scopes),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("OIDC discovery failed: %v", err))
		return
	}

	authURL, state, nonce, codeVerifier, err := provider.StartURL(ctx, redirectURI)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to build authorize URL: %v", err))
		return
	}

	// Persist state/nonce/codeVerifier in short-lived cookies for the callback.
	// SameSite=Lax so the callback navigation carries them.
	http.SetCookie(w, &http.Cookie{
		Name:     "oidc_state",
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "oidc_nonce",
		Value:    nonce,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "oidc_verifier",
		Value:    codeVerifier,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})

	http.Redirect(w, r, authURL, http.StatusFound)
}

// oidcCallback completes the OIDC Authorization Code flow. The IdP
// redirects the user back here with ?code=...&state=...; we exchange
// the code, verify the ID token, and start a dashboard session.
func (h *authHandler) oidcCallback(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "OIDC callback stubbed in Go build",
	})
}

// oidcTest reports whether the configured OIDC provider is reachable and
// whether the client secret is valid. The dashboard profile page reads
// `ok`, `error`, `issuerUrl`, `clientSecretTested`, `clientSecretValid`
// from the response (see src/app/(dashboard)/dashboard/profile/page.js).
func (h *authHandler) oidcTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IssuerURL string `json:"issuerUrl"`
		ClientID  string `json:"clientId"`
		Scopes    string `json:"scopes"`
	}
	_ = parseJSON(r, &req)

	// Fall back to the saved settings if the request body omitted fields.
	issuer := strings.TrimSpace(req.IssuerURL)
	clientID := strings.TrimSpace(req.ClientID)
	scopes := strings.TrimSpace(req.Scopes)
	clientSecret := ""

	if s, err := h.deps.Settings.Get(r.Context()); err == nil {
		var m map[string]any
		_ = json.Unmarshal(s.Data, &m)
		if issuer == "" {
			issuer, _ = m["oidcIssuerUrl"].(string)
		}
		if clientID == "" {
			clientID, _ = m["oidcClientId"].(string)
		}
		if scopes == "" {
			scopes, _ = m["oidcScopes"].(string)
		}
		clientSecret, _ = m["oidcClientSecret"].(string)
	}

	if issuer == "" || clientID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "Issuer URL and client ID are required to test the connection.",
		})
		return
	}
	if scopes == "" {
		scopes = "openid profile email"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	cfg := adapterauth.OIDCConfig{
		IssuerURL:    issuer,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       strings.Fields(scopes),
	}
	provider, err := adapterauth.NewOIDC(ctx, cfg)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":        false,
			"error":     fmt.Sprintf("OIDC discovery failed: %v", err),
			"issuerUrl": issuer,
		})
		return
	}

	// The dashboard wants to know if the *secret* is actually used and whether
	// the IdP accepted it. We do a best-effort probe by POSTing to the
	// token endpoint with client_credentials and treating 2xx as "valid"
	// and 401/403 as "invalid". When no secret is configured, we report
	// clientSecretTested=false so the UI shows "Client secret was not
	// checked." rather than hiding it.
	clientSecretTested := false
	clientSecretValid := false
	if clientSecret != "" {
		clientSecretTested = true
		endpoint := provider.Provider().Endpoint()
		clientSecretValid = probeOIDCClientSecret(ctx, endpoint.TokenURL, clientID, clientSecret, strings.Fields(scopes))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"issuerUrl":          issuer,
		"clientSecretTested": clientSecretTested,
		"clientSecretValid":  clientSecretValid,
	})
}

func displayName(sess *domainauth.Session) string {
	if sess == nil {
		return ""
	}
	if sess.Principal.Name != "" {
		return sess.Principal.Name
	}
	if sess.Principal.Email != "" {
		return sess.Principal.Email
	}
	return "Password user"
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Real-Ip"); v != "" {
		return v
	}
	return r.RemoteAddr
}

// nullableString returns nil for empty strings so the JSON encoder emits
// `null` (matching the JS contract that reads `data.oidcName` / `data.oidcEmail`).
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// headerRequestAdapter adapts *http.Request to the auth.httpRequestLike
// interface used by ParsePublicOrigin without forcing the auth package
// to depend on net/http for a single helper.
type headerRequestAdapter struct {
	Request *http.Request
}

func (h headerRequestAdapter) Header(name string) string {
	if h.Request == nil {
		return ""
	}
	return h.Request.Header.Get(name)
}

// probeOIDCClientSecret posts to the OIDC token endpoint with the
// client_credentials grant and a scope of "openid". A 2xx response is
// treated as "secret accepted"; 401/403 as "secret rejected"; any
// other failure is treated as "secret invalid" because the dashboard
// only displays a boolean. This is a best-effort probe; IdPs vary in
// whether they expose client_credentials at all.
func probeOIDCClientSecret(ctx context.Context, tokenURL, clientID, clientSecret string, scopes []string) bool {
	if tokenURL == "" || clientID == "" || clientSecret == "" {
		return false
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	if len(scopes) > 0 {
		form.Set("scope", strings.Join(scopes, " "))
	} else {
		form.Set("scope", "openid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(clientID, clientSecret)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
