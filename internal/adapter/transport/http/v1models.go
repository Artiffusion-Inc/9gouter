package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9router/internal/adapter/provider"
)

// GET /v1/models — OpenAI-compatible model catalog.
//
// This is the static MVP for upstream issue decolua/9router #2702
// (Endpoint /v1/models not list all the models). It mirrors the
// no-live-resolver branch of the JS buildModelsList in
// src/app/api/v1/models/route.js:
//
//   - combos first (filtered by kind)
//   - per-provider static catalogs, only for providers with an active
//     connection (mirrors JS activeConnectionByProvider)
//   - custom models merged in
//   - alias map applied (alias -> resolved model id)
//   - disabled models removed
//   - filtered by service kind (path param /v1/models/{kind}, default "llm")
//   - prefixed as "<alias>/<modelId>" (JS outputAlias convention)
//
// NOT YET PORTED (tracked as follow-up Worker tasks):
//   - live-model resolvers (kiro/qoder/kimchi/copilot/clinepass/grok-cli) —
//     this is why the list does not update when thinking mode changes (#2702)
//   - fetchCompatibleModelIds for openai/anthropic-compatible providers
//   - capabilities metadata
//
// The handler is session-agnostic on purpose: the JS /v1/models route did
// not require a dashboard session, but it DID honor requireApiKey (the same
// gate as /v1/chat). We reuse the same gate.

// modelKind defaults to "llm" and is the OpenAI-compatible object kind. The
// JS MODEL_TYPE_TO_KIND maps per-model `type` to service kind; the Go
// catalog stores Kind directly on domain.Model (empty => "llm").
func modelKind(k string) string {
	if k == "" {
		return "llm"
	}
	return k
}

// kindFilterFromPath returns the requested service kinds for a /v1/models
// or /v1/models/{kind} request. Empty kind => ["llm"] (the OpenAI default —
// chat models only). Special kind "all" returns every kind.
func kindFilterFromPath(kind string) []string {
	kind = strings.TrimSpace(strings.ToLower(kind))
	if kind == "" || kind == "llm" {
		return []string{"llm"}
	}
	if kind == "all" {
		return nil // nil means "no filter"
	}
	return []string{kind}
}

// oaiModel is the OpenAI /v1/models object entry.
type oaiModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
	Kind    string `json:"kind,omitempty"`
}

func (h *v1Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// API-key gate, identical to /v1/chat (dashboardGuard.js + auth.js).
	apiKey := extractAPIKey(r)
	requireKey, err := h.requireAPIKey(ctx)
	if err != nil {
		h.logger.Warn("api-key check failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "Auth check failed")
		return
	}
	if requireKey || !isLocalRequest(r) {
		if apiKey == "" {
			h.writeError(w, http.StatusUnauthorized, "Missing API key")
			return
		}
		valid, err := h.deps.APIKeysRepo.Validate(ctx, apiKey)
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, "Auth check failed")
			return
		}
		if !valid {
			h.writeError(w, http.StatusUnauthorized, "Invalid API key")
			return
		}
	}

	kindFilter := kindFilterFromPath(r.PathValue("kind"))

	models := h.buildModelsList(ctx, kindFilter)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   models,
	})
}

