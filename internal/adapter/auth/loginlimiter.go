package auth

import (
	"sync"
	"time"
)

// Default lockout constants ported from loginLimiter.js.
const (
	maxFailsBeforeLock = 5
	failWindow         = time.Hour
)

// lockSteps are the progressive lock durations after each lock escalation.
var lockSteps = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	30 * time.Minute,
}

// lockEntry tracks the state for one IP.
type lockEntry struct {
	fails      int
	lockUntil  time.Time
	lockLevel  int
	lastFailAt time.Time
}

// LoginLimiter provides in-memory progressive lockout for dashboard login.
// It is safe for concurrent use. A successful login resets the entry for the IP.
type LoginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*lockEntry
	now      func() time.Time
}

// NewLoginLimiter returns a fresh login limiter. Use the same constants as
// loginLimiter.js: 5 fails before lock, escalating lock durations, 1h idle reset.
func NewLoginLimiter() *LoginLimiter {
	return &LoginLimiter{
		attempts: make(map[string]*lockEntry),
		now:      time.Now,
	}
}

// Allow reports whether the IP is currently permitted to attempt login.
func (l *LoginLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.getEntryLocked(ip)
	if e == nil {
		return true
	}
	return l.now().After(e.lockUntil) || l.now().Equal(e.lockUntil)
}

// RecordFail increments the failure count and returns the remaining attempts
// before lockout. If the threshold is crossed the IP is locked for a step that
// escalates with each repeated lock.
func (l *LoginLimiter) RecordFail(ip string) (remainingBeforeLock int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	e := l.getEntryLocked(ip)
	if e == nil {
		e = &lockEntry{}
		l.attempts[ip] = e
	}
	e.fails++
	e.lastFailAt = l.now()

	if e.fails >= maxFailsBeforeLock {
		step := lockSteps[min(e.lockLevel, len(lockSteps)-1)]
		e.lockUntil = l.now().Add(step)
		e.lockLevel++
		e.fails = 0
	}

	return max(0, maxFailsBeforeLock-e.fails)
}

// RecordSuccess clears any lock/failure state for the IP.
func (l *LoginLimiter) RecordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, ip)
}

// LockStatus describes whether an IP is currently locked and for how long.
type LockStatus struct {
	Locked    bool
	RetryAfter time.Duration
}

// CheckLock returns the lock status for an IP without mutating state.
func (l *LoginLimiter) CheckLock(ip string) LockStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.getEntryLocked(ip)
	if e == nil {
		return LockStatus{}
	}
	remaining := e.lockUntil.Sub(l.now())
	if remaining <= 0 {
		return LockStatus{}
	}
	return LockStatus{Locked: true, RetryAfter: remaining}
}

// getEntryLocked returns the entry if it is still relevant, or nil after an
// idle reset. Caller must hold mu.
func (l *LoginLimiter) getEntryLocked(ip string) *lockEntry {
	e, ok := l.attempts[ip]
	if !ok {
		return nil
	}
	// Auto reset if the window expired and the lock is not active.
	if !e.lastFailAt.IsZero() && l.now().Sub(e.lastFailAt) > failWindow {
		if e.lockUntil.IsZero() || !l.now().Before(e.lockUntil) {
			delete(l.attempts, ip)
			return nil
		}
	}
	return e
}
