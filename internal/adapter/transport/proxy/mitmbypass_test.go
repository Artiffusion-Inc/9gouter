package proxy

import (
	"strings"
	"testing"
)

func TestShouldBypassMitmDns(t *testing.T) {
	for _, h := range defaultBypassHosts {
		if !shouldBypassMitmDns(h) {
			t.Fatalf("expected %s to match", h)
		}
		if !shouldBypassMitmDns("sub." + h) {
			t.Fatalf("expected sub.%s to match", h)
		}
	}
	if shouldBypassMitmDns("example.com") {
		t.Fatal("expected example.com not to match")
	}
}

func TestMITMBypassResolveNotInList(t *testing.T) {
	_, err := MITMBypassResolve("example.com")
	if err == nil {
		t.Fatal("expected error for non-bypass host")
	}
}

func TestMITMBypassResolveGoogle(t *testing.T) {
	// googleapis.com is in the default list; network may or may not be available.
	ip, err := MITMBypassResolve("cloudcode-pa.googleapis.com")
	if err != nil {
		t.Skipf("DNS not reachable in test environment: %v", err)
	}
	if ip == nil || ip.String() == "" {
		t.Fatal("expected IP")
	}
	if !strings.Contains(ip.String(), ".") {
		t.Fatalf("expected IPv4, got %s", ip.String())
	}
}
