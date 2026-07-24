package api

// codex_proxy_fetch.go ports the proxy-awareness half of 5cc4f222 / #154: the
// JS codex-reset-credits handler routed its upstream GET/POST through
// proxyAwareFetch using the connection's resolved proxy options
// (resolveConnectionProxyConfig → {connectionProxyEnabled, connectionProxyUrl,
// connectionNoProxy, vercelRelayUrl, strictProxy:false}). The Go usage handlers
// (usage_extra.go codexResetCredits) previously used a plain 30s http.Client
// that ignored per-connection proxy config.
//
// connectionProxyFetch builds proxy.ProxyFetchOptions from a connection's data
// blob — first merging the assigned proxy pool (ProxyPools repo) into the psd
// the same way v1.go resolveConnectionProxyConfig does — then runs the request
// through proxy.ProxyAwareFetch. When the dashboard Deps carries no proxy
// config (ProxyOpts zero-value, e.g. in tests that only exercise the direct
// path) the helper falls back to a plain timeout client so the handler still
// works.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// codexUsageTimeout caps the usage-handler upstream call. The proxy stack
// applies its own per-phase timeouts when ProxyOpts is set; this is the
// fallback-client cap.
const codexUsageTimeout = 30 * time.Second

// proxyFetchOptionsForConnection resolves a connection's proxy config the same
// way v1.go resolveConnectionProxyConfig does — connection-level proxy fields
// from the data blob plus the assigned proxy pool's strictProxy/proxyUrl/
// noProxy — and returns proxy.ProxyFetchOptions for ProxyAwareFetch. A nil
// pool repo (tests) skips the pool merge.
func proxyFetchOptionsForConnection(ctx context.Context, pools *repo.ProxyPoolRepo, conn *settings.ProviderConnection) proxy.ProxyFetchOptions {
	opts := proxy.ProxyFetchOptions{}
	if conn == nil {
		return opts
	}
	var data map[string]any
	if err := json.Unmarshal(conn.Data, &data); err != nil {
		return opts
	}
	// Copy the connection-level proxy fields the data blob already carries.
	opts.ConnectionProxyEnabled = psdBoolData(data, "connectionProxyEnabled")
	opts.ConnectionProxyUrl = psdStringData(data, "connectionProxyUrl")
	opts.NoProxy = psdStringData(data, "connectionNoProxy")
	opts.VercelRelayUrl = psdStringData(data, "vercelRelayUrl")

	// Merge the assigned pool (if any) — pool strictProxy always wins; pool
	// proxyUrl/noProxy fill in only when the connection does not set its own.
	poolID, _ := data["proxyPoolId"].(string)
	if poolID != "" && pools != nil {
		pool, err := pools.GetByID(ctx, poolID)
		if err == nil && pool != nil && pool.IsActive {
			var poolData map[string]any
			_ = json.Unmarshal(pool.Data, &poolData)
			if v, ok := poolData["strictProxy"].(bool); ok {
				opts.StrictProxy = v
			}
			if opts.ConnectionProxyUrl == "" {
				if v, ok := poolData["proxyUrl"].(string); ok && v != "" {
					opts.ConnectionProxyUrl = v
					if !opts.ConnectionProxyEnabled {
						opts.ConnectionProxyEnabled = true
					}
				}
			}
			if opts.NoProxy == "" {
				if v, ok := poolData["noProxy"].(string); ok && v != "" {
					opts.NoProxy = v
				}
			}
		}
	}
	return opts
}

// connectionProxyFetch runs req through the proxy stack when the Deps carries
// proxy config (proxy.ProxyAwareFetch), else falls back to a plain timeout
// client. The caller owns closing the returned response body.
func connectionProxyFetch(ctx context.Context, pools *repo.ProxyPoolRepo, proxyOpts proxy.Options, conn *settings.ProviderConnection, req *http.Request) (*http.Response, error) {
	if isProxyOptsZero(proxyOpts) {
		client := &http.Client{Timeout: codexUsageTimeout}
		return client.Do(req)
	}
	client := &http.Client{Timeout: codexUsageTimeout}
	proxyOpts2 := proxyOpts
	return proxy.ProxyAwareFetch(ctx, client, req, proxyOpts2, proxyFetchOptionsForConnection(ctx, pools, conn), nil)
}

// isProxyOptsZero reports whether the proxy Options are unset — the fallback
// signal. FetchConnectTimeout is the leading field OptionsFromConfig populates
// from config, so it is a reliable zero-marker.
func isProxyOptsZero(o proxy.Options) bool {
	return o.FetchConnectTimeout == 0
}

// readBodySafe reads up to 1 MiB of a response body, returning an empty slice
// on any read error (the usage handlers degrade to an empty payload).
func readBodySafe(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, 1<<20))
	return b
}

// psdStringData / psdBoolData are the data-blob twins of v1.go's psdString /
// psdBool (which read from a Credentials.ProviderSpecificData map). Kept
// local to the usage handlers so the api package does not grow a v1->api
// dependency.
func psdStringData(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func psdBoolData(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}
