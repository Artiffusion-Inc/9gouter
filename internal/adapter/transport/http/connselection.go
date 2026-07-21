package http

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// ErrNoActiveCredentials is returned when every active connection for a
// provider is excluded (Fix 3 fallback loop exhausted) — distinct from the
// "no active credentials configured" error so the caller can surface a 503
// "all accounts unavailable" instead of a 404 "no credentials".
var ErrNoActiveCredentials = errors.New("all active credentials excluded")

// connectionUsage is the sticky-round-robin state read from a connection's
// JSON data blob. Ports the JS `lastUsedAt` + `consecutiveUseCount` fields
// consumed by getProviderCredentials (decolua/9router #2703 Fix 4).
type connectionUsage struct {
	lastUsedAt          time.Time
	lastUsedAtPresent   bool
	consecutiveUseCount int
}

// readConnectionUsage parses the optional lastUsedAt/consecutiveUseCount
// fields from a connection's stored data. A zero lastUsedAt with
// lastUsedAtPresent=false means "never used".
func readConnectionUsage(c settings.ProviderConnection) connectionUsage {
	var data map[string]any
	_ = json.Unmarshal(c.Data, &data)
	u := connectionUsage{}
	if v, ok := data["lastUsedAt"].(string); ok && v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			u.lastUsedAt = t
			u.lastUsedAtPresent = true
		}
	}
	if v, ok := data["consecutiveUseCount"].(float64); ok {
		u.consecutiveUseCount = int(v)
	} else if v, ok := data["consecutiveUseCount"].(int); ok {
		u.consecutiveUseCount = v
	}
	return u
}

// pickStickyRoundRobin implements the JS getProviderCredentials
// round-robin branch (decolua/9router #2703 Fix 4):
//
//   - Sort available connections by lastUsedAt descending (most-recent first);
//     never-used connections sort by priority as a tiebreaker (JS falls back
//     to priority asc with default 999).
//   - If the most-recently-used connection has been used consecutiveUseCount
//     times below stickyLimit, stay with it: the caller bumps the count.
//   - Otherwise pick the least-recently-used connection (oldest first, never-
//     used wins) and the caller resets the count to 1.
//
// Returns the chosen index into `conns` and a flag indicating whether the
// caller should increment the existing consecutiveUseCount (stay=true) or
// reset it to 1 (stay=false). The caller persists via
// ConnectionRepo.UpdateUsageMeta.
//
// When stickyLimit <= 0 the selector degrades to fill-first (return index 0,
// stay=false), matching the JS `stickyRoundRobinLimit || 3` default but
// allowing an operator to disable stickiness with 0.
func pickStickyRoundRobin(conns []settings.ProviderConnection, stickyLimit int) (int, bool) {
	if len(conns) == 0 {
		return 0, false
	}
	if stickyLimit <= 0 {
		return 0, false
	}
	usages := make([]connectionUsage, len(conns))
	for i := range conns {
		usages[i] = readConnectionUsage(conns[i])
	}

	// Most-recent first.
	byRecency := make([]int, len(conns))
	for i := range conns {
		byRecency[i] = i
	}
	sort.SliceStable(byRecency, func(a, b int) bool {
		ua, ub := usages[byRecency[a]], usages[byRecency[b]]
		if !ua.lastUsedAtPresent && !ub.lastUsedAtPresent {
			return conns[byRecency[a]].Priority < conns[byRecency[b]].Priority
		}
		if !ua.lastUsedAtPresent {
			return false
		}
		if !ub.lastUsedAtPresent {
			return true
		}
		return ua.lastUsedAt.After(ub.lastUsedAt)
	})

	current := byRecency[0]
	if usages[current].lastUsedAtPresent && usages[current].consecutiveUseCount < stickyLimit {
		return current, true
	}

	// Oldest first.
	byOldest := make([]int, len(conns))
	for i := range conns {
		byOldest[i] = i
	}
	sort.SliceStable(byOldest, func(a, b int) bool {
		ua, ub := usages[byOldest[a]], usages[byOldest[b]]
		if !ua.lastUsedAtPresent && !ub.lastUsedAtPresent {
			return conns[byOldest[a]].Priority < conns[byOldest[b]].Priority
		}
		if !ua.lastUsedAtPresent {
			return true
		}
		if !ub.lastUsedAtPresent {
			return false
		}
		return ua.lastUsedAt.Before(ub.lastUsedAt)
	})
	return byOldest[0], false
}

// readStrategySettings resolves the effective fallback strategy and sticky
// limit for a provider: per-provider override wins, else global, else default.
// Mirrors the JS `(settings.providerStrategies||{})[providerId]` lookup plus
// the `settings.fallbackStrategy` and `settings.stickyRoundRobinLimit`
// fallbacks (defaults "fill-first" and 3).
func readStrategySettings(settingsData map[string]any, providerID string) (string, int) {
	strategy := "fill-first"
	stickyLimit := 3

	if v, ok := settingsData["fallbackStrategy"].(string); ok && v != "" {
		strategy = v
	}
	if v, ok := settingsData["stickyRoundRobinLimit"].(float64); ok {
		stickyLimit = int(v)
	} else if v, ok := settingsData["stickyRoundRobinLimit"].(int); ok {
		stickyLimit = v
	}

	if overrides, ok := settingsData["providerStrategies"].(map[string]any); ok {
		if ov, ok := overrides[providerID].(map[string]any); ok {
			if v, ok := ov["fallbackStrategy"].(string); ok && v != "" {
				strategy = v
			}
			if v, ok := ov["stickyRoundRobinLimit"].(float64); ok {
				stickyLimit = int(v)
			} else if v, ok := ov["stickyRoundRobinLimit"].(int); ok {
				stickyLimit = v
			}
		}
	}
	return strategy, stickyLimit
}

// selectConnection picks the connection index to use out of the available
// slice, applying the provider's effective fallback strategy. fill-first
// (default) returns index 0; round-robin delegates to pickStickyRoundRobin.
// Settings are read through the handler's SettingsRepo; on a settings read
// failure the selector degrades to fill-first so a settings error never
// blocks an otherwise-valid request.
func selectConnection(ctx context.Context, h *v1Handler, available []settings.ProviderConnection, providerID string) (int, bool) {
	s, err := h.deps.SettingsRepo.Get(ctx)
	if err != nil {
		return 0, false
	}
	var settingsData map[string]any
	_ = json.Unmarshal(s.Data, &settingsData)
	strategy, stickyLimit := readStrategySettings(settingsData, providerID)
	if strategy != "round-robin" {
		return 0, false
	}
	return pickStickyRoundRobin(available, stickyLimit)
}

// persistStickySelection writes the chosen connection's lastUsedAt and
// consecutiveUseCount back to the ConnectionRepo. When stay=true the count
// is incremented (sticky continuation); when stay=false it resets to 1
// (rotation to a fresh account). Failures are logged but non-fatal: a write
// error must never block the request — at worst the next selection is
// slightly stale, which the sticky algorithm tolerates.
func persistStickySelection(h *v1Handler, conn settings.ProviderConnection, stay bool) {
	usage := readConnectionUsage(conn)
	now := time.Now().UTC()
	count := 1
	if stay {
		count = usage.consecutiveUseCount + 1
	}
	if err := h.deps.ConnectionRepo.UpdateUsageMeta(context.Background(), conn.ID, now, count); err != nil {
		h.logger.Warn("sticky selection persist failed", "connectionId", conn.ID, "error", err)
	}
}