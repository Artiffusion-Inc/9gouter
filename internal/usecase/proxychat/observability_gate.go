// Package proxychat — observability gate backed by the settings repo.
//
// settingsGate reads the dashboard enableObservability flag + retention knobs
// from the settings JSON blob (the same keys the JS requestDetailsRepo.js
// read: enableObservability, observabilityMaxRecords, observabilityMaxJsonSize).
// It caches the parsed config for 5s (the JS CONFIG_CACHE_TTL_MS) so the per-
// request Enabled() call is a cheap map lookup, not a DB round-trip.
package proxychat

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

const observabilityCacheTTL = 5 * time.Second

// settingsReader is the subset of *repo.SettingsRepo the gate needs.
type settingsReader interface {
	Get(ctx context.Context) (*settings.Settings, error)
}

// settingsGate reads enableObservability + retention knobs from settings,
// cached for observabilityCacheTTL. A nil gate (the zero value of
// Dependencies.ObservabilityGate) disables observability.
type settingsGate struct {
	settings settingsReader

	mu       sync.Mutex
	cached   obsConfig
	cachedAt time.Time
}

type obsConfig struct {
	enabled   bool
	maxRec    int
	maxJsonKB int
}

// NewObservabilityGate wraps a *repo.SettingsRepo into a proxychat
// ObservabilityGate. Returns nil if settings is nil (observability off).
func NewObservabilityGate(settings *repo.SettingsRepo) ObservabilityGate {
	if settings == nil {
		return nil
	}
	return &settingsGate{settings: settings}
}

func (g *settingsGate) Enabled(ctx context.Context) (maxRecords int, maxJsonSize int, ok bool) {
	g.mu.Lock()
	if !g.cachedAt.IsZero() && time.Since(g.cachedAt) < observabilityCacheTTL {
		c := g.cached
		g.mu.Unlock()
		if !c.enabled {
			return 0, 0, false
		}
		return c.maxRec, c.maxJsonKB * 1024, true
	}
	g.mu.Unlock()

	// Re-read outside the lock to avoid holding it across a DB call.
	cfg, err := g.read(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cached = cfg
	g.cachedAt = time.Now()
	if err != nil || !cfg.enabled {
		return 0, 0, false
	}
	return cfg.maxRec, cfg.maxJsonKB * 1024, true
}

func (g *settingsGate) read(ctx context.Context) (obsConfig, error) {
	s, err := g.settings.Get(ctx)
	if err != nil {
		return obsConfig{}, err
	}
	var obj map[string]any
	if err := json.Unmarshal(s.Data, &obj); err != nil {
		return obsConfig{}, err
	}
	cfg := obsConfig{
		enabled:   boolVal(obj["enableObservability"], true),
		maxRec:    intVal(obj["observabilityMaxRecords"], 1000),
		maxJsonKB: intVal(obj["observabilityMaxJsonSize"], 5),
	}
	return cfg, nil
}

func boolVal(v any, def bool) bool {
	if v == nil {
		return def
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

func intVal(v any, def int) int {
	switch x := v.(type) {
	case float64:
		if x > 0 {
			return int(x)
		}
	case int:
		if x > 0 {
			return x
		}
	}
	return def
}