// buildModelsList assembles the static model catalog. It is split out so it
// is unit-testable without HTTP machinery.
func (h *v1Handler) buildModelsList(ctx context.Context, kindFilter []string) []oaiModel {
	// Active connections, keyed by provider (first active wins, mirroring JS
	// activeConnectionByProvider).
	activeProviders := map[string]bool{}
	if h.deps.ConnectionRepo != nil {
		conns, err := h.deps.ConnectionRepo.List(ctx, repo.ConnectionFilter{IsActive: boolPtr(true)})
		if err == nil {
			for _, c := range conns {
				activeProviders[c.Provider] = true
			}
		}
	}

	// Disabled models: map[providerAlias][]modelID.
	disabled := map[string]map[string]bool{}
	if h.deps.DisabledModels != nil {
		if all, err := h.deps.DisabledModels.GetAll(ctx); err == nil {
			for alias, ids := range all {
				m := map[string]bool{}
				for _, id := range ids {
					m[id] = true
				}
				disabled[alias] = m
			}
		}
	}

	// Aliases: alias -> resolved "<provider>/<model>".
	aliases := map[string]string{}
	if h.deps.AliasRepo != nil {
		if a, err := h.deps.AliasRepo.GetAliases(ctx); err == nil {
			aliases = a
		}
	}

	// Custom models keyed by provider alias.
	customByAlias := map[string][]string{}
	if h.deps.AliasRepo != nil {
		if cms, err := h.deps.AliasRepo.GetCustomModels(ctx); err == nil {
			for _, cm := range cms {
				if cm.ID == "" {
					continue
				}
				customByAlias[cm.ProviderAlias] = append(customByAlias[cm.ProviderAlias], cm.ID)
			}
		}
	}

	out := []oaiModel{}
	seen := map[string]bool{}
	add := func(id, ownedBy, kind string) {
		mk := modelKind(kind)
		if kindFilter != nil {
			match := false
			for _, k := range kindFilter {
				if k == mk {
					match = true
					break
				}
			}
			if !match {
				return
			}
		}
		if seen[id] {
			return
		}
		seen[id] = true
		entry := oaiModel{ID: id, Object: "model", OwnedBy: ownedBy}
		if mk != "llm" {
			entry.Kind = mk
		}
		out = append(out, entry)
	}

	// Combos first (filtered by kind). Web combos expose `kind`.
	if h.deps.ComboRepo != nil {
		if combos, err := h.deps.ComboRepo.List(ctx); err == nil {
			for _, c := range combos {
				ck := modelKind(c.Kind)
				if kindFilter != nil {
					match := false
					for _, k := range kindFilter {
						if k == ck {
							match = true
							break
						}
					}
					if !match {
						continue
					}
				}
				id := c.Name
				if seen[id] {
					continue
				}
				seen[id] = true
				entry := oaiModel{ID: id, Object: "model", OwnedBy: "combo"}
				if ck == "webSearch" || ck == "webFetch" {
					entry.Kind = ck
				}
				out = append(out, entry)
			}
		}
	}

	// Per-provider static catalogs — only for providers with an active
	// connection (JS activeConnectionByProvider). The catalog's Alias is the
	// output prefix; a connection's providerSpecificData.prefix would override
	// it in JS, but we use the catalog alias (prefix override is a follow-up).
	for _, cat := range provider.AllCatalogs() {
		if !activeProviders[cat.ID] {
			continue
		}
		// Kind filter at the provider level (serviceKinds intersect kindFilter).
		if kindFilter != nil && !kindsIntersect(cat.ServiceKinds, kindFilter) {
			continue
		}
		alias := cat.Alias
		if alias == "" {
			alias = cat.ID
		}
		for _, m := range cat.Models {
			mk := modelKind(m.Kind)
			if kindFilter != nil && !containsStr(kindFilter, mk) {
				continue
			}
			if disabled[alias] != nil && disabled[alias][m.ID] {
				continue
			}
			add(alias+"/"+m.ID, alias, m.Kind)
		}
		// Custom models for this provider alias (LLM-only by current schema).
		if kindFilter == nil || containsStr(kindFilter, "llm") {
			for _, id := range customByAlias[alias] {
				if disabled[alias] != nil && disabled[alias][id] {
					continue
				}
				add(alias+"/"+id, alias, "llm")
			}
		}
	}

	// Aliases: each alias resolves to a "<provider>/<model>"; emit the alias
	// itself as a model id (the JS route exposes alias keys as catalog entries
	// the client can request by alias name).
	for alias, resolved := range aliases {
		if resolved == "" || !strings.Contains(resolved, "/") {
			continue
		}
		if seen[alias] {
			continue
		}
		// Inherit the kind of the resolved target if we can find it; default llm.
		kind := "llm"
		if kindFilter != nil && !containsStr(kindFilter, kind) {
			continue
		}
		_ = resolved
		seen[alias] = true
		out = append(out, oaiModel{ID: alias, Object: "model", OwnedBy: "alias"})
	}

	return out
}

func kindsIntersect(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}