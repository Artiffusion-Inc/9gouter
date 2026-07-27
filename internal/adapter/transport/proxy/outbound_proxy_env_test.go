package proxy

import (
	"os"
	"testing"
)

// saveEnv snapshots/restores the env keys ApplyOutboundProxyEnv touches so the
// tests are hermetic regardless of the operator environment.
func saveEnv(t *testing.T) {
	t.Helper()
	keys := []string{envHTTPProxy, envHTTPSProxy, envAllProxy, envNoProxy, envManaged, envManagedURL, envManagedNoPrx}
	saved := map[string]string{}
	for _, k := range keys {
		saved[k], _ = os.LookupEnv(k)
		os.Unsetenv(k)
	}
	t.Cleanup(func() {
		for _, k := range keys {
			if v, ok := saved[k]; ok {
				os.Setenv(k, v)
			} else {
				os.Unsetenv(k)
			}
		}
	})
}

func TestApplyOutboundProxyEnv_DisabledClearsManaged(t *testing.T) {
	saveEnv(t)
	// Pretend a prior run managed these.
	os.Setenv(envHTTPProxy, "http://old:8080")
	os.Setenv(envHTTPSProxy, "http://old:8080")
	os.Setenv(envAllProxy, "http://old:8080")
	os.Setenv(envNoProxy, "example.com")
	os.Setenv(envManaged, "1")
	os.Setenv(envManagedURL, "http://old:8080")
	os.Setenv(envManagedNoPrx, "example.com")

	ApplyOutboundProxyEnv(OutboundProxyConfig{Enabled: false})

	for _, k := range []string{envHTTPProxy, envHTTPSProxy, envAllProxy, envNoProxy, envManaged, envManagedURL, envManagedNoPrx} {
		if _, ok := os.LookupEnv(k); ok {
			t.Fatalf("disabled+managed should clear %s", k)
		}
	}
}

func TestApplyOutboundProxyEnv_DisabledLeavesOperatorEnv(t *testing.T) {
	saveEnv(t)
	// Operator-supplied env, not managed.
	os.Setenv(envHTTPProxy, "http://operator:8080")
	os.Setenv(envHTTPSProxy, "http://operator:8080")

	ApplyOutboundProxyEnv(OutboundProxyConfig{Enabled: false})

	if got := os.Getenv(envHTTPProxy); got != "http://operator:8080" {
		t.Fatalf("disabled+unmanaged should leave operator HTTP_PROXY, got %q", got)
	}
	if os.Getenv(envManaged) != "" {
		t.Fatalf("disabled+unmanaged should not mark managed")
	}
}

func TestApplyOutboundProxyEnv_EnabledWritesAllSchemes(t *testing.T) {
	saveEnv(t)
	ApplyOutboundProxyEnv(OutboundProxyConfig{Enabled: true, ProxyURL: "http://proxy.local:3128"})

	want := "http://proxy.local:3128"
	for _, k := range []string{envHTTPProxy, envHTTPSProxy, envAllProxy, envManagedURL} {
		if got := os.Getenv(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
	if os.Getenv(envManaged) != "1" {
		t.Fatalf("enabled should mark managed")
	}
}

func TestApplyOutboundProxyEnv_EnabledWritesNoProxy(t *testing.T) {
	saveEnv(t)
	ApplyOutboundProxyEnv(OutboundProxyConfig{Enabled: true, NoProxy: "localhost,127.0.0.1"})

	if got := os.Getenv(envNoProxy); got != "localhost,127.0.0.1" {
		t.Fatalf("NO_PROXY = %q", got)
	}
	if got := os.Getenv(envManagedNoPrx); got != "localhost,127.0.0.1" {
		t.Fatalf("NINE_ROUTER_NO_PROXY = %q", got)
	}
	if os.Getenv(envManaged) != "1" {
		t.Fatalf("enabled should mark managed")
	}
}

func TestApplyOutboundProxyEnv_RejectsBadScheme(t *testing.T) {
	saveEnv(t)
	ApplyOutboundProxyEnv(OutboundProxyConfig{Enabled: true, ProxyURL: "ftp://bad:21"})

	for _, k := range []string{envHTTPProxy, envHTTPSProxy, envAllProxy, envManagedURL} {
		if _, ok := os.LookupEnv(k); ok {
			t.Fatalf("bad scheme should not write %s", k)
		}
	}
	if os.Getenv(envManaged) == "1" {
		t.Fatalf("rejected proxy should not mark managed")
	}
}

func TestApplyOutboundProxyEnv_RejectsControlChars(t *testing.T) {
	saveEnv(t)
	ApplyOutboundProxyEnv(OutboundProxyConfig{Enabled: true, ProxyURL: "http://ev\ril:8080"})
	if _, ok := os.LookupEnv(envHTTPProxy); ok {
		t.Fatalf("control-char URL must be rejected")
	}
}

func TestApplyOutboundProxyEnv_EnabledEmptyFieldsLeaveOperatorEnv(t *testing.T) {
	saveEnv(t)
	os.Setenv(envHTTPProxy, "http://operator:8080")

	ApplyOutboundProxyEnv(OutboundProxyConfig{Enabled: true}) // both fields empty

	if got := os.Getenv(envHTTPProxy); got != "http://operator:8080" {
		t.Fatalf("enabled-but-empty should leave operator env, got %q", got)
	}
	if os.Getenv(envManaged) == "1" {
		t.Fatalf("enabled-but-empty should not mark managed")
	}
}

func TestApplyOutboundProxyEnv_ClearingFieldWhileManaged(t *testing.T) {
	saveEnv(t)
	// First enable with a URL.
	ApplyOutboundProxyEnv(OutboundProxyConfig{Enabled: true, ProxyURL: "http://first:8080"})
	if os.Getenv(envHTTPProxy) != "http://first:8080" {
		t.Fatalf("setup: first apply failed")
	}
	// Now enabled but URL cleared: should clear the managed proxy env.
	ApplyOutboundProxyEnv(OutboundProxyConfig{Enabled: true, ProxyURL: ""})
	for _, k := range []string{envHTTPProxy, envHTTPSProxy, envAllProxy, envManagedURL} {
		if _, ok := os.LookupEnv(k); ok {
			t.Fatalf("clearing managed URL should clear %s", k)
		}
	}
	// Still managed marker absent because nothing left to manage.
	if os.Getenv(envManaged) == "1" {
		t.Fatalf("nothing left to manage should drop the marker")
	}
}

func TestApplyOutboundProxyEnv_Socks5Scheme(t *testing.T) {
	saveEnv(t)
	ApplyOutboundProxyEnv(OutboundProxyConfig{Enabled: true, ProxyURL: "socks5://127.0.0.1:1080"})
	want := "socks5://127.0.0.1:1080"
	if got := os.Getenv(envAllProxy); got != want {
		t.Fatalf("socks5 ALL_PROXY = %q, want %q", got, want)
	}
}
