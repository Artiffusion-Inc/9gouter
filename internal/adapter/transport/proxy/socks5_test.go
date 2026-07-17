package proxy

import (
	"testing"
)

func TestSocks5TransportBuilds(t *testing.T) {
	opts := testOptions()
	tr, err := Socks5Transport(opts, "socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatalf("Socks5Transport: %v", err)
	}
	if tr.DialContext == nil {
		t.Fatal("expected DialContext")
	}
}

func TestSocks5TransportAuth(t *testing.T) {
	tr, err := Socks5Transport(testOptions(), "socks5://user:pass@127.0.0.1:1080")
	if err != nil {
		t.Fatalf("Socks5Transport: %v", err)
	}
	if tr.DialContext == nil {
		t.Fatal("expected DialContext with auth")
	}
}
