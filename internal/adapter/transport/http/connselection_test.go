package http

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

func connWith(id string, priority int, lastUsedAt *time.Time, consecutive int) settings.ProviderConnection {
	data := map[string]any{
		"consecutiveUseCount": consecutive,
	}
	if lastUsedAt != nil {
		data["lastUsedAt"] = lastUsedAt.UTC().Format(time.RFC3339Nano)
	}
	raw, _ := json.Marshal(data)
	return settings.ProviderConnection{
		ID:       id,
		Provider: "openai",
		AuthType: "apiKey",
		IsActive: true,
		Priority: priority,
		Data:     json.RawMessage(raw),
	}
}

func TestReadConnectionUsage(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c := connWith("c1", 1, &now, 2)
	u := readConnectionUsage(c)
	if !u.lastUsedAtPresent {
		t.Fatal("expected lastUsedAt present")
	}
	if !u.lastUsedAt.Equal(now) {
		t.Fatalf("lastUsedAt = %v, want %v", u.lastUsedAt, now)
	}
	if u.consecutiveUseCount != 2 {
		t.Fatalf("consecutive = %d, want 2", u.consecutiveUseCount)
	}

	// Never used.
	c2 := connWith("c2", 1, nil, 0)
	u2 := readConnectionUsage(c2)
	if u2.lastUsedAtPresent {
		t.Fatal("expected lastUsedAt absent")
	}
	if u2.consecutiveUseCount != 0 {
		t.Fatalf("consecutive = %d, want 0", u2.consecutiveUseCount)
	}
}

// TestPickStickyRoundRobin_StaysUnderLimit verifies that when the most-recently
// used connection is still under the sticky limit, the selector keeps it and
// reports stay=true so the caller increments the count.
func TestPickStickyRoundRobin_StaysUnderLimit(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 7, 1, 12, 0, 5, 0, time.UTC) // most recent
	conns := []settings.ProviderConnection{
		connWith("a", 1, &t0, 1),
		connWith("b", 2, &t1, 1), // most-recent, count 1 < limit 3
		connWith("c", 3, nil, 0),
	}
	idx, stay := pickStickyRoundRobin(conns, 3)
	if conns[idx].ID != "b" {
		t.Fatalf("picked %s, want b (most-recent under limit)", conns[idx].ID)
	}
	if !stay {
		t.Fatal("expected stay=true (continue sticky account)")
	}
}

// TestPickStickyRoundRobin_RotatesAtLimit verifies that once the current
// account hits the sticky limit, the selector rotates to the least-recently
// used connection and reports stay=false so the caller resets the count.
// Never-used connections sort as oldest (JS puts never-used first in the
// least-recently-used ordering), so a fresh account wins over a used one.
func TestPickStickyRoundRobin_RotatesAtLimit(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) // oldest used
	t1 := time.Date(2026, 7, 1, 12, 0, 5, 0, time.UTC) // most recent, count == limit
	conns := []settings.ProviderConnection{
		connWith("a", 1, &t0, 1), // used least-recently
		connWith("b", 2, &t1, 3), // current, exhausted sticky limit
		connWith("c", 3, nil, 0), // never used → oldest, next pick
	}
	idx, stay := pickStickyRoundRobin(conns, 3)
	if conns[idx].ID != "c" {
		t.Fatalf("picked %s, want c (never-used sorts as oldest)", conns[idx].ID)
	}
	if stay {
		t.Fatal("expected stay=false (rotation resets count to 1)")
	}
}

// TestPickStickyRoundRobin_RotatesToLeastRecentlyUsed verifies that when every
// connection has been used, rotation lands on the least-recently-used (oldest
// timestamp), not the most-recent.
func TestPickStickyRoundRobin_RotatesToLeastRecentlyUsed(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) // oldest used → next pick
	t1 := time.Date(2026, 7, 1, 12, 0, 5, 0, time.UTC) // most recent, count == limit
	conns := []settings.ProviderConnection{
		connWith("a", 1, &t0, 1), // least-recently-used
		connWith("b", 2, &t1, 3), // current, exhausted
	}
	idx, stay := pickStickyRoundRobin(conns, 3)
	if conns[idx].ID != "a" {
		t.Fatalf("picked %s, want a (least-recently-used among used)", conns[idx].ID)
	}
	if stay {
		t.Fatal("expected stay=false (rotation resets count to 1)")
	}
}

// TestPickStickyRoundRobin_NeverUsedPicksByPriority verifies never-used
// connections sort by priority asc as a tiebreaker, mirroring the JS fallback.
func TestPickStickyRoundRobin_NeverUsedPicksByPriority(t *testing.T) {
	conns := []settings.ProviderConnection{
		connWith("a", 5, nil, 0),
		connWith("b", 1, nil, 0),
		connWith("c", 9, nil, 0),
	}
	idx, stay := pickStickyRoundRobin(conns, 3)
	if conns[idx].ID != "b" {
		t.Fatalf("picked %s, want b (lowest priority)", conns[idx].ID)
	}
	if stay {
		t.Fatal("never-used should report stay=false")
	}
}

// TestPickStickyRoundRobin_ZeroLimitDegradesToFillFirst verifies stickyLimit<=0
// disables stickiness and falls back to fill-first (index 0).
func TestPickStickyRoundRobin_ZeroLimitDegradesToFillFirst(t *testing.T) {
	t1 := time.Date(2026, 7, 1, 12, 0, 5, 0, time.UTC)
	conns := []settings.ProviderConnection{
		connWith("a", 2, nil, 0),
		connWith("b", 1, &t1, 1),
	}
	idx, stay := pickStickyRoundRobin(conns, 0)
	if idx != 0 {
		t.Fatalf("zero limit should pick index 0 (fill-first), got %d", idx)
	}
	if stay {
		t.Fatal("fill-first should report stay=false")
	}
}

func TestReadStrategySettings_Defaults(t *testing.T) {
	s, lim := readStrategySettings(map[string]any{}, "openai")
	if s != "fill-first" {
		t.Fatalf("default strategy = %q, want fill-first", s)
	}
	if lim != 3 {
		t.Fatalf("default sticky limit = %d, want 3", lim)
	}
}

func TestReadStrategySettings_GlobalOverride(t *testing.T) {
	cfg := map[string]any{
		"fallbackStrategy":     "round-robin",
		"stickyRoundRobinLimit": float64(5),
	}
	s, lim := readStrategySettings(cfg, "openai")
	if s != "round-robin" {
		t.Fatalf("strategy = %q, want round-robin", s)
	}
	if lim != 5 {
		t.Fatalf("sticky limit = %d, want 5", lim)
	}
}

func TestReadStrategySettings_PerProviderOverride(t *testing.T) {
	cfg := map[string]any{
		"fallbackStrategy":     "fill-first",
		"stickyRoundRobinLimit": float64(3),
		"providerStrategies": map[string]any{
			"anthropic": map[string]any{
				"fallbackStrategy":      "round-robin",
				"stickyRoundRobinLimit": float64(7),
			},
		},
	}
	// anthropic gets the override.
	s, lim := readStrategySettings(cfg, "anthropic")
	if s != "round-robin" || lim != 7 {
		t.Fatalf("anthropic override: strategy=%q limit=%d, want round-robin/7", s, lim)
	}
	// openai falls back to global.
	s2, lim2 := readStrategySettings(cfg, "openai")
	if s2 != "fill-first" || lim2 != 3 {
		t.Fatalf("openai fallback: strategy=%q limit=%d, want fill-first/3", s2, lim2)
	}
}