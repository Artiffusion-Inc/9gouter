package http

import (
	"context"
	"encoding/json"
	"math/rand"
	"sync"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
)

// proxypoolrotate.go ports src/lib/network/connectionProxy.js pickProxyPoolId
// + src/sse/services/auth.js getProviderCredentials no-auth branch from
// decolua/9router #2409 (e1f3399b): round-robin / random rotation across all
// active proxy pools for no-auth free providers (opencode, grok-web, mimo-free,
// mmf) so load is distributed and per-IP rate limits are avoided. The rotation
// strategy is selectable per provider in the dashboard and persisted to
// settings.providerStrategies[providerId].rotateStrategy.
//
// Round-robin state is in-memory and resets on restart, matching the JS
// `rotateState` Map. Random uses math/rand (process-wide source); this is
// request-routing, not security-sensitive, so the default non-seeded source is
// acceptable (Go 1.20+ auto-seeds the global source).

// rotateStateKeyed holds the per-provider round-robin index. Guarded by a mutex
// so concurrent requests on the same no-auth provider advance the counter
// atomically rather than racing on map read/write.
var rotateStateKeyed = struct {
	sync.Mutex
	m map[string]int
}{m: make(map[string]int)}

// pickProxyPoolId chooses one pool id from poolIds based on strategy.
// round-robin cycles sequentially (in-memory, resets on restart); random makes
// a uniform pick; none/single (or unknown strategy) returns the first entry.
// Returns "" when poolIds is empty.
func pickProxyPoolId(poolIds []string, strategy, providerID string) string {
	if len(poolIds) == 0 {
		return ""
	}
	if len(poolIds) == 1 {
		return poolIds[0]
	}
	switch strategy {
	case "round-robin":
		rotateStateKeyed.Lock()
		// Start at -1 (zero value) so the first call yields index 0, matching
		// the JS `state.index = (state.index + 1) % len` with index seeded -1.
		cur, ok := rotateStateKeyed.m[providerID]
		if !ok {
			cur = -1
		}
		next := cur + 1
		if next >= len(poolIds) {
			next = 0
		}
		rotateStateKeyed.m[providerID] = next
		rotateStateKeyed.Unlock()
		return poolIds[next]
	case "random":
		return poolIds[rand.Intn(len(poolIds))]
	default:
		return poolIds[0]
	}
}

// readRotateStrategy resolves the effective rotate strategy for a provider from
// settings.providerStrategies[providerId].rotateStrategy. Mirrors the JS
// `override.rotateStrategy || "none"` lookup. Returns "none" when unset.
func readRotateStrategy(settingsData map[string]any, providerID string) string {
	if overrides, ok := settingsData["providerStrategies"].(map[string]any); ok {
		if ov, ok := overrides[providerID].(map[string]any); ok {
			if v, ok := ov["rotateStrategy"].(string); ok && v != "" {
				return v
			}
		}
	}
	return "none"
}

// activeProxyPoolIds returns the ids of active proxy pools that carry a
// proxyUrl, mirroring the JS `getProxyPools({isActive:true}).filter(p =>
// p.proxyUrl).map(p => p.id)`. Pools without a proxyUrl cannot rotate traffic
// and are excluded. Errors are non-fatal: a repo failure degrades to an empty
// list (the caller then falls back to the configured proxyPoolId or direct).
func activeProxyPoolIds(ctx context.Context, poolRepo *repo.ProxyPoolRepo) []string {
	if poolRepo == nil {
		return nil
	}
	active := boolPtr(true)
	pools, err := poolRepo.List(ctx, repo.ProxyPoolFilter{IsActive: active})
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(pools))
	for _, p := range pools {
		if !p.IsActive {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(p.Data, &data); err != nil {
			continue
		}
		if v, ok := data["proxyUrl"].(string); ok && v != "" {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// resolveNoAuthProxyRotation applies the e1f3399b rotation to a no-auth
// provider's virtual public connection providerSpecificData. When the provider's
// rotateStrategy is "round-robin" or "random", it picks one active pool with a
// proxyUrl, writes its id to psd["proxyPoolId"], and calls
// resolveConnectionProxyConfig so the executor's ProxyAwareFetch routes through
// the rotated pool. With strategy "none" (default) psd is left untouched — the
// no-auth connection has no per-connection proxy and goes direct, preserving
// pre-rotation behaviour.
//
// settingsData is the already-parsed settings.Data map the caller read for the
// request; nil is tolerated (treated as "none"). Returns the strategy actually
// applied, for diagnostics.
func (h *v1Handler) resolveNoAuthProxyRotation(ctx context.Context, psd map[string]any, settingsData map[string]any, providerID string) string {
	strategy := readRotateStrategy(settingsData, providerID)
	if strategy == "none" {
		return strategy
	}
	poolIds := activeProxyPoolIds(ctx, h.deps.ProxyPoolRepo)
	picked := pickProxyPoolId(poolIds, strategy, providerID)
	if picked == "" {
		return strategy
	}
	if psd == nil {
		return strategy
	}
	psd["proxyPoolId"] = picked
	h.resolveConnectionProxyConfig(ctx, psd)
	return strategy
}
