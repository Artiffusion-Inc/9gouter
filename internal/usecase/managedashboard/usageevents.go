package managedashboard

// usageevents.go is the live in-memory surface behind real-time usage analytics:
// the legacy JS module kept a process-global pendingRequests tracker
// ({byModel, byAccount}), a lastErrorProvider stamp, a recent-requests ring
// buffer, and a statsEmitter EventEmitter that the /api/usage/stream SSE handler
// subscribed to (open-sse utils/usageTracking.js + src/lib/db/repos/usageRepo.js).
// None of that existed in the Go rewrite — the SSE handler was a dead stub
// returning one `data:{}` frame, so the dashboard's real-time analytics never
// moved.
//
// This file ports that surface into Go as a single EventTracker: the proxychat
// usecase publishes Start/Stop/Error/Save events into it; the SSE handler
// registers a Subscriber and pushes lightweight frames on each emit. The
// tracker is safe for concurrent use (mutex-guarded maps + a subscriber list).
//
// It is deliberately in-memory only (no persistence): like the JS original it
// resets on restart. Lifetime usage totals live in the SQLite repo; this only
// holds the live "what is happening right now" view.

import (
	"context"
	"sync"
	"time"
)

// EventTracker is the process-live usage event surface. Construct one in the
// composition root, inject it into proxychat (as UsageEvents) and into the
// usage API handlers (as UsagePending). nil is always valid on the publish side
// — proxychat treats a nil tracker as a no-op so tests/legacy wiring still work.
type EventTracker struct {
	mu        sync.Mutex
	pending   pendingState
	lastErr   errorProvider
	subs      []Subscriber
	subID     int
	pendingT  map[string]*time.Timer // timerKey -> expiry timer
	recent    []recentEntry
	recentIdx map[string]bool
}

type pendingState struct {
	ByModel   map[string]int            // "model (provider)" -> count
	ByAccount map[string]map[string]int // connID -> {modelKey -> count}
}

type errorProvider struct {
	provider string
	ts       time.Time
}

type recentEntry struct {
	timestamp        time.Time
	model            string
	provider         string
	promptTokens     int
	completionTokens int
	status           string
}

// Subscriber receives a non-blocking notify callback when usage state changes.
// The SSE handler uses this to push a new frame to the open stream.
type Subscriber func()

const (
	pendingTimeout = 60 * time.Second
	recentRingCap   = 50
	recentDedupeCap = 20
	errProviderTTL  = 10 * time.Second
)

// NewEventTracker constructs a ready-to-use tracker.
func NewEventTracker() *EventTracker {
	return &EventTracker{
		pending: pendingState{
			ByModel:   map[string]int{},
			ByAccount: map[string]map[string]int{},
		},
		pendingT:  map[string]*time.Timer{},
		recentIdx: map[string]bool{},
	}
}

// PublishStart records an in-flight request: increments byModel and (if a
// connection is present) byAccount, arms a 60s safety timer that clears the
// counter if no matching Stop arrives (mirrors JS PENDING_TIMEOUT_MS), and
// notifies subscribers.
func (t *EventTracker) PublishStart(model, provider, connectionID string) {
	if t == nil {
		return
	}
	modelKey := modelKey(model, provider)
	timerKey := connectionID + "|" + modelKey

	t.mu.Lock()
	t.pending.ByModel[modelKey]++
	if connectionID != "" {
		if t.pending.ByAccount[connectionID] == nil {
			t.pending.ByAccount[connectionID] = map[string]int{}
		}
		t.pending.ByAccount[connectionID][modelKey]++
	}
	// (Re)arm the safety timer.
	if old := t.pendingT[timerKey]; old != nil {
		old.Stop()
	}
	t.pendingT[timerKey] = time.AfterFunc(pendingTimeout, func() {
		t.expirePending(connectionID, modelKey, timerKey)
	})
	t.mu.Unlock()

	t.notify()
}

