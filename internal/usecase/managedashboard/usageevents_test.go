package managedashboard

import (
	"sync/atomic"
	"testing"
	"time"
)

// usageevents_test.go ports the regression coverage for the stats-event
// debounce half of decolua/9router #2509 (0d216689): schedule coalesces bursts
// of publish notifications into one subscriber callback per kind within the
// debounce window.

// subscribeCount registers a subscriber that increments a counter on each
// notify. Returns the counter pointer and an unsubscribe func.
func subscribeCount(t *testing.T, tr *EventTracker) (*int64, func()) {
	t.Helper()
	var n int64
	unsub := tr.Subscribe(func() {
		atomic.AddInt64(&n, 1)
	})
	return &n, unsub
}

func waitForCount(counter *int64, want int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(counter) >= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return atomic.LoadInt64(counter) >= want
}

func TestEventTracker_DebounceCoalescesUpdateBurst(t *testing.T) {
	tr := NewEventTracker()
	// Speed up the test: shrink the debounce windows.
	// We cannot lower the constants, so just tolerate the real 250ms window.
	counter, _ := subscribeCount(t, tr)
	ts := time.Now()
	// A burst of 5 saves within the debounce window must coalesce to ONE notify.
	for i := 0; i < 5; i++ {
		tr.PublishSave("gpt-4", "openai", "ok", 10, 20, ts)
	}
	// No notify should have fired synchronously (debounce armed).
	if got := atomic.LoadInt64(counter); got != 0 {
		t.Fatalf("notify fired synchronously during burst, count=%d, want 0", got)
	}
	if !waitForCount(counter, 1, statsUpdateDebounce+200*time.Millisecond) {
		t.Fatalf("coalesced notify did not fire, count=%d, want 1", atomic.LoadInt64(counter))
	}
	// Wait past the window to ensure no second notify.
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt64(counter); got != 1 {
		t.Errorf("notify count = %d, want 1 (burst coalesced)", got)
	}
}

func TestEventTracker_DebounceCoalescesPendingBurst(t *testing.T) {
	tr := NewEventTracker()
	counter, _ := subscribeCount(t, tr)
	// A burst of starts/stops must coalesce pending notifies to one.
	for i := 0; i < 4; i++ {
		tr.PublishStart("gpt-4", "openai", "c1")
		tr.PublishStop("gpt-4", "openai", "c1", false)
	}
	if got := atomic.LoadInt64(counter); got != 0 {
		t.Fatalf("notify fired synchronously, count=%d, want 0", got)
	}
	if !waitForCount(counter, 1, statsPendingDebounce+200*time.Millisecond) {
		t.Fatalf("coalesced pending notify did not fire, count=%d", atomic.LoadInt64(counter))
	}
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt64(counter); got != 1 {
		t.Errorf("pending notify count = %d, want 1", got)
	}
}

func TestEventTracker_DebounceIndependentKindsFireSeparately(t *testing.T) {
	tr := NewEventTracker()
	counter, _ := subscribeCount(t, tr)
	ts := time.Now()
	// Fire one update (250ms) and one pending (150ms). Both should fire, each
	// once, but the pending fires first (shorter window).
	tr.PublishSave("gpt-4", "openai", "ok", 10, 20, ts)
	tr.PublishStart("claude-3", "anthropic", "c2")
	// Both kinds arm their own timer; expect 2 notifies total.
	if !waitForCount(counter, 2, statsUpdateDebounce+300*time.Millisecond) {
		t.Fatalf("both kinds did not fire, count=%d, want 2", atomic.LoadInt64(counter))
	}
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt64(counter); got != 2 {
		t.Errorf("notify count = %d, want 2 (one per kind)", got)
	}
}

func TestEventTracker_NotifyNowFlushesPendingDebounce(t *testing.T) {
	tr := NewEventTracker()
	counter, _ := subscribeCount(t, tr)
	ts := time.Now()
	tr.PublishSave("gpt-4", "openai", "ok", 10, 20, ts)
	tr.PublishStart("claude-3", "anthropic", "c2")
	// Flush immediately without waiting for the debounce timers.
	tr.NotifyNow()
	if got := atomic.LoadInt64(counter); got != 2 {
		t.Errorf("NotifyNow count = %d, want 2 (both pending timers flushed)", got)
	}
	// A subsequent debounce timer should NOT double-fire.
	time.Sleep(statsUpdateDebounce + 100*time.Millisecond)
	if got := atomic.LoadInt64(counter); got != 2 {
		t.Errorf("post-flush count = %d, want 2 (no double fire)", got)
	}
}

func TestEventTracker_DebounceDisabledFiresSynchronously(t *testing.T) {
	tr := NewEventTracker()
	tr.DebounceEnabled = false
	counter, _ := subscribeCount(t, tr)
	ts := time.Now()
	tr.PublishSave("gpt-4", "openai", "ok", 10, 20, ts)
	// With debounce disabled, notify fires inline.
	if got := atomic.LoadInt64(counter); got != 1 {
		t.Fatalf("synchronous notify count = %d, want 1", got)
	}
	tr.PublishStart("gpt-4", "openai", "c1")
	if got := atomic.LoadInt64(counter); got != 2 {
		t.Errorf("synchronous pending notify count = %d, want 2", got)
	}
}

func TestEventTracker_StateUpdatesSynchronouslyDespiteDebounce(t *testing.T) {
	// The dedup/debounce work must NOT delay state: Snapshot/ActiveRequests/
	// RecentRequests reflect publishes immediately (only the subscriber notify
	// is debounced). This is what lets the existing #83 tests stay valid.
	tr := NewEventTracker()
	tr.PublishStart("gpt-4", "openai", "conn-aaa")
	snap := tr.Snapshot()
	byModel, _ := snap["byModel"].(map[string]int)
	if byModel["gpt-4 (openai)"] != 1 {
		t.Errorf("state not updated synchronously: byModel=%v", byModel)
	}
	ts := time.Now()
	tr.PublishSave("gpt-4", "openai", "ok", 10, 20, ts)
	if rr := tr.RecentRequests(5); len(rr) != 1 {
		t.Errorf("recent ring not updated synchronously: %v", rr)
	}
}
