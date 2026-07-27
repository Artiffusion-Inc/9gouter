// Package proxychat — per-provider thinking-mode gate backed by the settings
// repo.
//
// providerThinkingGate reads settings.providerThinking[provider].mode — the
// dashboard "Thinking" toggle per provider (src/app/(dashboard)/dashboard/
// providers/[id]/ClientPage.js saves it as { providerThinking: { [id]:
// { mode } } }). It ports open-sse/handlers/chatCore.js:70-82: when mode != "auto"
// the chat path injects a thinking/reasoning_effort override into the raw
// client body BEFORE translation, so the downstream translator + per-provider
// thinkingUnified logic remaps it into the provider-native field. The Go rewrite
// previously dropped this entirely (isThinkingEnabled was a hard-coded body/
// header/model-name heuristic and never read settings), so the dashboard toggle
// did nothing.
//
// The gate caches the parsed mode-per-provider map for providerThinkingCacheTTL
// (mirroring the token-saver / observability gates' 5s cache) so a chat burst
// does not turn into one settings read per request.
package proxychat

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// providerThinkingCacheTTL mirrors the token-saver / observability gate cache.
const providerThinkingCacheTTL = 5 * time.Second

// ProviderThinkingGate is the live per-provider thinking-mode surface. A nil
// gate (the zero value of Dependencies.ProviderThinkingGate) yields "" for
// every provider — matching the pre-port behaviour where no thinking override
// is injected (auto).
type ProviderThinkingGate interface {
	// Mode returns the configured thinking mode for the provider, or "" when
	// the provider has no override / the mode is "auto" / no gate is wired.
	// Empty means "do not inject".
	Mode(ctx context.Context, provider string) string
}

// providerThinkingSettingsReader is the subset of *repo.SettingsRepo the gate
// needs.
type providerThinkingSettingsReader interface {
	Get(ctx context.Context) (*settings.Settings, error)
}

type providerThinkingGate struct {
	settings providerThinkingSettingsReader

	mu       sync.Mutex
	cached   map[string]string
	cachedAt time.Time
}

// NewProviderThinkingGate wraps a *repo.SettingsRepo into a
// ProviderThinkingGate. Returns nil when settings is nil so the chat path
// keeps the pre-port fail-open behaviour (no thinking override injected) on a
// misconfigured composition root.
func NewProviderThinkingGate(settings *repo.SettingsRepo) ProviderThinkingGate {
	if settings == nil {
		return nil
	}
	return &providerThinkingGate{settings: settings}
}

// Mode returns the cached thinking mode for the provider, refreshing from the
// settings repo when the cache is stale. On any read/parse error it returns
// "" (auto / no injection) for the duration of the cache window so a flaky DB
// is not retried per request.
func (g *providerThinkingGate) Mode(ctx context.Context, provider string) string {
	if provider == "" {
		return ""
	}
	g.mu.Lock()
	if !g.cachedAt.IsZero() && time.Since(g.cachedAt) < providerThinkingCacheTTL {
		mode := g.cached[provider]
		g.mu.Unlock()
		return mode
	}
	g.mu.Unlock()

	modes, err := g.read(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		// Cache the empty map too so a flaky DB is not retried per request.
		g.cached = map[string]string{}
		g.cachedAt = time.Now()
		return ""
	}
	g.cached = modes
	g.cachedAt = time.Now()
	return modes[provider]
}

func (g *providerThinkingGate) read(ctx context.Context) (map[string]string, error) {
	s, err := g.settings.Get(ctx)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(s.Data, &obj); err != nil {
		return nil, err
	}
	raw, ok := obj["providerThinking"].(map[string]any)
	if !ok {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(raw))
	for provider, cfg := range raw {
		entry, ok := cfg.(map[string]any)
		if !ok {
			continue
		}
		mode, _ := entry["mode"].(string)
		// "auto" (and empty) means "do not inject" — skip storing so a lookup
		// returns "" and injection is skipped. Matches chatCore.js gating on
		// `providerThinking.mode !== "auto"`.
		if mode == "" || mode == "auto" {
			continue
		}
		out[provider] = mode
	}
	return out, nil
}
