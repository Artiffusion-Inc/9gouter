// Package api implements the dashboard /api routes for the Go rewrite.
//
// It is organised by functional area. Each area has a Register* function that
// mounts its routes on a *http.ServeMux using Go 1.22+ method patterns. The
// public routes are exempt from session auth in the transport middleware via
// IsPublicRoute.
package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	domainauth "github.com/Artiffusion-Inc/9gouter/internal/domain/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/managedashboard"
)

// publicRoutes lists /api paths that bypass the session auth requirement.
// Keep them sorted longest-first so a concrete route is matched before its
// prefix sibling (e.g. /api/settings/require-login before /api/settings).
var publicRoutes = []string{
	"/api/auth/login",
	"/api/auth/logout",
	"/api/auth/oidc/",
	"/api/auth/reset-password",
	"/api/auth/status",
	"/api/health",
	"/api/init",
	"/api/locale",
	"/api/settings/require-login",
	"/api/tags",
	"/api/version",
	"/api/version/shutdown",
	"/api/version/update",
}

// alwaysProtectedRoutes lists /api paths that require a valid session even
// when settings.requireLogin is false. They carry destructive or secret
// surface (shutdown, DB backup import/export, OAuth auto-import) and must not
// be opened by the requireLogin=false gate. Mirrors the legacy
// dashboardGuard.js ALWAYS_PROTECTED list. Keep sorted longest-first.
//
// Note: some of these (/api/version/shutdown, /api/version/update) are also in
// publicRoutes; that is a pre-existing quirk of the JS contract (they were
// listed in both) and is outside the scope of T020/#205 — leave them as-is.
var alwaysProtectedRoutes = []string{
	"/api/oauth/cursor/auto-import",
	"/api/oauth/kiro/auto-import",
	"/api/settings/database",
	"/api/shutdown",
	"/api/version/shutdown",
	"/api/version/update",
}

// IsAlwaysProtected reports whether path must carry a valid session even when
// settings.requireLogin is false (mirrors dashboardGuard.js ALWAYS_PROTECTED).
func IsAlwaysProtected(path string) bool {
	for _, prefix := range alwaysProtectedRoutes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// IsPublicRoute reports whether path is a public /api route. The comparison is
// prefix-based, which matches the static Next.js public routes and any nested
// OIDC paths.
func IsPublicRoute(path string) bool {
	for _, prefix := range publicRoutes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// Deps holds all dependencies required by dashboard API handlers.
type Deps struct {
	APIKeys        *repo.APIKeyRepo
	Alias          *repo.AliasRepo
	Combos         *repo.ComboRepo
	Connections    *repo.ConnectionRepo
	DisabledModels *repo.DisabledModelsRepo
	Nodes          *repo.NodeRepo
	Pricing        *repo.PricingRepo
	ProxyPools     *repo.ProxyPoolRepo
	RequestDetails *repo.RequestDetailRepo
	Settings       *repo.SettingsRepo
	Usage          *repo.UsageRepo
	UsageTracker   *managedashboard.EventTracker
	SessionStore   domainauth.Store
	Logger         *slog.Logger

	// DB is the raw *sql.DB used by the backup import/export handler for bulk
	// writes that mirror the legacy importDb() transaction (settings/database).
	DB *sql.DB

	// Version is injected by the composition root; defaults to "dev" if empty.
	Version string

	// V1Dispatch, when set, dispatches a request whose URL.Path has been
	// rewritten to a /v1/* path through the real client-facing v1 handler
	// (registered by httptransport.RegisterV1). The dashboard /api/v1/*
	// passthrough routes use it to alias the implemented /v1/* endpoints
	// without re-implementing them. nil leaves the passthrough routes as
	// not-available stubs.
	V1Dispatch func(http.ResponseWriter, *http.Request)

	// ProxyOpts carries the proxy-stack timeout/behaviour config used by the
	// proxy-aware usage handlers (codex reset-credits GET/POST) to route their
	// upstream calls through the same proxy pipeline as the chat path
	// (proxy.ProxyAwareFetch). Zero-value when unset → those handlers fall back
	// to a plain timeout client (the pre-#154 behaviour).
	ProxyOpts proxy.Options

	// ResetComboRotation, when set, is called by the combos API on
	// create/update/delete so the round-robin cursor honors the new model list
	// immediately instead of carrying a stale index. nil → no-op (rotation
	// state only self-corrects once the cursor wraps past the new length).
	ResetComboRotation func(comboName string)
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

// parseJSON parses the request body into v.
func parseJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// parseOptionalJSON decodes the body into v, returning nil on empty body.
func parseOptionalJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

// jsonString returns a JSON string from a map, or the fallback.
func jsonString(m map[string]any, key, fallback string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

// jsonBool returns a JSON bool from a map, or the fallback.
func jsonBool(m map[string]any, key string, fallback bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return fallback
}

// stringsTrim trims a string, returning the zero value for non-strings.
func stringsTrim(v string) string { return strings.TrimSpace(v) }

// queryOptionalBool parses an optional boolean query param ("true"/"false").
func queryOptionalBool(r *http.Request, key string) *bool {
	switch r.URL.Query().Get(key) {
	case "true":
		return boolPtr(true)
	case "false":
		return boolPtr(false)
	}
	return nil
}

// hasField reports whether the JSON request body contains key.
func hasField(r *http.Request, key string) bool {
	if r.Body == nil {
		return false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	_ = r.Body.Close()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	_, ok := m[key]
	r.Body = io.NopCloser(bytes.NewReader(body))
	return ok
}

// nowISO returns the current UTC time as a JS-style ISO string.
func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// generateID returns a short random id string. Tests may override this.
var generateID = func() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// boolPtr returns a pointer to b.
func boolPtr(b bool) *bool { return &b }
