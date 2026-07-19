package accountfallback

import (
	"errors"
	"testing"
	"time"
)

func TestCheckFallbackError_TextRuleLong(t *testing.T) {
	d := CheckFallbackError(500, "no credentials configured", 0, nil)
	if !d.ShouldFallback {
		t.Fatal("expected fallback")
	}
	if d.CooldownMs != cooldownLong {
		t.Fatalf("cooldown = %d, want %d", d.CooldownMs, cooldownLong)
	}
	if _, changed := d.NewBackoffLevelSet(); changed {
		t.Fatal("text rule should not change backoff level")
	}
}

func TestCheckFallbackError_BackoffRule(t *testing.T) {
	d := CheckFallbackError(429, "rate limit exceeded", 0, nil)
	if !d.ShouldFallback {
		t.Fatal("expected fallback for 429")
	}
	newLevel, changed := d.NewBackoffLevelSet()
	if !changed {
		t.Fatal("expected backoff level changed")
	}
	if newLevel != 1 {
		t.Fatalf("new level = %d, want 1", newLevel)
	}
	if d.CooldownMs != GetQuotaCooldown(1) {
		t.Fatalf("cooldown = %d, want %d", d.CooldownMs, GetQuotaCooldown(1))
	}
}

func TestCheckFallbackError_BackoffEscalation(t *testing.T) {
	// Level 3 → next is 4, cooldown doubles.
	d := CheckFallbackError(429, "too many requests", 3, nil)
	newLevel, _ := d.NewBackoffLevelSet()
	if newLevel != 4 {
		t.Fatalf("new level = %d, want 4", newLevel)
	}
	if d.CooldownMs != GetQuotaCooldown(4) {
		t.Fatalf("cooldown = %d, want %d", d.CooldownMs, GetQuotaCooldown(4))
	}
}

func TestCheckFallbackError_BackoffCap(t *testing.T) {
	d := CheckFallbackError(429, "overloaded", BackoffConfig.MaxLevel+5, nil)
	newLevel, _ := d.NewBackoffLevelSet()
	if newLevel != BackoffConfig.MaxLevel {
		t.Fatalf("new level = %d, want cap %d", newLevel, BackoffConfig.MaxLevel)
	}
}

func TestCheckFallbackError_DefaultTransient(t *testing.T) {
	// 5xx not in rules, text not matching → transient default.
	d := CheckFallbackError(500, "internal error", 0, nil)
	if !d.ShouldFallback {
		t.Fatal("unmatched error should still fallback (transient)")
	}
	if d.CooldownMs != TransientCooldown {
		t.Fatalf("cooldown = %d, want %d", d.CooldownMs, TransientCooldown)
	}
}

func TestCheckFallbackError_StatusRule(t *testing.T) {
	d := CheckFallbackError(401, "Unauthorized", 0, nil)
	if !d.ShouldFallback {
		t.Fatal("401 should fallback")
	}
	if d.CooldownMs != cooldownLong {
		t.Fatalf("cooldown = %d, want %d", d.CooldownMs, cooldownLong)
	}
}

// TestCheckFallbackError_ProxyOverrideDoesNotLock is the #2703 Fix 3
// acceptance criterion: a proxy outage override must NOT lock the account
// (ShouldFallback=false), even though an unmatched error defaults to
// transient fallback.
func TestCheckFallbackError_ProxyOverrideDoesNotLock(t *testing.T) {
	override := &FallbackDecision{ShouldFallback: false, CooldownMs: 0}
	d := CheckFallbackError(502, "fetch failed: dial timeout", 0, override)
	if d.ShouldFallback {
		t.Fatal("proxy outage override must not lock the account")
	}
	if d.CooldownMs != 0 {
		t.Fatalf("cooldown = %d, want 0", d.CooldownMs)
	}
}

