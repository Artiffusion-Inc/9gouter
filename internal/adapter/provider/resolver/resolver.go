// Package resolver ports the per-provider live-model resolvers from
// open-sse/services/*Models.js. Each resolver fetches the live model catalog
// for an authenticated connection and expands it into 9router-shaped model
// entries (with synthetic -thinking / -agentic variants where the provider
// supports them). Resolvers are read-only side-channels used by GET /v1/models
// to keep the catalog fresh without a static catalog.
//
// On any failure (network, 4xx/5xx, missing credentials) a resolver returns
// nil and the caller falls back to the provider's static catalog, so a broken
// live resolver never breaks /v1/models.
package resolver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// ResolvedModel is one catalog entry produced by a live resolver. It mirrors
// the JS `PROVIDER_MODELS` entry shape plus the capability flags the JS
// buildVariants populated so /v1/models can report thinking/agentic support.
type ResolvedModel struct {
	ID            string
	Name          string
	Kind          string
	ContextLength int
	// Capabilities is nil for providers that do not synthesize variants.
	Capabilities *Capabilities
	// UpstreamModelID is the raw upstream model id before variant expansion
	// (the -thinking/-agentic suffixes are 9router fictions; the translator
	// strips them before the request leaves the process).
	UpstreamModelID string
}

// Capabilities flags the synthetic variant a model represents.
type Capabilities struct {
	Thinking bool
	Agentic  bool
}

// Result is what a resolver returns on success.
type Result struct {
	Models    []ResolvedModel
	RawModels []map[string]any
}

// LiveModelResolver fetches the live catalog for one provider connection.
// Resolve returns (nil, nil) when the credentials are insufficient (no
// accessToken) — callers treat that as "no live catalog, use static".
// An error means the fetch was attempted and failed; callers still fall back
// to static, but may log the error.
type LiveModelResolver interface {
	// ProviderID is the canonical provider id this resolver serves
	// ("kiro", "github", "grok-cli", ...).
	ProviderID() string
	// Resolve fetches the live catalog. ctx carries the request deadline;
	// resolvers MUST honor it. opts carries the credential refresh hook.
	Resolve(ctx context.Context, creds provider.Credentials, opts ResolveOpts) (*Result, error)
}

// ResolveOpts carries callbacks a resolver needs to persist refreshed
// credentials and log diagnostics — mirroring the JS options bag.
type ResolveOpts struct {
	Logger               Logger
	OnCredentialsRefreshed func(RefreshedCredentials) error
	// ProxyOptions is reserved for resolvers that fetch through the proxy
	// stack (grok-cli). Populated by the caller from the connection's
	// resolved proxy config.
	ProxyOptions ProxyOptions
}

// RefreshedCredentials is the token-refresh result a TokenRefresher returns.
type RefreshedCredentials struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	// IDToken is returned by providers that issue OpenID Connect id_tokens
	// alongside the access token (codex, xai). Carried through so the
	// OnCredentialsRefreshed hook can persist it into the connection.
	IDToken string
	// ExpiresAt is an RFC3339 timestamp returned by GitHub Copilot's
	// token exchange (which returns expires_at, not expires_in). Carried
	// through verbatim; the hook parses it if non-empty.
	ExpiresAt string
	// APIKey is set by providers whose "refresh" actually mints/rotates an
	// API key (rare). Merged back into the connection by the refresh hook.
	APIKey string
	// Token is a generic bearer some providers return alongside or instead
	// of access_token (e.g. copilotToken). Mirrors the JS mergeRefreshedCredentials
	// `token` field.
	Token string
	// ProjectID is returned by antigravity/gemini-cli refresh and merged back
	// into the connection's providerSpecificData. Mirrors the JS
	// mergeRefreshedCredentials `projectId` field (#2703 Fix 2e).
	ProjectID string
	// CopilotToken and CopilotTokenExpiresAt carry GitHub Copilot's rotated
	// session token (the JS flow persists both via the refresh hook).
	CopilotToken         string
	CopilotTokenExpiresAt string
	// ProviderSpecificData carries provider-specific fields the caller must
	// merge back into the connection (e.g. qwen resource_url, kiro
	// profileArn). nil/empty means "no patch".
	ProviderSpecificData map[string]any
	// Unrecoverable is set when the upstream signals the refresh token is
	// permanently invalid (invalid_grant / refresh_token_expired / reused).
	// The caller should mark the connection as needing re-auth rather than
	// retry. When true, AccessToken is empty and the error path is used.
	Unrecoverable bool
}

