package proxy

import (
	"errors"
	"net"
	"testing"
)

func TestDescribeFetchCauseOpError(t *testing.T) {
	opErr := &net.OpError{
		Op:   "dial",
		Net:  "tcp",
		Err:  errors.New("connection refused"),
		Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1},
	}
	s := DescribeFetchCause(opErr)
	if s == "" {
		t.Fatal("expected non-empty description")
	}
	if !containsAny(s, "dial", "connection refused") {
		t.Fatalf("unexpected description: %s", s)
	}
}

func TestDescribeFetchCausePlainError(t *testing.T) {
	err := errors.New("something failed")
	s := DescribeFetchCause(err)
	if s != "something failed" {
		t.Fatalf("got %q", s)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if contains(s, sub) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || findSubstr(s, sub))
}

func findSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
