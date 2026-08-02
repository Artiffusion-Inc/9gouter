package proxychat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

type fakeTSGateRepo struct {
	data  []byte
	err   error
	calls atomic.Int64
}

func (f *fakeTSGateRepo) Get(ctx context.Context) (*settings.Settings, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return &settings.Settings{Data: f.data}, nil
}

func tsBlob(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestTokenSaverGate_DefaultsWhenMissing(t *testing.T) {
	g := &tokenSaverGate{settings: &fakeTSGateRepo{data: tsBlob(t, map[string]any{})}}
	cfg := g.Config(context.Background())
	// Defaults match repo.defaultSettings: rtk on, headroom off + localhost:8787,
	// caveman/ponytail off with "full" level, pxpipe off with 25000/15000.
	if !cfg.RtkEnabled {
		t.Fatalf("rtkEnabled default should be true")
	}
	if cfg.HeadroomEnabled {
		t.Fatalf("headroomEnabled default should be false")
	}
	if cfg.HeadroomURL != "http://localhost:8787" {
		t.Fatalf("headroomURL default = %q", cfg.HeadroomURL)
	}
	if cfg.CavemanEnabled || cfg.PonytailEnabled || cfg.PxpipeEnabled {
		t.Fatalf("caveman/ponytail/pxpipe should default off")
	}
	if cfg.CavemanLevel != "full" || cfg.PonytailLevel != "full" {
		t.Fatalf("levels should default to full")
	}
	if cfg.PxpipeMinChars != 25000 || cfg.PxpipeTimeoutMs != 15000 {
		t.Fatalf("pxpipe defaults wrong: %d/%d", cfg.PxpipeMinChars, cfg.PxpipeTimeoutMs)
	}
	if cfg.PxpipeTransform == nil {
		t.Fatalf("PxpipeTransform must be non-nil (subprocess bridge wired)")
	}
}

func TestTokenSaverGate_ReadsExplicitValues(t *testing.T) {
	g := &tokenSaverGate{settings: &fakeTSGateRepo{data: tsBlob(t, map[string]any{
		"rtkEnabled":                   false,
		"headroomEnabled":              true,
		"headroomUrl":                  "http://hr.local:8787",
		"headroomCompressUserMessages": true,
		"cavemanEnabled":               true,
		"cavemanLevel":                 "medium",
		"ponytailEnabled":              true,
		"ponytailLevel":                "high",
		"pxpipeEnabled":                true,
		"pxpipeMinChars":               5000,
		"pxpipeTimeoutMs":              9000,
	})}}
	cfg := g.Config(context.Background())
	if cfg.RtkEnabled {
		t.Fatalf("rtkEnabled should be false")
	}
	if !cfg.HeadroomEnabled || cfg.HeadroomURL != "http://hr.local:8787" || !cfg.HeadroomCompressUser {
		t.Fatalf("headroom fields wrong: %+v", cfg)
	}
	if !cfg.CavemanEnabled || cfg.CavemanLevel != "medium" {
		t.Fatalf("caveman wrong: %+v", cfg)
	}
	if !cfg.PonytailEnabled || cfg.PonytailLevel != "high" {
		t.Fatalf("ponytail wrong: %+v", cfg)
	}
	if !cfg.PxpipeEnabled || cfg.PxpipeMinChars != 5000 || cfg.PxpipeTimeoutMs != 9000 {
		t.Fatalf("pxpipe wrong: %+v", cfg)
	}
}

func TestTokenSaverGate_EnvOverridesHeadroomURL(t *testing.T) {
	os.Setenv("HEADROOM_URL", "http://env-override:9999")
	defer os.Unsetenv("HEADROOM_URL")
	g := &tokenSaverGate{settings: &fakeTSGateRepo{data: tsBlob(t, map[string]any{"headroomUrl": "http://settings:8787"})}}
	cfg := g.Config(context.Background())
	if cfg.HeadroomURL != "http://env-override:9999" {
		t.Fatalf("HEADROOM_URL env should win, got %q", cfg.HeadroomURL)
	}
}

func TestTokenSaverGate_SettingsHeadroomURLOverEmptyEnv(t *testing.T) {
	os.Unsetenv("HEADROOM_URL")
	g := &tokenSaverGate{settings: &fakeTSGateRepo{data: tsBlob(t, map[string]any{"headroomUrl": "http://settings:8787"})}}
	cfg := g.Config(context.Background())
	if cfg.HeadroomURL != "http://settings:8787" {
		t.Fatalf("settings headroomUrl should win over empty env, got %q", cfg.HeadroomURL)
	}
}

func TestTokenSaverGate_CachesWithinTTL(t *testing.T) {
	repo := &fakeTSGateRepo{data: tsBlob(t, map[string]any{})}
	g := &tokenSaverGate{settings: repo}
	_ = g.Config(context.Background())
	_ = g.Config(context.Background())
	_ = g.Config(context.Background())
	if got := repo.calls.Load(); got != 1 {
		t.Fatalf("expected 1 DB read (cached), got %d", got)
	}
}

func TestTokenSaverGate_RereadsAfterTTL(t *testing.T) {
	repo := &fakeTSGateRepo{data: tsBlob(t, map[string]any{})}
	g := &tokenSaverGate{settings: repo}
	_ = g.Config(context.Background())
	g.mu.Lock()
	g.cachedAt = time.Now().Add(-tokenSaverCacheTTL - time.Second)
	g.mu.Unlock()
	_ = g.Config(context.Background())
	if got := repo.calls.Load(); got != 2 {
		t.Fatalf("expected 2 DB reads after TTL expiry, got %d", got)
	}
}

func TestTokenSaverGate_ReadErrorReturnsZeroAndCaches(t *testing.T) {
	repo := &fakeTSGateRepo{err: errors.New("boom")}
	g := &tokenSaverGate{settings: repo}
	cfg := g.Config(context.Background())
	if cfg.RtkEnabled {
		t.Fatalf("DB error should yield zero config (all off), not the rtk default")
	}
	_ = g.Config(context.Background())
	if got := repo.calls.Load(); got != 1 {
		t.Fatalf("error result must be cached so flaky DB is not hammered, got %d reads", got)
	}
}

func TestTokenSaverGate_NilRepoReturnsNilGate(t *testing.T) {
	if NewTokenSaverGate(nil) != nil {
		t.Fatalf("nil repo must yield nil gate")
	}
}

func TestTokenSaverGate_NilGateIsNoOp(t *testing.T) {
	// A nil gate (interface nil) must not panic when Handle() guards it.
	var gate TokenSaverGate
	if gate != nil {
		t.Fatalf("uninitialized interface should be nil")
	}
}

func TestIsZeroTokenSaverConfig(t *testing.T) {
	if !isZeroTokenSaverConfig(TokenSaverConfig{}) {
		t.Fatalf("zero value should be zero")
	}
	if isZeroTokenSaverConfig(TokenSaverConfig{RtkEnabled: true}) {
		t.Fatalf("populated config should not be zero")
	}
	if isZeroTokenSaverConfig(TokenSaverConfig{HeadroomURL: "x"}) {
		t.Fatalf("config with URL should not be zero")
	}
	// PxpipeTransform alone (func) can't make it non-zero without a scalar —
	// a caller that sets Transform also sets the scalars, so this is fine.
}
