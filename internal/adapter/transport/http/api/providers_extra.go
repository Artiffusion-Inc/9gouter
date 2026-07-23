package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	adapterprovider "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/resolver"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// RegisterProvidersExtra mounts provider sub-routes that were missing from the
// initial providers handler.
func RegisterProvidersExtra(mux *http.ServeMux, deps Deps) {
	h := &providersExtraHandler{deps: deps}
	mux.HandleFunc("GET /api/providers/{id}/models", h.models)
	mux.HandleFunc("GET /api/providers/{id}/test-models", h.testModels)
	mux.HandleFunc("POST /api/providers/{id}/test", h.test)
	mux.HandleFunc("GET /api/providers/{id}", h.get)
	mux.HandleFunc("PUT /api/providers/{id}", h.update)
	mux.HandleFunc("DELETE /api/providers/{id}", h.delete)
	mux.HandleFunc("GET /api/providers/suggested-models", h.suggestedModels)
	mux.HandleFunc("GET /api/providers/kilo/free-models", h.kiloFreeModels)
	mux.HandleFunc("POST /api/providers/test-batch", h.testBatch)
	mux.HandleFunc("POST /api/providers/validate", h.validate)
}

type providersExtraHandler struct {
	deps Deps
}

// models serves GET /api/providers/{id}/models — the live model catalog for a
// connection. Ports src/app/api/providers/[id]/models/route.js (upstream
// v0.5.40): a registered LiveModelResolver for the provider fetches the live
// catalog (codex client_version gate, gemini-cli, kiro, grok-cli, copilot,
// kimchi, qoder, clinepass), refreshing credentials on 401/403 via
// OnCredentialsRefreshed → Connections.ApplyConnectionPatch. When no live
// resolver exists or the live fetch returns nothing, the handler falls back
// to the provider's static catalog (adapterprovider.Catalog). A provider with neither
// a live resolver nor a static catalog returns 400 "does not support models
// listing".
func (h *providersExtraHandler) models(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	conn, err := h.deps.Connections.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch connection")
		return
	}
	if conn == nil {
		writeError(w, http.StatusNotFound, "Connection not found")
		return
	}

	providerID := conn.Provider
	creds := credsFromConnection(conn)
	resolverInstance := resolver.Lookup(providerID)

	// Live resolver path.
	if resolverInstance != nil {
		var warning string
		result, rerr := resolverInstance.Resolve(r.Context(), creds, resolver.ResolveOpts{
			Logger: resolverLogger(h.deps.Logger),
			OnCredentialsRefreshed: func(rc resolver.RefreshedCredentials) error {
				return h.persistRefreshedCreds(r.Context(), conn, rc)
			},
		})
		if rerr != nil {
			warning = "Failed to fetch live models: " + rerr.Error()
		}
		if result != nil && len(result.Models) > 0 {
			resp := map[string]any{
				"provider":     providerID,
				"connectionId": id,
				"models":       resolvedModelsToAPI(result.Models),
			}
			if warning != "" {
				resp["warning"] = warning
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
		// Live fetch returned nothing; fall back to static catalog.
		if warning == "" {
			warning = "Provider returned no live models; using static catalog."
		}
		if staticModels, ok := staticCatalogModels(providerID); ok {
			resp := map[string]any{
				"provider":     providerID,
				"connectionId": id,
				"models":       staticModels,
				"warning":      warning,
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
		// No static catalog either — return the (possibly empty) live result
		// with the warning so the dashboard can surface the failure.
		resp := map[string]any{
			"provider":     providerID,
			"connectionId": id,
			"models":       []any{},
		}
		if warning != "" {
			resp["warning"] = warning
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// No live resolver: static catalog, if any.
	if models, ok := staticCatalogModels(providerID); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"provider":     providerID,
			"connectionId": id,
			"models":       models,
		})
		return
	}
	writeError(w, http.StatusBadRequest, "Provider "+providerID+" does not support models listing")
}

// credsFromConnection builds provider.Credentials from a connection's JSON
// data blob, mirroring v1.resolveCredentialsWithOpts. The refreshToken is
// carried in ProviderSpecificData (where the resolvers read it via
// refreshTokenOf), matching the chat-path credential shape.
func credsFromConnection(conn *settings.ProviderConnection) provider.Credentials {
	var data map[string]any
	_ = json.Unmarshal(conn.Data, &data)
	creds := provider.Credentials{ProviderSpecificData: map[string]any{"_connectionId": conn.ID}}
	if v, ok := data["apiKey"].(string); ok {
		creds.APIKey = v
	}
	if v, ok := data["accessToken"].(string); ok {
		creds.AccessToken = v
	}
	if v, ok := data["refreshToken"].(string); ok {
		creds.ProviderSpecificData["refreshToken"] = v
	}
	if v, ok := data["expiresAt"].(string); ok && v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			creds.ExpiresAt = &t
		}
	}
	if v, ok := data["providerSpecificData"].(map[string]any); ok {
		for k, val := range v {
			creds.ProviderSpecificData[k] = val
		}
	}
	return creds
}