// ProxyOptions is the per-connection proxy subset a resolver or token refresher
// may need to route its upstream call through the proxy stack. Populated by the
// caller from the connection's resolved proxy config (resolveConnectionProxyConfig)
// and threaded through ResolveOpts and TokenRefresher.Refresh (#2703 Fix 2a —
// route-aware refresh). It maps 1:1 onto proxy.ProxyFetchOptions.
type ProxyOptions struct {
	ConnectionProxyEnabled bool
	ConnectionProxyURL     string
	ConnectionNoProxy      string
	VercelRelayURL         string
	StrictProxy            bool
	// Logger receives structured route-diagnostics lines from the proxy stack
	// during a refresh. When nil, diagnostics are dropped. Mirrors
	// proxy.ProxyFetchOptions.Logger (#2703 Fix 5).
	Logger Logger
}

// Logger is the minimal logger interface resolvers use; slog.Logger satisfies
// it via a thin adapter at the call site.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Debug(msg string, args ...any)
}

// TokenRefresher refreshes an expired access token for a provider. The kiro
// resolver uses it on a 401 from ListAvailableModels. Implementations live
// in the tokenRefresh subsystem (T027); resolvers that hit 401/403 inject
// the provider's refresher via this interface so they can retry once after
// refreshing.
//
// #2703 Fix 2a: Refresh is route-aware. The caller passes the connection's
// resolved ProxyOptions so the refresh HTTP call goes through the same proxy
// stack as the chat/catalog call (ProxyAwareFetch), instead of a plain
// http.Client that ignores per-connection proxy config. opts carries proxy
// config; an empty ProxyOptions means "direct" (the refresh endpoints of most
// providers are not themselves behind the customer proxy, but the option
// must be honored when set, e.g. kiro behind a strict proxy).
type TokenRefresher interface {
	Refresh(ctx context.Context, refreshToken string, psd map[string]any, opts ProxyOptions, log Logger) (*RefreshedCredentials, error)
}

// stableHash returns the hex sha256 of seed, used by resolvers to build a
// stable per-credential cache key (different login sessions for the same
// account share an entry).
func stableHash(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// Cache is a per-credential TTL cache of resolved catalogs, mirroring the JS
// `catalogCache: Map<string, { expiresAt, models }>`. It is safe for
// concurrent use (mu guards the store map). Keyed by a stable per-credential
// hash (resolver-defined).
type Cache struct {
	ttl   time.Duration
	mu    sync.RWMutex
	store map[string]cacheEntry
}

type cacheEntry struct {
	expiresAt time.Time
	result    *Result
}

// NewCache returns a Cache with the given per-entry TTL.
func NewCache(ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Cache{ttl: ttl, store: map[string]cacheEntry{}}
}

// Get returns the cached result for key if present and unexpired.
func (c *Cache) Get(key string) (*Result, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if e, ok := c.store[key]; ok && time.Now().Before(e.expiresAt) {
		return e.result, true
	}
	return nil, false
}

// Set stores a result under key with the cache TTL.
func (c *Cache) Set(key string, result *Result) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store[key] = cacheEntry{expiresAt: time.Now().Add(c.ttl), result: result}
}

// Invalidate drops the cached entry for key (call after token rotation).
func (c *Cache) Invalidate(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.store, key)
}

// Clear drops the entire cache (tests / manual debug).
func (c *Cache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store = map[string]cacheEntry{}
}

// registry is the set of registered live resolvers, keyed by provider id.
var registry = map[string]LiveModelResolver{}

// Register adds a live resolver to the registry. Called from each resolver
// subpackage's init().
func Register(r LiveModelResolver) {
	if r == nil {
		return
	}
	registry[r.ProviderID()] = r
}

// Unregister removes a live resolver from the registry by provider id. Tests
// use it to clean up mock resolvers so the global registry does not leak
// between test cases. Production code does not call it.
func Unregister(providerID string) {
	delete(registry, providerID)
}

// ResetRegistry clears all live resolvers. Test-only.
func ResetRegistry() {
	registry = map[string]LiveModelResolver{}
}

// Lookup returns the live resolver for a provider id, or nil if none is
// registered.
func Lookup(providerID string) LiveModelResolver {
	return registry[providerID]
}

// Has reports whether a live resolver is registered for the provider.
func Has(providerID string) bool {
	_, ok := registry[providerID]
	return ok
}