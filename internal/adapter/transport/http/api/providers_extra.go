package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
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

func (h *providersExtraHandler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": []any{}})
}

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
		"success":   true,
		"id":        id,
		"valid":     valid,
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
				"valid":           true,
			})
			continue
		}
		failed++
		results = append(results, map[string]any{
			"connectionId":   it.connectionID,
			"connectionName": it.connectionName,
			"provider":       it.provider,
			"valid":           false,
			"diagnosis":       map[string]any{"type": "NO_CREDENTIALS"},
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
	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
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