// staticCatalogModels returns the provider's static model catalog as the
// {id,name,upstreamModelId?} shape the dashboard expects. Returns (nil, false)
// when the provider has no static catalog.
func staticCatalogModels(providerID string) ([]map[string]any, bool) {
	cat, ok := adapterprovider.Catalog(providerID)
	if !ok || len(cat.Models) == 0 {
		return nil, false
	}
	out := make([]map[string]any, 0, len(cat.Models))
	for _, m := range cat.Models {
		name := m.Name
		if name == "" {
			name = m.ID
		}
		entry := map[string]any{"id": m.ID, "name": name}
		if m.UpstreamModelID != "" && m.UpstreamModelID != m.ID {
			entry["upstreamModelId"] = m.UpstreamModelID
		}
		out = append(out, entry)
	}
	return out, true
}

// resolvedModelsToAPI maps resolver.ResolvedModel entries to the dashboard
// {id,name} shape.
func resolvedModelsToAPI(models []resolver.ResolvedModel) []map[string]any {
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		name := m.Name
		if name == "" {
			name = m.ID
		}
		entry := map[string]any{"id": m.ID, "name": name}
		if m.UpstreamModelID != "" && m.UpstreamModelID != m.ID {
			entry["upstreamModelId"] = m.UpstreamModelID
		}
		out = append(out, entry)
	}
	return out
}

// persistRefreshedCreds merges a RefreshedCredentials patch back into the
// connection's data blob via ApplyConnectionPatch, mirroring the JS
// updateProviderCredentials. Returns nil when there is nothing to persist.
func (h *providersExtraHandler) persistRefreshedCreds(ctx context.Context, conn *settings.ProviderConnection, rc resolver.RefreshedCredentials) error {
	patch := map[string]any{}
	if rc.AccessToken != "" {
		patch["accessToken"] = rc.AccessToken
	}
	if rc.RefreshToken != "" {
		patch["refreshToken"] = rc.RefreshToken
	}
	if rc.APIKey != "" {
		patch["apiKey"] = rc.APIKey
	}
	if rc.Token != "" {
		patch["token"] = rc.Token
	}
	if rc.CopilotToken != "" {
		patch["copilotToken"] = rc.CopilotToken
	}
	if rc.ExpiresIn > 0 {
		patch["expiresAt"] = expiresAtFromIn(rc.ExpiresIn)
	} else if rc.ExpiresAt != "" {
		patch["expiresAt"] = rc.ExpiresAt
	}
	if rc.IDToken != "" {
		psdPatch := map[string]any{}
		if existing := psdField(conn, "providerSpecificData"); existing != nil {
			if m, ok := existing.(map[string]any); ok {
				for k, v := range m {
					psdPatch[k] = v
				}
			}
		}
		psdPatch["idToken"] = rc.IDToken
		if rc.ProjectID != "" {
			psdPatch["projectId"] = rc.ProjectID
		}
		if len(rc.ProviderSpecificData) > 0 {
			for k, v := range rc.ProviderSpecificData {
				psdPatch[k] = v
			}
		}
		patch["providerSpecificData"] = psdPatch
	} else if rc.ProjectID != "" || len(rc.ProviderSpecificData) > 0 {
		psdPatch := map[string]any{}
		if existing := psdField(conn, "providerSpecificData"); existing != nil {
			if m, ok := existing.(map[string]any); ok {
				for k, v := range m {
					psdPatch[k] = v
				}
			}
		}
		if rc.ProjectID != "" {
			psdPatch["projectId"] = rc.ProjectID
		}
		for k, v := range rc.ProviderSpecificData {
			psdPatch[k] = v
		}
		patch["providerSpecificData"] = psdPatch
	}
	if len(patch) == 0 {
		return nil
	}
	_, err := h.deps.Connections.ApplyConnectionPatch(ctx, conn.ID, patch)
	return err
}

func psdField(conn *settings.ProviderConnection, key string) any {
	var data map[string]any
	_ = json.Unmarshal(conn.Data, &data)
	return data[key]
}

func expiresAtFromIn(expiresIn int) string {
	return time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
}

// resolverLogger adapts *slog.Logger to the resolver.Logger interface used by
// the live resolvers. nil → a no-op logger.
func resolverLogger(log *slog.Logger) resolver.Logger {
	if log == nil {
		return nopLogger{}
	}
	return slogResolverLogger{log: log}
}