func TestOverrideForSource(t *testing.T) {
	if ov := OverrideForSource(FailureSourceProxy); ov == nil || ov.ShouldFallback {
		t.Fatal("proxy source should override to no-fallback")
	}
	if ov := OverrideForSource(FailureSourceRelay); ov == nil || ov.ShouldFallback {
		t.Fatal("relay source should override to no-fallback")
	}
	if ov := OverrideForSource(FailureSourceUpstream); ov != nil {
		t.Fatal("upstream source should NOT override (rule table decides)")
	}
	if ov := OverrideForSource(FailureSourceUnknown); ov != nil {
		t.Fatal("unknown source should NOT override")
	}
}

func TestAsProxyRouteError(t *testing.T) {
	inner := errors.New("dial timeout")
	pe := &ProxyRouteError{Source: FailureSourceProxy, Err: inner}
	got, ok := AsProxyRouteError(pe)
	if !ok {
		t.Fatal("expected to unwrap ProxyRouteError")
	}
	if got.Source != FailureSourceProxy {
		t.Fatalf("source = %q, want proxy", got.Source)
	}
	// Wrapped.
	wrapped := errors.Join(pe)
	got2, ok2 := AsProxyRouteError(wrapped)
	if !ok2 {
		t.Fatal("expected to unwrap through errors.Join")
	}
	if got2 != pe {
		t.Fatal("unwrapped to wrong instance")
	}
	// Plain error.
	if _, ok := AsProxyRouteError(errors.New("plain")); ok {
		t.Fatal("plain error should not unwrap to ProxyRouteError")
	}
}

func TestModelLockKey(t *testing.T) {
	if k := ModelLockKey("gpt-4"); k != "modelLock_gpt-4" {
		t.Fatalf("key = %q", k)
	}
	if k := ModelLockKey(""); k != ModelLockAll {
		t.Fatalf("empty model key = %q, want %q", k, ModelLockAll)
	}
}

func TestModelLockRoundTrip(t *testing.T) {
	data := ModelLockUpdate("gpt-4", 5000)
	if _, ok := data["modelLock_gpt-4"]; !ok {
		t.Fatalf("missing modelLock_gpt-4 key: %v", data)
	}
	if !IsModelLockActive(data, "gpt-4") {
		t.Fatal("fresh lock should be active")
	}
	// Empty model resolves to the account-level __all key, which is not set
	// here — only modelLock_gpt-4 is. So a per-model lock does NOT lock the
	// account-wide "all models" view.
	if IsModelLockActive(data, "") {
		t.Fatal("per-model lock should not report account-level __all active")
	}
}

func TestIsModelLockActive_Expired(t *testing.T) {
	data := map[string]any{
		"modelLock_gpt-4": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
	}
	if IsModelLockActive(data, "gpt-4") {
		t.Fatal("expired lock should be inactive")
	}
}

func TestIsModelLockActive_AccountLevelAll(t *testing.T) {
	data := map[string]any{
		ModelLockAll: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
	}
	if !IsModelLockActive(data, "gpt-4") {
		t.Fatal("account-level lock should lock every model")
	}
}

func TestIsModelLockActive_Nil(t *testing.T) {
	if IsModelLockActive(nil, "gpt-4") {
		t.Fatal("nil data should not be locked")
	}
}

func TestClearAccountErrorPatch_SucceededModel(t *testing.T) {
	data := map[string]any{
		"modelLock_gpt-4": time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
		"modelLock_gpt-5": time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
		"testStatus":      "unavailable",
		"lastError":       "rate limit",
	}
	patch := ClearAccountErrorPatch(data, "gpt-4")
	if patch == nil {
		t.Fatal("expected a clear patch")
	}
	// gpt-4 cleared, gpt-5 retained (still active), so error state NOT reset.
	if _, cleared := patch["modelLock_gpt-4"]; !cleared || patch["modelLock_gpt-4"] != nil {
		t.Fatal("succeeded model lock should be cleared (set to nil)")
	}
	if _, retained := patch["modelLock_gpt-5"]; retained {
		t.Fatal("active gpt-5 lock should not be in the patch")
	}
	if ts, reset := patch["testStatus"]; reset {
		t.Fatalf("testStatus should not reset while gpt-5 active, got %v", ts)
	}
}

