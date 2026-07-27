package proxy

import (
	"net/url"
	"os"
	"strings"
)

// OutboundProxyConfig is the settings surface applyOutboundProxyEnv needs.
// Mirrors the legacy src/lib/network/outboundProxy.js applyOutboundProxyEnv
// arguments: the dashboard "Outbound proxy" panel fields.
type OutboundProxyConfig struct {
	Enabled  bool
	ProxyURL string
	NoProxy  string
}

// Env keys managed by ApplyOutboundProxyEnv. They match the JS side exactly so
// that resolveEnvProxyURL (which reads HTTP_PROXY/HTTPS_PROXY/ALL_PROXY/NO_PROXY)
// honours them without any extra wiring.
const (
	envHTTPProxy    = "HTTP_PROXY"
	envHTTPSProxy   = "HTTPS_PROXY"
	envAllProxy     = "ALL_PROXY"
	envNoProxy      = "NO_PROXY"
	envManaged      = "NINE_ROUTER_PROXY_MANAGED"
	envManagedURL   = "NINE_ROUTER_PROXY_URL"
	envManagedNoPrx = "NINE_ROUTER_NO_PROXY"
)

// allowedProxySchemes mirrors ALLOWED_PROXY_SCHEMES in outboundProxy.js.
var allowedProxySchemes = map[string]bool{
	"http":    true,
	"https":   true,
	"socks5":  true,
	"socks4":  true,
	"socks5h": true,
	"socks4a": true,
}

// validateProxyURL mirrors validateProxyUrl: rejects empty/control-char URLs,
// requires an allowed scheme, returns the normalized href or "" on failure.
func validateProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.ContainsAny(raw, "\n\r`$") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return ""
	}
	if !allowedProxySchemes[strings.ToLower(u.Scheme)] {
		return ""
	}
	return u.String()
}

// ApplyOutboundProxyEnv ports src/lib/network/outboundProxy.js applyOutboundProxyEnv
// 1:1. It mutates the process environment so the proxy stack's resolveEnvProxyURL
// (which reads HTTP_PROXY/HTTPS_PROXY/ALL_PROXY/NO_PROXY) honours the dashboard
// "Outbound proxy" settings on every outbound fetch — without threading a new
// field through every ProxyFetchOptions call site.
//
// State machine (NINE_ROUTER_PROXY_MANAGED = "1" marks "we set these env vars"):
//
//   - disabled: only clear env vars previously managed by us, then return.
//   - enabled + proxyURL: write HTTP_PROXY/HTTPS_PROXY/ALL_PROXY (validated),
//     mark managed.
//   - enabled + noProxy: write NO_PROXY, mark managed.
//   - enabled but a field empty we previously managed: clear only that field,
//     leaving operator-supplied env untouched.
//
// When enabled but neither field is set, the operator's own env is left in place
// (the "do not touch externally-provided env" branch in the JS).
func ApplyOutboundProxyEnv(cfg OutboundProxyConfig) {
	proxyURL := strings.TrimSpace(cfg.ProxyURL)
	noProxy := strings.TrimSpace(cfg.NoProxy)
	wasManaged := os.Getenv(envManaged) == "1"

	if !cfg.Enabled {
		if wasManaged {
			os.Unsetenv(envHTTPProxy)
			os.Unsetenv(envHTTPSProxy)
			os.Unsetenv(envAllProxy)
			os.Unsetenv(envNoProxy)
			os.Unsetenv(envManaged)
			os.Unsetenv(envManagedURL)
			os.Unsetenv(envManagedNoPrx)
		}
		return
	}

	managed := false

	// Clear fields we previously managed but that are now empty, so an
	// operator clearing the dashboard field actually disables it rather than
	// inheriting the stale managed value.
	if wasManaged {
		if proxyURL == "" {
			os.Unsetenv(envHTTPProxy)
			os.Unsetenv(envHTTPSProxy)
			os.Unsetenv(envAllProxy)
			os.Unsetenv(envManagedURL)
		}
		if noProxy == "" {
			os.Unsetenv(envNoProxy)
			os.Unsetenv(envManagedNoPrx)
		}
	}

	if proxyURL != "" {
		if validated := validateProxyURL(proxyURL); validated != "" {
			os.Setenv(envHTTPProxy, validated)
			os.Setenv(envHTTPSProxy, validated)
			os.Setenv(envAllProxy, validated)
			os.Setenv(envManagedURL, validated)
			managed = true
		}
	}

	if noProxy != "" {
		os.Setenv(envNoProxy, noProxy)
		os.Setenv(envManagedNoPrx, noProxy)
		managed = true
	}

	if managed {
		os.Setenv(envManaged, "1")
	} else if wasManaged {
		// We previously managed env but now have nothing to manage: drop the
		// marker so a later operator-supplied value is not cleared on disable.
		os.Unsetenv(envManaged)
	}
}
