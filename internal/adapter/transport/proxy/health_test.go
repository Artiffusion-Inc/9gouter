package proxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestHealthCacheTTL(t *testing.T) {
	opts := testOptions()
	opts.ProxyHealthCacheTTL = 100 * time.Millisecond
	h := NewHealth(opts)

	h.Set("http://127.0.0.1:1", true)
	if healthy, ok := h.Get("http://127.0.0.1:1"); !ok || !healthy {
		t.Fatalf("expected fresh healthy entry")
	}
	time.Sleep(150 * time.Millisecond)
	if _, ok := h.Get("http://127.0.0.1:1"); ok {
		t.Fatal("expected stale entry")
	}
}

func TestHealthUnhealthyTTL(t *testing.T) {
	opts := testOptions()
	opts.ProxyHealthUnhealthyTTL = 50 * time.Millisecond
	opts.ProxyHealthCacheTTL = 200 * time.Millisecond
	h := NewHealth(opts)

	h.Set("http://127.0.0.1:1", false)
	if _, ok := h.Get("http://127.0.0.1:1"); !ok {
		t.Fatal("expected fresh unhealthy entry")
	}
	time.Sleep(80 * time.Millisecond)
	if _, ok := h.Get("http://127.0.0.1:1"); ok {
		t.Fatal("expected unhealthy entry to expire sooner")
	}
}

func TestHealthProbeDedup(t *testing.T) {
	opts := testOptions()
	h := NewHealth(opts)
	var callsMu sync.Mutex
	calls := 0
	started := make(chan struct{})
	fn := func(ctx context.Context) error {
		callsMu.Lock()
		calls++
		callsMu.Unlock()
		started <- struct{}{}
		// Hold the probe open so concurrent callers collapse into this one.
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
		return errors.New("fail")
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Probe(ctx, "p", fn)
		}()
	}
	<-started
	wg.Wait()
	callsMu.Lock()
	got := calls
	callsMu.Unlock()
	if got != 1 {
		t.Fatalf("expected single probe due to dedup, got %d", got)
	}
}
