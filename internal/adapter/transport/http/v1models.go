package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/capabilities"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/resolver"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
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
//
// PORTED (cluster #109): capabilities metadata is now populated per entry via
// capsForModel (capabilities.FromServiceKind for non-LLM, then
// capabilities.GetCapabilitiesForModel for LLMs) — upstream 2629218b.
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

// capsForModel resolves the capabilities to surface on /v1/models for a model
// entry, mirroring the JS route.js fallback chain (upstream 2629218b):
//
//	liveCapabilitiesById || capabilitiesFromServiceKind(kind)
//	  || (kind === LLM ? getCapabilitiesForModel(provider, id) : null)
//
// In Go the live resolver's synthetic variant flags (resolver.Capabilities)
// are not model capabilities, so the chain is: service-kind delta first (for
// non-LLM kinds like image/embedding/stt/tts), then, for LLMs, the shared
// capabilities table keyed on provider+model. Returns nil when neither applies
// (unknown LLM with no service-kind delta → the JS route omits the field).
func capsForModel(providerID, modelID, kind string) *capabilities.Capabilities {
	mk := modelKind(kind)
	if mk != "llm" {
		if c := capabilities.FromServiceKind(mk); c != nil {
			return c
		}
	}
	if mk == "llm" {
		c := capabilities.GetCapabilitiesForModel(providerID, modelID)
		// Only attach when the resolved entry carries real signal beyond the
		// bare Default floor (e.g. a known model with vision/reasoning/limits).
		// The Default floor alone is not useful to clients and would attach a
		// capabilities blob to every unknown model id, matching the JS route
		// which only sets model.capabilities when getCapabilitiesForModel
		// returns a non-null override.
		if hasCapabilitySignal(c) {
			return &c
		}
	}
	return nil
}

