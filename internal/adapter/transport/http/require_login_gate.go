package http

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// requireLoginGate reads settings.requireLogin from the settings JSON blob and
// caches it for requireLoginCacheTTL so the per-request check is a cheap map
// lookup, not a DB round-trip. It mirrors the legacy dashboardGuard.js
// isAuthenticated() branch: when settings.requireLogin === false, the /api/*
// session-auth requirement is bypassed for non-ALWAYS_PROTECTED routes.
//
// A nil gate (the zero value when NewRequireLoginGate is given a nil repo) keeps
// the historical behaviour: requireLogin defaults to true (deny-by-default),
// matching the previous Go wiring that never consulted settings at all.
type requireLoginGate struct {
	settings settingsReader

	mu       sync.Mutex
	cached   bool
	cachedAt time.Time
}

// requireLoginCacheTTL mirrors the proxychat observability gate's 5s cache so a
// /api/* fan-out from the dashboard does not turn into one DB read per call.
const requireLoginCacheTTL = 5 * time.Second

// settingsReader is the subset of *repo.SettingsRepo the gate needs.
type settingsReader interface {
	Get(ctx context.Context) (*settings.Settings, error)
}

// NewRequireLoginGate wraps a *repo.SettingsRepo into a requireLogin gate.
// Returns nil when settings is nil so the historical deny-by-default
// behaviour is preserved bit-for-bit on a misconfigured composition root.
func NewRequireLoginGate(settings *repo.SettingsRepo) *requireLoginGate {
	if settings == nil {
		return nil
	}
	return &requireLoginGate{settings: settings}
}

// RequireLogin reports whether the dashboard requires a logged-in session for
// /api/* routes. Returns true (require login) on any read/parse error or when
// the settings blob omits the flag — the safe default that preserves the
// pre-#205 deny-by-default behaviour.
func (g *requireLoginGate) RequireLogin(ctx context.Context) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	if !g.cachedAt.IsZero() && time.Since(g.cachedAt) < requireLoginCacheTTL {
		v := g.cached
		g.mu.Unlock()
		return v
	}
	g.mu.Unlock()

	v, err := g.read(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		// Cache the safe default too so a flaky DB does not get hammered.
		g.cached = true
		g.cachedAt = time.Now()
		return true
	}
	g.cached = v
	g.cachedAt = time.Now()
	return v
}

func (g *requireLoginGate) read(ctx context.Context) (bool, error) {
	s, err := g.settings.Get(ctx)
	if err != nil {
		return true, err
	}
	var obj map[string]any
	if err := json.Unmarshal(s.Data, &obj); err != nil {
		return true, err
	}
	// settings.requireLogin defaults to true (legacy dashboardGuard.js:
	// `settings.requireLogin !== false`). Missing/non-bool → require login.
	if b, ok := obj["requireLogin"].(bool); ok {
		return b, nil
	}
	return true, nil
}