// PublishStop decrements the in-flight counters for a request that completed
// (success or not) and clears its safety timer. On error with a provider set,
// stamps lastErrorProvider so the dashboard surfaces the failing provider.
func (t *EventTracker) PublishStop(model, provider, connectionID string, errored bool) {
	if t == nil {
		return
	}
	modelKey := modelKey(model, provider)
	timerKey := connectionID + "|" + modelKey

	t.mu.Lock()
	t.pending.ByModel[modelKey]--
	if t.pending.ByModel[modelKey] <= 0 {
		delete(t.pending.ByModel, modelKey)
	}
	if connectionID != "" {
		if m, ok := t.pending.ByAccount[connectionID]; ok {
			m[modelKey]--
			if m[modelKey] <= 0 {
				delete(m, modelKey)
			}
			if len(m) == 0 {
				delete(t.pending.ByAccount, connectionID)
			}
		}
	}
	if old := t.pendingT[timerKey]; old != nil {
		old.Stop()
		delete(t.pendingT, timerKey)
	}
	if errored && provider != "" {
		t.lastErr = errorProvider{provider: provider, ts: time.Now()}
	}
	t.mu.Unlock()

	t.notify()
}

// PublishSave records a completed request into the recent-requests ring (the
// source of the dashboard's "Recent Requests" panel and the recentRequests
// field on /api/usage/stats). Dedupe by model|provider|prompt|completion|minute
// up to recentDedupeCap; ring caps at recentRingCap.
func (t *EventTracker) PublishSave(model, provider, status string, prompt, completion int, ts time.Time) {
	if t == nil {
		return
	}
	if prompt == 0 && completion == 0 {
		return
	}
	if ts.IsZero() {
		ts = time.Now()
	}
	minute := ts.UTC().Format("2006-01-02T15:04")
	key := model + "|" + provider + "|" + itoa(prompt) + "|" + itoa(completion) + "|" + minute

	t.mu.Lock()
	if t.recentIdx[key] {
		t.mu.Unlock()
		return
	}
	t.recentIdx[key] = true
	t.recent = append(t.recent, recentEntry{
		timestamp:        ts,
		model:            model,
		provider:         provider,
		promptTokens:     prompt,
		completionTokens: completion,
		status:           status,
	})
	if len(t.recent) > recentRingCap {
		// Drop oldest; keep dedupe index from growing unbounded by pruning the
		// oldest key too.
		drop := t.recent[0]
		dk := drop.model + "|" + drop.provider + "|" + itoa(drop.promptTokens) + "|" + itoa(drop.completionTokens) + "|" + drop.timestamp.UTC().Format("2006-01-02T15:04")
		delete(t.recentIdx, dk)
		t.recent = t.recent[1:]
	}
	t.mu.Unlock()

	t.notify()
}

// ActiveRequests returns the active-request array the dashboard's
// ProviderTopology consumes: one entry per (connection,model) with count>0,
// resolving connection display names via connName. Mirrors JS getActiveRequests.
func (t *EventTracker) ActiveRequests(_ context.Context, connName func(id string) string) []map[string]any {
	if t == nil {
		return nil
	}
	out := []map[string]any{}
	t.mu.Lock()
	defer t.mu.Unlock()
	for connID, models := range t.pending.ByAccount {
		for mk, count := range models {
			if count <= 0 {
				continue
			}
			model, provider := splitModelKey(mk)
			name := connName(connID)
			out = append(out, map[string]any{
				"model":    model,
				"provider": provider,
				"account":  name,
				"count":    count,
			})
		}
	}
	return out
}

