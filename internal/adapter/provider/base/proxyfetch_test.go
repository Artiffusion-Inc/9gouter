package base

import (
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// TestProxyFetchOptsFromCredsEmptyPassthrough verifies that credentials without
// providerSpecificData return the executor default unchanged — backwards
// compatibility for connections with no assigned proxy pool.
func TestProxyFetchOptsFromCredsEmptyPassthrough(t *testing.T) {
	def := proxy.ProxyFetchOptions{VercelRelayUrl: "https://relay.example"}
	creds := provider.Credentials{}
	got := proxyFetchOptsFromCreds(creds, def)
	if got.VercelRelayUrl != def.VercelRelayUrl {
		t.Fatalf("default relay url dropped: got %q want %q", got.VercelRelayUrl, def.VercelRelayUrl)
	}
	if got.ConnectionProxyEnabled {
		t.Fatalf("connectionProxyEnabled should default to false, got true")
	}
	if got.StrictProxy {
		t.Fatalf("strictProxy should default to false, got true")
	}
}

// TestProxyFetchOptsFromCredsResolvesConnectionFields is the core regression
// test for decolua/9gouter #2703 Fix 1: the connection's resolved proxy fields
// — including strictProxy resolved from the connection's proxyPoolId — must
// reach ProxyFetchOptions so a strict route never falls back to the host's
// direct IP. Before the fix, doFetch sent the empty executor-level
// ProxyFetchOpts and strict mode was silently ignored for normal chat traffic.
func TestProxyFetchOptsFromCredsResolvesConnectionFields(t *testing.T) {
	creds := provider.Credentials{
		ProviderSpecificData: map[string]any{
			"connectionProxyEnabled": true,
			"connectionProxyUrl":     "http://pool-a.example:1080",
			"connectionNoProxy":      "localhost,127.0.0.1",
			"vercelRelayUrl":         "https://relay.example",
			"strictProxy":            true,
		},
	}
	got := proxyFetchOptsFromCreds(creds, proxy.ProxyFetchOptions{})
	if !got.ConnectionProxyEnabled {
		t.Fatalf("connectionProxyEnabled not propagated")
	}
	if got.ConnectionProxyUrl != "http://pool-a.example:1080" {
		t.Fatalf("connectionProxyUrl = %q, want pool-a", got.ConnectionProxyUrl)
	}
	if got.NoProxy != "localhost,127.0.0.1" {
		t.Fatalf("connectionNoProxy = %q, want localhost,127.0.0.1", got.NoProxy)
	}
	if got.VercelRelayUrl != "https://relay.example" {
		t.Fatalf("vercelRelayUrl = %q", got.VercelRelayUrl)
	}
	if !got.StrictProxy {
		t.Fatalf("strictProxy not propagated — strict route would fall back to direct IP (#2703)")
	}
}

// TestProxyFetchOptsFromCredsStrictOnlyOnTrue verifies the defensive `=== true`
// guard: an absent or non-bool strictProxy must NOT enable strict mode, and an
// explicit false must override a true default.
func TestProxyFetchOptsFromCredsStrictOnlyOnTrue(t *testing.T) {
	cases := []struct {
		name     string
		def      bool
		psdValue any
		present  bool
		want     bool
	}{
		{"absent keeps default false", false, nil, false, false},
		{"absent keeps default true", true, nil, false, true},
		{"explicit true overrides false default", false, true, true, true},
		{"explicit false overrides true default", true, false, true, false},
		{"non-bool string does not enable strict", false, "true", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			psd := map[string]any{}
			if c.present {
				psd["strictProxy"] = c.psdValue
			}
			creds := provider.Credentials{ProviderSpecificData: psd}
			def := proxy.ProxyFetchOptions{StrictProxy: c.def}
			got := proxyFetchOptsFromCreds(creds, def)
			if got.StrictProxy != c.want {
				t.Fatalf("strictProxy = %v, want %v", got.StrictProxy, c.want)
			}
		})
	}
}