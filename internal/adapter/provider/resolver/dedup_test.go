package resolver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

func newDedup() RefreshDedup { return &refreshDedup{} }

func TestRefreshDedup_EmptyKeyBypasses(t *testing.T) {
	var calls int32
	d := newDedup()
	_, err := d.Refresh(context.Background(), "", func() (*RefreshedCredentials, error) {
		atomic.AddInt32(&calls, 1)
		return &RefreshedCredentials{AccessToken: "at"}, nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	_, _ = d.Refresh(context.Background(), "", func() (*RefreshedCredentials, error) {
		atomic.AddInt32(&calls, 1)
		return &RefreshedCredentials{AccessToken: "at"}, nil
	})
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("empty key must not cache; expected 2 calls, got %d", calls)
	}
}

func TestRefreshDedup_CoalescesConcurrent(t *testing.T) {
	d := newDedup()
	var calls int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	const n = 10
	results := make([]*RefreshedCredentials, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := d.Refresh(context.Background(), "conn-a", func() (*RefreshedCredentials, error) {
				atomic.AddInt32(&calls, 1)
				<-release
				return &RefreshedCredentials{AccessToken: "at"}, nil
			})
			results[i] = r
			errs[i] = err
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("concurrent same-key refreshes must coalesce into 1 call, got %d", calls)
	}
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("caller %d err: %v", i, errs[i])
		}
		if results[i] == nil || results[i].AccessToken != "at" {
			t.Fatalf("caller %d got %v", i, results[i])
		}
	}
}

func TestRefreshDedup_ReusesRecentResult(t *testing.T) {
	d := newDedup()
	var calls int32
	call := func() (*RefreshedCredentials, error) {
		atomic.AddInt32(&calls, 1)
		return &RefreshedCredentials{AccessToken: "at"}, nil
	}
	if _, err := d.Refresh(context.Background(), "conn-b", call); err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, err := d.Refresh(context.Background(), "conn-b", call); err != nil {
		t.Fatalf("err: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("second same-key call within TTL must reuse cached result, got %d calls", calls)
	}
}

func TestRefreshDedup_FailedRefreshNotCached(t *testing.T) {
	d := newDedup()
	var calls int32
	fail := errors.New("boom")
	_, err := d.Refresh(context.Background(), "conn-c", func() (*RefreshedCredentials, error) {
		atomic.AddInt32(&calls, 1)
		return nil, fail
	})
	if !errors.Is(err, fail) {
		t.Fatalf("first call err = %v, want boom", err)
	}
	_, err = d.Refresh(context.Background(), "conn-c", func() (*RefreshedCredentials, error) {
		atomic.AddInt32(&calls, 1)
		return &RefreshedCredentials{AccessToken: "at"}, nil
	})
	if err != nil {
		t.Fatalf("retry err: %v", err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("failed refresh must not cache; expected 2 calls, got %d", calls)
	}
}

func TestRefreshDedup_ContextCancelWaitingCaller(t *testing.T) {
	d := newDedup()
	release := make(chan struct{})
	leaderDone := make(chan struct{})
	go func() {
		_, _ = d.Refresh(context.Background(), "conn-d", func() (*RefreshedCredentials, error) {
			<-release
			return &RefreshedCredentials{AccessToken: "at"}, nil
		})
		close(leaderDone)
	}()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := d.Refresh(ctx, "conn-d", func() (*RefreshedCredentials, error) {
		t.Fatal("waiting caller should not run fn")
		return nil, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting caller err = %v, want DeadlineExceeded", err)
	}
	close(release)
	<-leaderDone
}

func TestRefreshKey(t *testing.T) {
	if k := RefreshKey(provider.Credentials{ProviderSpecificData: map[string]any{"connectionId": "c-1"}}); k != "c-1" {
		t.Fatalf("connectionId wins, got %q", k)
	}
	if k := RefreshKey(provider.Credentials{ProviderSpecificData: map[string]any{"email": "a@b"}}); k != "a@b" {
		t.Fatalf("email fallback, got %q", k)
	}
	long := "abcdefghijklmnopqrstuvwxyz0123456789"
	if k := RefreshKey(provider.Credentials{ProviderSpecificData: map[string]any{"refreshToken": long}}); k != long[len(long)-16:] {
		t.Fatalf("refreshToken suffix fallback, got %q", k)
	}
	if k := RefreshKey(provider.Credentials{ProviderSpecificData: map[string]any{"refreshToken": "short"}}); k != "short" {
		t.Fatalf("short refreshToken used verbatim, got %q", k)
	}
	if k := RefreshKey(provider.Credentials{}); k != "default" {
		t.Fatalf("no identity → default, got %q", k)
	}
}