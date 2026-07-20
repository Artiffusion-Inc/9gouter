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
	domainauth "github.com/Artiffusion-Inc/9gouter/internal/domain/auth"
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

// intPtr returns a pointer to n.
func intPtr(n int) *int { return &n }

// floatPtr returns a pointer to f.
func floatPtr(f float64) *float64 { return &f }
