package config

import (
	"os"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	os.Unsetenv("PORT")
	os.Unsetenv("FETCH_CONNECT_TIMEOUT_MS")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 20127 {
		t.Errorf("Port = %d, want 20127", cfg.Port)
	}
	want := 60 * time.Second
	if cfg.FetchConnectTimeout.Duration() != want {
		t.Errorf("FetchConnectTimeout = %v, want %v", cfg.FetchConnectTimeout.Duration(), want)
	}
	// Verify a bare-ms env value parses as ms, not µs (regression for envconfig time.Duration default bug).
	os.Setenv("FETCH_BODY_TIMEOUT_MS", "600000")
	cfg2, _ := Load()
	if cfg2.FetchBodyTimeout.Duration() != 600*time.Second {
		t.Errorf("FetchBodyTimeout raw-ms env = %v, want 600s", cfg2.FetchBodyTimeout.Duration())
	}
}
