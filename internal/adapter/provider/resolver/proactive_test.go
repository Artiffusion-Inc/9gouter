package resolver

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestProactiveRefreshIfNeeded_NoRefresher is a no-op when refresher is nil.
func TestProactiveRefreshIfNeeded_NoRefresher(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	data := map[string]any{
		"refreshToken": "rt",
		"expiresAt":    now.Add(1 * time.Minute).Format(time.RFC3339Nano),
	}
	res, err := ProactiveRefreshIfNeeded(context.Background(), "claude", data, nil, ProxyOptions{}, NopLogger(), now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Refreshed || res.Patch != nil {
		t.Fatalf("nil refresher must be a no-op, got %+v", res)
	}
}

// TestProactiveRefreshIfNeeded_NotNearExpiry skips refresh and returns zero.
func TestProactiveRefreshIfNeeded_NotNearExpiry(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	data := map[string]any{
		"refreshToken": "rt",
		"expiresAt":    now.Add(1 * time.Hour).Format(time.RFC3339Nano),
	}
	var calls int32
	wrap := countingWrap(&stubRefresher{token: "at-new"}, &calls)
	res, err := ProactiveRefreshIfNeeded(context.Background(), "claude", data, wrap, ProxyOptions{}, NopLogger(), now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Refreshed || atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("far-from-expiry token must not refresh, got %+v calls=%d", res, calls)
	}
}

// TestProactiveRefreshIfNeeded_RefreshesAndMerges runs the refresh and returns
// a merge patch with the rotated access token + expiresIn-derived expiresAt.
func TestProactiveRefreshIfNeeded_RefreshesAndMerges(t *testing.T) {
	ResetSharedRefreshDedup()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	data := map[string]any{
		"refreshToken": "rt-old",
		"expiresAt":    now.Add(1 * time.Minute).Format(time.RFC3339Nano),
	}
	// Force expiresIn onto the stub result via a custom refresher.
	refresher2 := &tokenRefresherWithExpiry{token: "at-new", expiresIn: 3600}
	res, err := ProactiveRefreshIfNeeded(context.Background(), "claude", data, refresher2, ProxyOptions{}, NopLogger(), now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Refreshed {
		t.Fatal("expected Refreshed=true")
	}
	if res.Patch["accessToken"] != "at-new" {
		t.Errorf("patch accessToken=%v want at-new", res.Patch["accessToken"])
	}
	if res.Patch["refreshToken"] != "rt-old" {
		t.Errorf("patch refreshToken preserved=%v want rt-old", res.Patch["refreshToken"])
	}
	if exp, _ := res.Patch["expiresAt"].(string); !strings.HasPrefix(exp, "2026-07-19T13:") {
		t.Errorf("expiresAt=%q want ~2026-07-19T13:00Z", exp)
	}
	if _, ok := res.Patch["lastRefreshAt"]; !ok {
		t.Error("expected lastRefreshAt stamped on rotated access token")
	}
}

// TestProactiveRefreshIfNeeded_Unrecoverable surfaces the marker.
func TestProactiveRefreshIfNeeded_Unrecoverable(t *testing.T) {
	ResetSharedRefreshDedup()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	data := map[string]any{
		"refreshToken": "rt",
		"expiresAt":    now.Add(1 * time.Minute).Format(time.RFC3339Nano),
	}
	refresher := &unrecoverableRefresher{}
	res, err := ProactiveRefreshIfNeeded(context.Background(), "claude", data, refresher, ProxyOptions{}, NopLogger(), now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Unrecoverable {
		t.Fatalf("expected Unrecoverable=true, got %+v", res)
	}
}

// TestProactiveRefreshIfNeeded_RefreshError propagates the error and does not
// cache the failed refresh (dedup contract: failures are never cached).
func TestProactiveRefreshIfNeeded_RefreshError(t *testing.T) {
	ResetSharedRefreshDedup()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	data := map[string]any{
		"refreshToken": "rt-err",
		"expiresAt":    now.Add(1 * time.Minute).Format(time.RFC3339Nano),
	}
	refresher := &errorRefresher{err: errors.New("upstream 500")}
	_, err := ProactiveRefreshIfNeeded(context.Background(), "claude", data, refresher, ProxyOptions{}, NopLogger(), now)
	if err == nil {
		t.Fatal("expected refresh error to propagate")
	}

	// Second call must retry (failure not cached).
	_, err2 := ProactiveRefreshIfNeeded(context.Background(), "claude", data, refresher, ProxyOptions{}, NopLogger(), now)
	if err2 == nil {
		t.Fatal("expected second refresh to retry (failure must not cache)")
	}
}

// tokenRefresherWithExpiry is a stub that returns a token + expiresIn, for the
// merge-patch test (stubRefresher returns no expiresIn).
type tokenRefresherWithExpiry struct {
	token    string
	expiresIn int
}

func (r *tokenRefresherWithExpiry) Refresh(_ context.Context, _ string, _ map[string]any, _ ProxyOptions, _ Logger) (*RefreshedCredentials, error) {
	return &RefreshedCredentials{AccessToken: r.token, ExpiresIn: r.expiresIn}, nil
}

type unrecoverableRefresher struct{}

func (r *unrecoverableRefresher) Refresh(_ context.Context, _ string, _ map[string]any, _ ProxyOptions, _ Logger) (*RefreshedCredentials, error) {
	return &RefreshedCredentials{Unrecoverable: true}, nil
}

type errorRefresher struct{ err error }

func (r *errorRefresher) Refresh(_ context.Context, _ string, _ map[string]any, _ ProxyOptions, _ Logger) (*RefreshedCredentials, error) {
	return nil, r.err
}

// countingWrap wraps a TokenRefresher and counts Refresh calls.
func countingWrap(inner TokenRefresher, calls *int32) TokenRefresher {
	return &countingRefresher{inner: inner, calls: calls}
}

type countingRefresher struct {
	inner TokenRefresher
	calls *int32
}

func (c *countingRefresher) Refresh(ctx context.Context, rt string, psd map[string]any, opts ProxyOptions, log Logger) (*RefreshedCredentials, error) {
	atomic.AddInt32(c.calls, 1)
	return c.inner.Refresh(ctx, rt, psd, opts, log)
}

// TestReactiveRefresh_ForcesRefresh runs the refresh even when the token is
// far from expiry (no shouldRefresh gate — the upstream already rejected it).
func TestReactiveRefresh_ForcesRefresh(t *testing.T) {
	ResetSharedRefreshDedup()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	// Token is 1h from expiry — proactive would skip, reactive must NOT.
	data := map[string]any{
		"refreshToken": "rt-react",
		"expiresAt":    now.Add(1 * time.Hour).Format(time.RFC3339Nano),
	}
	refresher := &tokenRefresherWithExpiry{token: "at-react", expiresIn: 3600}
	res, err := ReactiveRefresh(context.Background(), "claude", data, refresher, ProxyOptions{}, NopLogger(), now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Refreshed {
		t.Fatal("reactive refresh must run regardless of expiry")
	}
	if res.Patch["accessToken"] != "at-react" {
		t.Errorf("patch accessToken=%v want at-react", res.Patch["accessToken"])
	}
}

// TestReactiveRefresh_NoRefresher is a no-op when refresher is nil.
func TestReactiveRefresh_NoRefresher(t *testing.T) {
	res, err := ReactiveRefresh(context.Background(), "claude", map[string]any{"refreshToken": "rt"}, nil, ProxyOptions{}, NopLogger(), time.Now())
	if err != nil || res.Refreshed || res.Patch != nil {
		t.Fatalf("nil refresher must be a no-op, got %+v err=%v", res, err)
	}
}

// TestReactiveRefresh_Unrecoverable surfaces the marker (refresh token revoked).
func TestReactiveRefresh_Unrecoverable(t *testing.T) {
	ResetSharedRefreshDedup()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	data := map[string]any{"refreshToken": "rt-unrec", "expiresAt": now.Add(1 * time.Hour).Format(time.RFC3339Nano)}
	res, err := ReactiveRefresh(context.Background(), "claude", data, &unrecoverableRefresher{}, ProxyOptions{}, NopLogger(), now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Unrecoverable {
		t.Fatalf("expected Unrecoverable=true, got %+v", res)
	}
}

// TestReactiveRefresh_ErrorPropagates does not cache the failure.
func TestReactiveRefresh_ErrorPropagates(t *testing.T) {
	ResetSharedRefreshDedup()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	data := map[string]any{"refreshToken": "rt-err2", "expiresAt": now.Add(1 * time.Hour).Format(time.RFC3339Nano)}
	r := &errorRefresher{err: errors.New("500")}
	if _, err := ReactiveRefresh(context.Background(), "claude", data, r, ProxyOptions{}, NopLogger(), now); err == nil {
		t.Fatal("expected error propagation")
	}
	if _, err := ReactiveRefresh(context.Background(), "claude", data, r, ProxyOptions{}, NopLogger(), now); err == nil {
		t.Fatal("second reactive refresh must retry (failure not cached)")
	}
}