func TestClearAccountErrorPatch_AllClearResetsState(t *testing.T) {
	data := map[string]any{
		"modelLock_gpt-4": time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
		"testStatus":      "unavailable",
		"lastError":       "rate limit",
		"backoffLevel":    2,
	}
	patch := ClearAccountErrorPatch(data, "gpt-4")
	if patch == nil {
		t.Fatal("expected a clear patch")
	}
	if patch["testStatus"] != "active" {
		t.Fatalf("testStatus = %v, want active", patch["testStatus"])
	}
	if patch["lastError"] != nil {
		t.Fatal("lastError should be cleared")
	}
	if patch["backoffLevel"] != 0 {
		t.Fatalf("backoffLevel = %v, want 0", patch["backoffLevel"])
	}
}

func TestClearAccountErrorPatch_ExpiredLocksCleared(t *testing.T) {
	data := map[string]any{
		"modelLock_gpt-4": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), // expired
		"modelLock_gpt-5": time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),  // active
		"testStatus":      "unavailable",
	}
	patch := ClearAccountErrorPatch(data, "claude-3") // different model
	if patch == nil {
		t.Fatal("expired lock should be cleared even for a different model")
	}
	if _, cleared := patch["modelLock_gpt-4"]; !cleared {
		t.Fatal("expired gpt-4 lock should be in patch")
	}
	if _, retained := patch["modelLock_gpt-5"]; retained {
		t.Fatal("active gpt-5 should not be cleared")
	}
	if _, reset := patch["testStatus"]; reset {
		t.Fatal("testStatus should not reset while gpt-5 active")
	}
}

func TestClearAccountErrorPatch_NothingToDo(t *testing.T) {
	data := map[string]any{
		"apiKey": "sk-test",
	}
	if patch := ClearAccountErrorPatch(data, "gpt-4"); patch != nil {
		t.Fatalf("clean connection should produce nil patch, got %v", patch)
	}
}

func TestMarkUnavailablePatch(t *testing.T) {
	patch := MarkUnavailablePatch("gpt-4", 429, "rate limit exceeded", 4000, 1, true)
	if _, ok := patch["modelLock_gpt-4"]; !ok {
		t.Fatal("missing modelLock_gpt-4")
	}
	if patch["testStatus"] != "unavailable" {
		t.Fatalf("testStatus = %v, want unavailable", patch["testStatus"])
	}
	if patch["errorCode"] != 429 {
		t.Fatalf("errorCode = %v, want 429", patch["errorCode"])
	}
	if patch["backoffLevel"] != 1 {
		t.Fatalf("backoffLevel = %v, want 1", patch["backoffLevel"])
	}
	if patch["lastError"] != "rate limit exceeded" {
		t.Fatalf("lastError = %v", patch["lastError"])
	}
	// Truncation.
	long := string(make([]byte, 200))
	for i := range long {
		long = long[:i] + "x" + long[i+1:]
	}
	patch2 := MarkUnavailablePatch("m", 500, long, 1000, 0, false)
	if len(patch2["lastError"].(string)) > 100 {
		t.Fatalf("lastError not truncated: %d", len(patch2["lastError"].(string)))
	}
	if _, hasBackoff := patch2["backoffLevel"]; hasBackoff {
		t.Fatal("backoffLevel should not be written when backoffChanged=false")
	}
}

func TestGetQuotaCooldown(t *testing.T) {
	if cd := GetQuotaCooldown(1); cd != BackoffConfig.Base {
		t.Fatalf("level 1 cooldown = %d, want %d", cd, BackoffConfig.Base)
	}
	if cd := GetQuotaCooldown(2); cd != BackoffConfig.Base*2 {
		t.Fatalf("level 2 cooldown = %d, want %d", cd, BackoffConfig.Base*2)
	}
	if cd := GetQuotaCooldown(100); cd != BackoffConfig.Max {
		t.Fatalf("capped cooldown = %d, want %d", cd, BackoffConfig.Max)
	}
}