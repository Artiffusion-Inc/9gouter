package auth

import (
	"testing"
	"time"
)

func TestLoginLimiter_AllowAfterFailures(t *testing.T) {
	l := NewLoginLimiter()
	ip := "192.0.2.1"

	for i := 0; i < maxFailsBeforeLock-1; i++ {
		if !l.Allow(ip) {
			t.Fatalf("expected IP to be allowed before lock, fail %d", i)
		}
		rem := l.RecordFail(ip)
		if rem != maxFailsBeforeLock-i-1 {
			t.Errorf("remaining before lock = %d, want %d", rem, maxFailsBeforeLock-i-1)
		}
	}

	// Fifth failure triggers the lock.
	l.RecordFail(ip)
	if l.Allow(ip) {
		t.Fatal("expected IP to be locked after 5 failures")
	}

	status := l.CheckLock(ip)
	if !status.Locked {
		t.Fatal("expected CheckLock to report locked")
	}
	if status.RetryAfter <= 0 || status.RetryAfter > lockSteps[0] {
		t.Errorf("unexpected retryAfter: %v", status.RetryAfter)
	}
}

func TestLoginLimiter_SuccessResets(t *testing.T) {
	l := NewLoginLimiter()
	ip := "192.0.2.2"

	for i := 0; i < maxFailsBeforeLock; i++ {
		l.RecordFail(ip)
	}
	if l.Allow(ip) {
		t.Fatal("expected IP to be locked")
	}

	l.RecordSuccess(ip)
	if !l.Allow(ip) {
		t.Fatal("expected success to reset lock")
	}
	if status := l.CheckLock(ip); status.Locked {
		t.Fatal("expected no lock after success")
	}
}

func TestLoginLimiter_Escalation(t *testing.T) {
	l := NewLoginLimiter()
	l.now = func() time.Time { return time.Unix(0, 0) }
	ip := "192.0.2.3"

	// Trigger the first lock.
	for i := 0; i < maxFailsBeforeLock; i++ {
		l.RecordFail(ip)
	}
	firstLock := l.CheckLock(ip).RetryAfter
	if firstLock != lockSteps[0] {
		t.Fatalf("first lock = %v, want %v", firstLock, lockSteps[0])
	}

	// Simulate lock expiry and re-trigger; level escalates.
	l.now = func() time.Time { return time.Unix(0, 0).Add(lockSteps[0] + time.Second) }
	for i := 0; i < maxFailsBeforeLock; i++ {
		l.RecordFail(ip)
	}
	secondLock := l.CheckLock(ip).RetryAfter
	if secondLock != lockSteps[1] {
		t.Fatalf("second lock = %v, want %v", secondLock, lockSteps[1])
	}
}

func TestLoginLimiter_WindowReset(t *testing.T) {
	start := time.Unix(0, 0)
	l := NewLoginLimiter()
	l.now = func() time.Time { return start }
	ip := "192.0.2.4"

	l.RecordFail(ip)
	if l.CheckLock(ip).Locked {
		t.Fatal("single fail should not lock")
	}

	// Move beyond the 1h idle window; the entry should reset.
	l.now = func() time.Time { return start.Add(failWindow + time.Second) }
	if !l.Allow(ip) {
		t.Fatal("expected auto reset after idle window")
	}
	if status := l.CheckLock(ip); status.Locked {
		t.Fatalf("expected no lock after idle window, got %v", status)
	}
}

func TestLoginLimiter_DistinctIPs(t *testing.T) {
	l := NewLoginLimiter()
	for i := 0; i < maxFailsBeforeLock; i++ {
		l.RecordFail("192.0.2.5")
	}
	if !l.Allow("192.0.2.6") {
		t.Fatal("expected different IP to be unaffected")
	}
}