// RecentRequests returns up to `limit` recent completed requests, newest
// first. The dashboard's RecentRequests panel reads this (overlaid via SSE).
func (t *EventTracker) RecentRequests(limit int) []map[string]any {
	if t == nil {
		return nil
	}
	if limit <= 0 || limit > recentDedupeCap {
		limit = recentDedupeCap
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(t.recent)
	if n == 0 {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, n)
	// t.recent is append-ordered (oldest first); iterate newest-first.
	for i := n - 1; i >= 0 && len(out) < limit; i-- {
		e := t.recent[i]
		out = append(out, map[string]any{
			"timestamp":        e.timestamp.UTC().Format(time.RFC3339Nano),
			"model":            e.model,
			"provider":         e.provider,
			"promptTokens":     e.promptTokens,
			"completionTokens": e.completionTokens,
			"status":           nonEmpty(e.status, "ok"),
		})
	}
	return out
}

// Snapshot returns the pending state for the /api/usage/stats `pending` field.
func (t *EventTracker) Snapshot() map[string]any {
	if t == nil {
		return map[string]any{"byModel": map[string]int{}, "byAccount": map[string]map[string]int{}}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	byModel := make(map[string]int, len(t.pending.ByModel))
	for k, v := range t.pending.ByModel {
		byModel[k] = v
	}
	byAccount := make(map[string]map[string]int, len(t.pending.ByAccount))
	for k, m := range t.pending.ByAccount {
		cp := make(map[string]int, len(m))
		for kk, vv := range m {
			cp[kk] = vv
		}
		byAccount[k] = cp
	}
	return map[string]any{"byModel": byModel, "byAccount": byAccount}
}

// ErrorProvider returns the provider id of the most recent error within the
// 10s window, or "" if none. Mirrors JS lastErrorProvider logic.
func (t *EventTracker) ErrorProvider() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Since(t.lastErr.ts) < errProviderTTL {
		return t.lastErr.provider
	}
	return ""
}

// Subscribe registers a non-blocking notify callback. Returns an unsubscribe
// func. The SSE handler uses this to trigger a frame push. Subscribers are
// keyed by an integer id (captured in the returned closure) so unsubscribe is
// O(1) and does not rely on closure-pointer identity.
func (t *EventTracker) Subscribe(fn Subscriber) func() {
	if t == nil {
		return func() {}
	}
	t.mu.Lock()
	id := t.subID
	t.subID++
	t.subs = append(t.subs, fn)
	idx := len(t.subs) - 1
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if idx < len(t.subs) {
			t.subs[idx] = nil
		}
		_ = id
	}
}

// notify invokes every subscriber. Best-effort: a panicking subscriber is
// recovered so one bad SSE stream cannot kill the tracker.
func (t *EventTracker) notify() {
	t.mu.Lock()
	subs := make([]Subscriber, len(t.subs))
	copy(subs, t.subs)
	t.mu.Unlock()
	for _, s := range subs {
		if s == nil {
			continue
		}
		func() {
			defer func() { _ = recover() }()
			s()
		}()
	}
}

func (t *EventTracker) expirePending(connectionID, modelKey, timerKey string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending.ByModel[modelKey] > 0 {
		t.pending.ByModel[modelKey] = 0
		delete(t.pending.ByModel, modelKey)
	}
	if connectionID != "" {
		if m, ok := t.pending.ByAccount[connectionID]; ok {
			if m[modelKey] > 0 {
				delete(m, modelKey)
			}
			if len(m) == 0 {
				delete(t.pending.ByAccount, connectionID)
			}
		}
	}
	delete(t.pendingT, timerKey)
}

func modelKey(model, provider string) string {
	if provider == "" {
		return model
	}
	return model + " (" + provider + ")"
}

func splitModelKey(mk string) (model, provider string) {
	// mk == "model (provider)" when a provider was set, else mk == model.
	if len(mk) > 0 && mk[len(mk)-1] == ')' {
		open := -1
		for i := len(mk) - 2; i >= 0; i-- {
			if mk[i] == '(' && i > 0 && mk[i-1] == ' ' {
				open = i
				break
			}
		}
		if open > 0 {
			return mk[:open-1], mk[open+1 : len(mk)-1]
		}
	}
	return mk, "unknown"
}