// Package proxychat — token-saver gate backed by the settings repo.
//
// tokenSaverGate reads the dashboard token-saver switches (the same keys the JS
// src/sse/handlers/chat.js read from getSettings() on every chat request:
// rtkEnabled, headroomEnabled, headroomUrl, headroomCompressUserMessages,
// cavemanEnabled/Level, ponytailEnabled/Level, pxpipeEnabled/MinChars/
// TimeoutMs). It caches the parsed TokenSaverConfig for tokenSaverCacheTTL so
// the per-request Config() call is a cheap map lookup, not a DB round-trip —
// mirroring the observability gate's 5s CONFIG_CACHE_TTL_MS.
//
// PxpipeTransform is left nil: the legacy side lazily loaded an in-process
// module (getPxpipeTransform()); the Go build has no such loader yet, so
// runPxpipe stays a fail-open no-op (it returns early when Transform == nil)
// until the pxpipe transform is ported. Enabling pxpipe in the dashboard
// therefore surfaces as "pxpipe skipped" rather than silently doing nothing
// useful — the safe, observable behaviour.
package proxychat

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/pxpipe"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// tokenSaverCacheTTL mirrors the observability gate's 5s cache so a chat burst
// does not turn into one settings read per request.
const tokenSaverCacheTTL = 5 * time.Second

// defaultHeadroomURL matches src/sse/handlers/chat.js DEFAULT_HEADROOM_URL and
// the settings default in repo.defaultSettings. It is the fallback when
// settings.headroomUrl is empty AND no HEADROOM_URL env override is set.
const defaultHeadroomURL = "http://localhost:8787"

// TokenSaverGate is the live token-saver settings surface. A nil gate (the zero
// value of Dependencies.TokenSaverGate) yields the zero TokenSaverConfig, which
// disables every stage — matching the pre-#208 behaviour where the chat path
// never populated Request.TokenSavers.
type TokenSaverGate interface {
	Config(ctx context.Context) TokenSaverConfig
}

// tokenSaverSettingsReader is the subset of *repo.SettingsRepo the gate needs.
type tokenSaverSettingsReader interface {
	Get(ctx context.Context) (*settings.Settings, error)
}

type tokenSaverGate struct {
	settings tokenSaverSettingsReader

	mu       sync.Mutex
	cached   TokenSaverConfig
	cachedAt time.Time
}

// NewTokenSaverGate wraps a *repo.SettingsRepo into a TokenSaverGate. Returns
// nil when settings is nil so the chat path keeps the pre-#208 fail-open
// behaviour (all token-saver stages off) on a misconfigured composition root.
func NewTokenSaverGate(settings *repo.SettingsRepo) TokenSaverGate {
	if settings == nil {
		return nil
	}
	return &tokenSaverGate{settings: settings}
}

// Config returns the cached token-saver config, refreshing from the settings
// repo when the cache is stale. On any read/parse error it returns the zero
// config (all stages off) for the duration of the cache window so a flaky DB
// does not get hammered on every request.
func (g *tokenSaverGate) Config(ctx context.Context) TokenSaverConfig {
	g.mu.Lock()
	if !g.cachedAt.IsZero() && time.Since(g.cachedAt) < tokenSaverCacheTTL {
		cfg := g.cached
		g.mu.Unlock()
		return cfg
	}
	g.mu.Unlock()

	cfg, err := g.read(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		// Cache the zero config too so a flaky DB is not retried per request.
		g.cached = TokenSaverConfig{}
		g.cachedAt = time.Now()
		return TokenSaverConfig{}
	}
	g.cached = cfg
	g.cachedAt = time.Now()
	return cfg
}

func (g *tokenSaverGate) read(ctx context.Context) (TokenSaverConfig, error) {
	s, err := g.settings.Get(ctx)
	if err != nil {
		return TokenSaverConfig{}, err
	}
	var obj map[string]any
	if err := json.Unmarshal(s.Data, &obj); err != nil {
		return TokenSaverConfig{}, err
	}
	cfg := TokenSaverConfig{
		RtkEnabled:           boolVal(obj["rtkEnabled"], true),
		HeadroomEnabled:      boolVal(obj["headroomEnabled"], false),
		HeadroomCompressUser: boolVal(obj["headroomCompressUserMessages"], false),
		CavemanEnabled:       boolVal(obj["cavemanEnabled"], false),
		CavemanLevel:         strVal(obj["cavemanLevel"], "full"),
		PonytailEnabled:      boolVal(obj["ponytailEnabled"], false),
		PonytailLevel:        strVal(obj["ponytailLevel"], "full"),
		PxpipeEnabled:        boolVal(obj["pxpipeEnabled"], false),
		PxpipeMinChars:       intVal(obj["pxpipeMinChars"], 25000),
		PxpipeTimeoutMs:      intVal(obj["pxpipeTimeoutMs"], 15000),
		PxpipeTransform: pxpipeTransformFunc,
	}
	cfg.HeadroomURL = headroomURLFrom(obj)
	return cfg, nil
}

// headroomURLFrom mirrors the JS precedence: HEADROOM_URL env override →
// settings.headroomUrl → localhost:8787 default. Matches api.headroomHandler's
// headroomURL() ordering so the chat path and the proxy panel resolve the same
// target.
func headroomURLFrom(obj map[string]any) string {
	if v := os.Getenv("HEADROOM_URL"); v != "" {
		return v
	}
	if v, ok := obj["headroomUrl"].(string); ok && v != "" {
		return v
	}
	return defaultHeadroomURL
}

func strVal(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

// isZeroTokenSaverConfig reports whether cfg is the zero value, i.e. the caller
// did not populate it. Handle() uses this to decide whether to overwrite it
// with the gate's live config: a hand-built non-zero config (tests, explicit
// callers) is left alone, the zero value is filled from settings. PxpipeTransform
// is a func and cannot be compared, so it is excluded — the gate never sets it,
// and a caller that did set it also set the other scalar fields, so this check
// still detects "caller populated".
func isZeroTokenSaverConfig(cfg TokenSaverConfig) bool {
	return !cfg.RtkEnabled &&
		!cfg.HeadroomEnabled &&
		cfg.HeadroomURL == "" &&
		!cfg.HeadroomCompressUser &&
		!cfg.CavemanEnabled &&
		cfg.CavemanLevel == "" &&
		!cfg.PonytailEnabled &&
		cfg.PonytailLevel == "" &&
		!cfg.PxpipeEnabled &&
		cfg.PxpipeMinChars == 0 &&
		cfg.PxpipeTimeoutMs == 0
}


// pxpipeTransformFunc bridges the pxpipe-proxy Node library via a subprocess.
// It captures the minChars and timeout from the closure when called by the
// chat path. When pxpipe-proxy is not installed, it returns (nil, nil) which
// the rtk bridge treats as "no change" (fail-open).
//
// This function is created per-config-refresh rather than per-request to avoid
// allocating closures on the hot path. The minChars and timeoutMs are passed
// through the PxpipeTransform signature as the third argument and the gate's
// timeout field respectively.
func pxpipeTransformFunc(body []byte, model string, minChars int) ([]byte, error) {
	return pxpipe.Transform(body, model, minChars, "", 15000)
}
