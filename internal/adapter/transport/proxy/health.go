package proxy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Health tracks proxy TCP reachability with TTLs and inflight dedup.
type Health struct {
	mu         sync.RWMutex
	entries    map[string]*healthEntry
	opts       Options
	flight     singleflight.Group
}

type healthEntry struct {
	healthy   bool
	checkedAt time.Time
	ttl       time.Duration
}

// GlobalHealth returns a package-level default Health cache for opts. Callers may
// also construct their own Health.
func GlobalHealth(opts Options) *Health {
	globalHealthOnce.Do(func() {
		globalHealth = NewHealth(opts)
	})
	return globalHealth
}

var (
	globalHealth     *Health
	globalHealthOnce sync.Once
)

// NewHealth creates a new health cache with the provided options.
func NewHealth(opts Options) *Health {
	return &Health{
		entries: make(map[string]*healthEntry),
		opts:    opts,
	}
}

// Get returns the cached healthy status and whether it is fresh.
func (h *Health) Get(proxyURL string) (healthy bool, ok bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, ok := h.entries[proxyURL]
	if !ok {
		return false, false
	}
	if time.Since(e.checkedAt) >= e.ttl {
		return false, false
	}
	return e.healthy, true
}

// Set records a health result with the appropriate TTL.
func (h *Health) Set(proxyURL string, healthy bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ttl := h.opts.ProxyHealthCacheTTL
	if !healthy && h.opts.ProxyHealthUnhealthyTTL < ttl {
		ttl = h.opts.ProxyHealthUnhealthyTTL
	}
	h.entries[proxyURL] = &healthEntry{
		healthy:   healthy,
		checkedAt: time.Now(),
		ttl:       ttl,
	}
}

// Invalidate removes a cached entry.
func (h *Health) Invalidate(proxyURL string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.entries, proxyURL)
}

// Probe runs fn once for concurrent callers of the same proxyURL and caches
// the result.
func (h *Health) Probe(ctx context.Context, proxyURL string, fn func(context.Context) error) error {
	_, err, _ := h.flight.Do(proxyURL, func() (interface{}, error) {
		err := fn(ctx)
		h.Set(proxyURL, err == nil)
		if err != nil {
			return nil, fmt.Errorf("proxy %s unreachable: %w", ProxyURLForLogs(proxyURL), err)
		}
		return nil, nil
	})
	return err
}

// All returns all cached entries for diagnostics.
func (h *Health) All() []HealthStatus {
	now := time.Now()
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]HealthStatus, 0, len(h.entries))
	for url, e := range h.entries {
		out = append(out, HealthStatus{
			ProxyURL:  url,
			Healthy:   e.healthy,
			CheckedAt: e.checkedAt,
			Stale:     now.Sub(e.checkedAt) >= e.ttl,
		})
	}
	return out
}

// HealthStatus is a diagnostic snapshot of a cache entry.
type HealthStatus struct {
	ProxyURL  string
	Healthy   bool
	CheckedAt time.Time
	Stale     bool
}
