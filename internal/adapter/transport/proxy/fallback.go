package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const fallbackCacheTTL = 5 * time.Minute

// ProxyPoolSource abstracts the proxyPools repo so the proxy package doesn't
// depend directly on repo internals. The repo interface from T006 only exposes
// List and GetByID; List returns active pools when filtered.
type ProxyPoolSource interface {
	List(ctx context.Context, isActive bool) ([]ProxyPool, error)
}

// ProxyPool is a minimal representation of a proxy pool entry.
type ProxyPool struct {
	ID       string
	ProxyURL string
	Type     string
	Host     string
	Port     string
	Username string
	Password string
	IsActive bool
}

// Fallback finds a working proxy for a target. It mirrors proxyFallback.js.
type Fallback struct {
	mu        sync.RWMutex
	cache     map[string]*fallbackEntry
	opts      Options
	health    *Health
	source    ProxyPoolSource
	client    *http.Client
}

type fallbackEntry struct {
	proxyURL  string
	expiresAt time.Time
}

// NewFallback creates a fallback selector.
func NewFallback(opts Options, source ProxyPoolSource, health *Health) *Fallback {
	return &Fallback{
		cache:  make(map[string]*fallbackEntry),
		opts:   opts,
		health: health,
		source: source,
		client: &http.Client{
			Timeout: opts.ProxyFallbackProbeTimeout,
		},
	}
}

func (f *Fallback) cacheKey(targetURL string) string {
	u, err := url.Parse(targetURL)
	if err != nil {
		return strings.ToLower(targetURL)
	}
	return fmt.Sprintf("%s://%s%s%s", u.Scheme, u.Host, u.Path, u.RawQuery)
}

// Find returns a working *http.Transport for the target, or nil if none found.
func (f *Fallback) Find(ctx context.Context, targetURL string) (*http.Transport, string, error) {
	key := f.cacheKey(targetURL)

	f.mu.RLock()
	cached, ok := f.cache[key]
	f.mu.RUnlock()
	if ok {
		if cached.expiresAt.After(time.Now()) {
			if cached.proxyURL == "" {
				return nil, "", nil
			}
			tr, err := NewTransport(f.opts, cached.proxyURL)
			return tr, cached.proxyURL, err
		}
		f.mu.Lock()
		delete(f.cache, key)
		f.mu.Unlock()
	}

	candidates, err := f.collectCandidates(ctx, targetURL)
	if err != nil {
		return nil, "", err
	}
	if len(candidates) == 0 {
		return nil, "", nil
	}

	type result struct {
		proxyURL string
		ok       bool
	}
	results := make(chan result, len(candidates))
	var wg sync.WaitGroup
	for _, proxyURL := range candidates {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			ok := f.testCandidate(ctx, p, targetURL)
			results <- result{proxyURL: p, ok: ok}
		}(proxyURL)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var working string
	for r := range results {
		if r.ok {
			working = r.proxyURL
			break
		}
	}

	f.mu.Lock()
	f.cache[key] = &fallbackEntry{proxyURL: working, expiresAt: time.Now().Add(fallbackCacheTTL)}
	f.mu.Unlock()

	if working == "" {
		return nil, "", nil
	}
	tr, err := NewTransport(f.opts, working)
	return tr, working, err
}

// collectCandidates gathers proxies from active pools + env, deduplicated.
func (f *Fallback) collectCandidates(ctx context.Context, targetURL string) ([]string, error) {
	seen := make(map[string]struct{})
	var candidates []string

	if f.source != nil {
		pools, err := f.source.List(ctx, true)
		if err == nil {
			for _, p := range pools {
				url := p.ProxyURL
				if url == "" && p.Type != "" && p.Host != "" {
					port := p.Port
					if port == "" {
						port = defaultPortForScheme(p.Type)
					}
					url = fmt.Sprintf("%s://%s:%s", p.Type, p.Host, port)
				}
				if parsed, err := NormalizeProxyURL(url); err == nil {
					url = fmt.Sprintf("%s://%s%s:%s", parsed.Scheme, parsed.Host, formatAuth(parsed.Username, parsed.Password), parsed.Port)
				}
				if _, dup := seen[url]; url != "" && !dup {
					seen[url] = struct{}{}
					candidates = append(candidates, url)
				}
			}
		}
	}

	envProxy := resolveEnvProxyURL(targetURL)
	if envProxy != "" {
		if _, dup := seen[envProxy]; !dup {
			seen[envProxy] = struct{}{}
			candidates = append(candidates, envProxy)
		}
	}

	sort.Strings(candidates)
	return candidates, nil
}

// testCandidate first fast-fails dead TCP ports, then runs a HEAD probe.
func (f *Fallback) testCandidate(ctx context.Context, proxyURL, targetURL string) bool {
	if err := FastFail(ctx, f.opts, proxyURL); err != nil {
		return false
	}
	tr, err := NewTransport(f.opts, proxyURL)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: f.opts.ProxyFallbackProbeTimeout, Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, targetURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "9gouter/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
}

// resolveEnvProxyURL returns the env proxy for a target, honouring NO_PROXY.
func resolveEnvProxyURL(targetURL string) string {
	u, err := url.Parse(targetURL)
	if err != nil {
		return ""
	}
	if noProxyMatch(u.Hostname(), firstEnv("NO_PROXY", "no_proxy")) {
		return ""
	}
	var raw string
	if u.Scheme == "https" {
		raw = firstEnv("HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy")
	} else {
		raw = firstEnv("HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy")
	}
	if raw == "" {
		return ""
	}
	parsed, err := NormalizeProxyURL(raw)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s://%s%s:%s", parsed.Scheme, parsed.Host, formatAuth(parsed.Username, parsed.Password), parsed.Port)
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := envLookup(n); v != "" {
			return v
		}
	}
	return ""
}

// envLookup is overridable in tests.
var envLookup = getEnv

func getEnv(key string) string { return envGetter(key) }

// envGetter is the real os.Getenv wrapper; separated to allow test overrides.
var envGetter = func(key string) string {
	return os.Getenv(key)
}

// resetFallbackCache is exported for tests.
func (f *Fallback) resetFallbackCache() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cache = make(map[string]*fallbackEntry)
}