// hasCapabilitySignal reports whether a resolved Capabilities carries any
// non-default signal worth surfacing on /v1/models. The Default floor alone
// (Tools+ThinkingCanDisable true, 200k/64k limits, nothing else) is treated as
// "no signal" so unknown LLM ids do not get a redundant capabilities blob.
func hasCapabilitySignal(c capabilities.Capabilities) bool {
	return c.Vision || c.PDF || c.AudioInput || c.VideoInput ||
		c.ImageOutput || c.AudioOutput || c.Search || c.Reasoning ||
		c.ThinkingFormat != capabilities.ThinkingNone ||
		!c.ThinkingCanDisable ||
		c.ContextWindow != capabilities.Default.ContextWindow ||
		c.MaxOutput != capabilities.Default.MaxOutput
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
	ID           string                     `json:"id"`
	Object       string                     `json:"object"`
	OwnedBy      string                     `json:"owned_by"`
	Kind         string                     `json:"kind,omitempty"`
	Capabilities *capabilities.Capabilities `json:"capabilities,omitempty"`
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
	// activeConnectionByProvider). We keep the connection records too: the
	// live-model resolver needs the full credentials (accessToken + psd),
	// not just the provider id.
	activeProviders := map[string]bool{}
	var activeConns []settings.ProviderConnection
	if h.deps.ConnectionRepo != nil {
		conns, err := h.deps.ConnectionRepo.List(ctx, repo.ConnectionFilter{IsActive: boolPtr(true)})
		if err == nil {
			for _, c := range conns {
				if !activeProviders[c.Provider] {
					activeProviders[c.Provider] = true
					activeConns = append(activeConns, c)
				}
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
	add := func(id, ownedBy, kind string, caps *capabilities.Capabilities) {
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
		entry.Capabilities = caps
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
	//
	// Providers with a registered live-model resolver skip the static catalog
	// for their LLM entries — the live catalog takes precedence (mirrors the
	// JS LIVE_MODEL_RESOLVERS behavior, where /v1/models prefers the live
	// catalog for resolver-backed providers). On any live-resolve failure the
	// resolver returns nil and we fall back to the static catalog below.
	liveCatalogs := h.resolveLiveCatalogs(ctx, activeConns)

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
		// If a live catalog resolved for this provider, skip the static LLM
		// entries and emit the live ones instead (below). Non-LLM kinds still
		// come from the static catalog — live resolvers only serve LLM.
		hasLiveLLM := false
		if live, ok := liveCatalogs[cat.ID]; ok && live != nil {
			hasLiveLLM = true
		}
		for _, m := range cat.Models {
			mk := modelKind(m.Kind)
			if hasLiveLLM && mk == "llm" {
				continue
			}
			if kindFilter != nil && !containsStr(kindFilter, mk) {
				continue
			}
			if disabled[alias] != nil && disabled[alias][m.ID] {
				continue
			}
			add(alias+"/"+m.ID, alias, m.Kind, capsForModel(cat.ID, m.ID, m.Kind))
		}
		// Custom models for this provider alias (LLM-only by current schema).
		// Skip custom LLM models too when a live catalog is present.
		if (kindFilter == nil || containsStr(kindFilter, "llm")) && !hasLiveLLM {
			for _, id := range customByAlias[alias] {
				if disabled[alias] != nil && disabled[alias][id] {
					continue
				}
				add(alias+"/"+id, alias, "llm", capsForModel(cat.ID, id, "llm"))
			}
		}
	}

	// Live catalogs: emit their resolved models under the provider alias.
	// These run after the static loop so the `seen` set dedups any overlap,
	// but since we skipped static LLM entries for resolver-backed providers,
	// live LLM ids are fresh here.
	for providerID, live := range liveCatalogs {
		if live == nil {
			continue
		}
		alias := providerID
		if cat, ok := provider.Catalog(providerID); ok && cat.Alias != "" {
			alias = cat.Alias
		}
		if kindFilter != nil && !containsStr(kindFilter, "llm") {
			continue
		}
		for _, m := range live.Models {
			if disabled[alias] != nil && disabled[alias][m.ID] {
				continue
			}
			add(alias+"/"+m.ID, alias, "llm", capsForModel(providerID, m.ID, "llm"))
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

// resolveLiveCatalogs fetches the live model catalog for every active
// connection whose provider has a registered live-model resolver. Each
// resolver runs under a short per-call timeout (liveResolveTimeout) so a
// slow or dead upstream never blocks /v1/models. On any failure or timeout
// the resolver returns nil and is omitted from the map, so the caller falls
// back to the static catalog for that provider.
//
// Returns map[providerID]*resolver.Result; absent = no resolver / no
// active connection; nil value = resolver present but produced no catalog.
func (h *v1Handler) resolveLiveCatalogs(ctx context.Context, conns []settings.ProviderConnection) map[string]*resolver.Result {
	out := map[string]*resolver.Result{}
	if len(conns) == 0 {
		return out
	}
	for _, c := range conns {
		r := resolver.Lookup(c.Provider)
		if r == nil {
			continue
		}
		creds := connectionCredentials(c)
		// Per-call timeout so one dead upstream can't stall the whole list.
		rctx, cancel := context.WithTimeout(ctx, liveResolveTimeout)
		result, err := r.Resolve(rctx, creds, resolver.ResolveOpts{Logger: slogAdapter{h.logger}})
		cancel()
		if err != nil || result == nil {
			continue
		}
		if len(result.Models) == 0 {
			out[c.Provider] = nil
			continue
		}
		out[c.Provider] = result
	}
	return out
}

// connectionCredentials builds provider.Credentials from a connection record
// for the live resolver. Mirrors v1.go resolveCredentials' field extraction.
func connectionCredentials(c settings.ProviderConnection) domainProv.Credentials {
	creds := domainProv.Credentials{
		ProviderSpecificData: map[string]any{"_connectionId": c.ID},
	}
	var data map[string]any
	_ = json.Unmarshal(c.Data, &data)
	if v, ok := data["apiKey"].(string); ok {
		creds.APIKey = v
	}
	if v, ok := data["accessToken"].(string); ok {
		creds.AccessToken = v
	}
	if v, ok := data["refreshToken"].(string); ok && v != "" {
		// Copy the top-level refreshToken into ProviderSpecificData so
		// resolvers that refresh-on-401 (grok-cli, copilot) can reach it via
		// refreshTokenOf(creds) without knowing the on-disk layout.
		creds.ProviderSpecificData["refreshToken"] = v
	}
	if v, ok := data["providerSpecificData"].(map[string]any); ok {
		for k, val := range v {
			creds.ProviderSpecificData[k] = val
		}
	}
	return creds
}

// liveResolveTimeout bounds each live resolver call. Matches the JS
// fetchCompatibleModelIds 5s budget.
const liveResolveTimeout = 5 * time.Second

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
