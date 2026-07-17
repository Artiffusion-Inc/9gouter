package api

import (
	"encoding/json"
	"errors"
	"net/http"

	adapterauth "github.com/Artiffusion-Inc/9router/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	usecaseauth "github.com/Artiffusion-Inc/9router/internal/usecase/auth"
)

// RegisterAuth mounts the public auth routes on mux.
// These routes are not protected by APIMiddleware.
func RegisterAuth(mux *http.ServeMux, deps Deps, cfg config.Config) {
	h := newAuthHandler(deps, cfg)
	mux.HandleFunc("POST /api/auth/login", h.login)
	mux.HandleFunc("POST /api/auth/logout", h.logout)
	mux.HandleFunc("GET /api/auth/status", h.status)
	mux.HandleFunc("POST /api/auth/reset-password", h.resetPassword)
}

type authHandler struct {
	deps Deps
	uc   *usecaseauth.UseCase
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
		verifier = &usecaseauth.BcryptVerifier{Hash: cfg.DashboardPasswordHash, Comparator: bcryptCompareStub}
	} else {
		verifier = &usecaseauth.PlainVerifier{InitialPassword: "123456"}
	}
	return &authHandler{
		deps: deps,
		uc:   usecaseauth.New(deps.SessionStore, adapterauth.NewLoginLimiter(), verifier, 0),
	}
}

func bcryptCompareStub(_, _ string) error { return errors.New("not implemented in tests") }

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
	if err == nil {
		var m map[string]any
		_ = json.Unmarshal(settings.Data, &m)
		requireLogin = jsonBool(m, "requireLogin", true)
	}

	sess, _ := h.deps.SessionStore.Get(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"requireLogin":   requireLogin,
		"authMode":       "password",
		"oidcConfigured": false,
		"oidcLoginLabel": "Sign in with OIDC",
		"hasPassword":    true,
		"displayName":    displayName(sess),
		"loginMethod":    "Password",
		"oidcName":       nil,
		"oidcEmail":      nil,
		"oidcLogin":      false,
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

func displayName(sess any) string {
	if s, ok := sess.(interface{ Name() string }); ok {
		return s.Name()
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