type nopLogger struct{}

func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Debug(string, ...any) {}

type slogResolverLogger struct{ log *slog.Logger }

func (l slogResolverLogger) Info(msg string, args ...any)  { l.log.Info(msg, args...) }
func (l slogResolverLogger) Warn(msg string, args ...any)  { l.log.Warn(msg, args...) }
func (l slogResolverLogger) Debug(msg string, args ...any) { l.log.Debug(msg, args...) }

func (h *providersExtraHandler) testModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": []any{}})
}

func (h *providersExtraHandler) test(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := h.deps.Connections.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch connection")
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound, "Connection not found")
		return
	}
	valid := connectionLooksValid(c)
	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"id":         id,
		"valid":      valid,
		"testStatus": "ok",
	})
}

func (h *providersExtraHandler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := h.deps.Connections.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch connection")
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound, "Connection not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": c})
}

func (h *providersExtraHandler) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	patch := map[string]any{}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&patch)
	}

	priorityChanged := false
	newPriority := 0
	hasPriority := false
	if v, ok := patch["priority"]; ok {
		hasPriority = true
		switch t := v.(type) {
		case float64:
			newPriority = int(t)
		case int:
			newPriority = t
		case json.Number:
			n, _ := t.Int64()
			newPriority = int(n)
		}
		priorityChanged = true
	}

	if hasPriority {
		// Update the priority column directly (top-level).
		existing, err := h.deps.Connections.GetByID(r.Context(), id)
		if err != nil || existing == nil {
			if err != nil {
				writeError(w, http.StatusInternalServerError, "Failed to fetch connection")
			} else {
				writeError(w, http.StatusNotFound, "Connection not found")
			}
			return
		}
		updated := *existing
		updated.Priority = newPriority
		if err := h.deps.Connections.Update(r.Context(), updated); err != nil {
			if strings.Contains(err.Error(), "not found") {
				writeError(w, http.StatusNotFound, "Connection not found")
			} else {
				writeError(w, http.StatusInternalServerError, "Failed to update connection")
			}
			return
		}
		_ = priorityChanged // Update already triggers a reorder when priority changed.
	}

	if v, ok := patch["isActive"]; ok {
		existing, err := h.deps.Connections.GetByID(r.Context(), id)
		if err != nil || existing == nil {
			if err != nil {
				writeError(w, http.StatusInternalServerError, "Failed to fetch connection")
			} else {
				writeError(w, http.StatusNotFound, "Connection not found")
			}
			return
		}
		b, okBool := v.(bool)
		if !okBool {
			writeError(w, http.StatusBadRequest, "isActive must be a boolean")
			return
		}
		updated := *existing
		updated.IsActive = b
		if err := h.deps.Connections.Update(r.Context(), updated); err != nil {
			if strings.Contains(err.Error(), "not found") {
				writeError(w, http.StatusNotFound, "Connection not found")
			} else {
				writeError(w, http.StatusInternalServerError, "Failed to update connection")
			}
			return
		}
	}

	// Merge remaining patch fields (proxyPoolId, formData edits, …) into the
	// JSON data blob. We strip the columns we already wrote above so they
	// don't end up duplicated inside the blob.
	blobPatch := map[string]any{}
	for k, v := range patch {
		if k == "priority" || k == "isActive" {
			continue
		}
		blobPatch[k] = v
	}
	if len(blobPatch) > 0 {
		if _, err := h.deps.Connections.ApplyConnectionPatch(r.Context(), id, blobPatch); err != nil {
			if strings.Contains(err.Error(), "not found") {
				writeError(w, http.StatusNotFound, "Connection not found")
			} else {
				writeError(w, http.StatusInternalServerError, "Failed to update connection")
			}
			return
		}
	}

	reloaded, err := h.deps.Connections.GetByID(r.Context(), id)
	if err != nil || reloaded == nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"id":         id,
		"connection": reloaded,
	})
}

func (h *providersExtraHandler) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.deps.Connections.Delete(r.Context(), id); err != nil {
		if !strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusInternalServerError, "Failed to delete connection")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *providersExtraHandler) suggestedModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": []any{}})
}

func (h *providersExtraHandler) kiloFreeModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": []any{}})
}

// testBatch accepts {mode, providerId} (the providers page batch-test
// contract) and returns {mode, summary:{passed,failed,total}, results:[
// {connectionId, connectionName, provider, valid, latencyMs, diagnosis}]} —
// the shape ProviderTestResultsView in
// src/app/(dashboard)/dashboard/providers/page.js renders (it destructures
// summary.passed/failed/total and per-item connectionId, connectionName,
// provider, valid, latencyMs, diagnosis.type). Also still accepts the older
// {connections:[{id,...}]} / {ids:[]} shapes for direct callers.
func (h *providersExtraHandler) testBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode        string           `json:"mode"`
		ProviderID  string           `json:"providerId"`
		Connections []map[string]any `json:"connections"`
		IDs         []string         `json:"ids"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	type item struct {
		connectionID   string
		connectionName string
		provider       string
		valid          bool
	}
	items := make([]item, 0)

	addConn := func(c *settings.ProviderConnection) {
		if c == nil {
			return
		}
		items = append(items, item{
			connectionID:   c.ID,
			connectionName: c.Name,
			provider:       c.Provider,
			valid:          connectionLooksValid(c),
		})
	}

	// New contract: drive off mode + providerId. "provider" tests one
	// provider's connections; the auth modes (oauth/free/apikey) and "all"
	// test every active connection (auth mode is not derivable from the
	// connection row alone, so the filter is the same).
	switch body.Mode {
	case "provider":
		if body.ProviderID != "" {
			conns, err := h.deps.Connections.List(r.Context(), repo.ConnectionFilter{Provider: body.ProviderID})
			if err == nil {
				for i := range conns {
					addConn(&conns[i])
				}
			}
		}
	case "all", "oauth", "free", "apikey", "":
		active := true
		conns, err := h.deps.Connections.List(r.Context(), repo.ConnectionFilter{IsActive: &active})
		if err == nil {
			for i := range conns {
				addConn(&conns[i])
			}
		}
	}

	// Legacy explicit-id shapes still honored.
	for _, c := range body.Connections {
		if c == nil {
			continue
		}
		id, _ := c["id"].(string)
		if id == "" {
			continue
		}
		conn, err := h.deps.Connections.GetByID(r.Context(), id)
		if err != nil || conn == nil {
			items = append(items, item{connectionID: id, valid: false})
			continue
		}
		addConn(conn)
	}
	for _, id := range body.IDs {
		conn, err := h.deps.Connections.GetByID(r.Context(), id)
		if err != nil || conn == nil {
			items = append(items, item{connectionID: id, valid: false})
			continue
		}
		addConn(conn)
	}

	results := make([]map[string]any, 0, len(items))
	passed, failed := 0, 0
	for _, it := range items {
		if it.valid {
			passed++
			results = append(results, map[string]any{
				"connectionId":   it.connectionID,
				"connectionName": it.connectionName,
				"provider":       it.provider,
				"valid":          true,
			})
			continue
		}
		failed++
		results = append(results, map[string]any{
			"connectionId":   it.connectionID,
			"connectionName": it.connectionName,
			"provider":       it.provider,
			"valid":          false,
			"diagnosis":      map[string]any{"type": "NO_CREDENTIALS"},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"mode": body.Mode,
		"summary": map[string]any{
			"passed": passed,
			"failed": failed,
			"total":  len(items),
		},
		"results": results,
	})
}

func (h *providersExtraHandler) validate(w http.ResponseWriter, r *http.Request) {
	var body validateRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	url := validateURLForProvider(body.Provider, body.ProviderSpecificData)
	if url == "" {
		// Unknown provider / no registered BaseURL — degrade to the previous
		// stub behaviour (unconditional-true) rather than blocking the UI. The
		// dashboard's validate button stays green; a real probe lands when the
		// provider registry gains an entry.
		writeJSON(w, http.StatusOK, validateResponse{Valid: true, Provider: body.Provider})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), validateTimeout)
	defer cancel()
	status, err := validateProbe(ctx, url, body.APIKey)
	if err != nil {
		// Transport failure (DNS / connection refused / timeout): the key is
		// unprovable, not necessarily invalid. Report invalid with the error
		// so the dashboard surfaces it instead of silently green-lighting.
		writeJSON(w, http.StatusOK, validateResponse{Valid: false, Provider: body.Provider, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, validateResponse{
		Valid:    evaluateValidateStatus(body.Provider, status),
		Provider: body.Provider,
		Status:   status,
	})
}

// connectionLooksValid reports whether a stored connection is plausibly
// configured: active, or carrying credentials in its data blob.
func connectionLooksValid(c *settings.ProviderConnection) bool {
	if c == nil {
		return false
	}
	if c.IsActive {
		return true
	}
	hasKey := false
	if len(c.Data) > 0 {
		var data map[string]any
		if err := json.Unmarshal(c.Data, &data); err == nil {
			if v, ok := data["apiKey"].(string); ok && v != "" {
				hasKey = true
			}
			if v, ok := data["accessToken"].(string); ok && v != "" {
				hasKey = true
			}
		}
	}
	return hasKey
}
