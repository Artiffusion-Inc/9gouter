// Package accountfallback ports the JS account-selection error classification
// and per-model lock logic from open-sse/config/errorConfig.js +
// open-sse/services/accountFallback.js + src/sse/services/auth.js
// (markAccountUnavailable / clearAccountError). It is the pure-logic core of
// decolua/9gouter #2703 Fix 3 (structured failure types): a failure is
// classified into a fallback decision plus a cooldown, and proxy/relay
// outages are distinguished from provider/account failures so a proxy
// outage does not lock a healthy account.
package accountfallback

import (
	"errors"
	"strings"
	"time"
)

// Cooldowns (ms) ported from open-sse/config/errorConfig.js.
const (
	cooldownLong  = 2 * 60 * 1000
	cooldownShort = 5 * 1000

	// TransientCooldown is the default cooldown for any unmatched error.
	TransientCooldown = 30 * 1000
	// MaxRateLimitCooldown is the hard cap on a provider-reported rate-limit
	// cooldown (e.g. codex resets_at can be 5-6h).
	MaxRateLimitCooldown = 30 * 60 * 1000
)

// BackoffConfig ports the JS BACKOFF_CONFIG for 429 exponential backoff.
var BackoffConfig = struct {
	Base     int
	Max      int
	MaxLevel int
}{
	Base:     2000,
	Max:      5 * 60 * 1000,
	MaxLevel: 15,
}

// ErrorRule is one entry in the ERROR_RULES table. Exactly one of Text or
// Status is non-zero/non-empty. Backoff=true means use exponential backoff
// (rate limit); otherwise CooldownMs is a fixed cooldown.
type ErrorRule struct {
	Text      string
	Status    int
	CooldownMs int
	Backoff   bool
}

// ErrorRules is the top-to-bottom classification table. Text rules are
// checked first (in order), then status rules. The first match wins. This
// mirrors ERROR_RULES in errorConfig.js verbatim, including order.
var ErrorRules = []ErrorRule{
	{Text: "no credentials", CooldownMs: cooldownLong},
	{Text: "request not allowed", CooldownMs: cooldownShort},
	{Text: "improperly formed request", CooldownMs: cooldownLong},
	{Text: "rate limit", Backoff: true},
	{Text: "too many requests", Backoff: true},
	{Text: "quota exceeded", Backoff: true},
	{Text: "capacity", Backoff: true},
	{Text: "overloaded", Backoff: true},
	{Status: 401, CooldownMs: cooldownLong},
	{Status: 402, CooldownMs: cooldownLong},
	{Status: 403, CooldownMs: cooldownLong},
	{Status: 404, CooldownMs: cooldownLong},
	{Status: 429, Backoff: true},
}

// FallbackDecision is the result of classifying one failure. ShouldFallback
// is true when the caller should exclude this account and try the next; when
// false the request fails hard with the original error (e.g. a proxy route
// outage — the account itself is healthy). CooldownMs is the lock duration
// when ShouldFallback is true. NewBackoffLevel is set on backoff rules.
type FallbackDecision struct {
	ShouldFallback   bool
	CooldownMs       int
	NewBackoffLevel  int
	backoffChanged   bool
}

// GetQuotaCooldown returns the exponential-backoff cooldown for a backoff
// level (429 / rate-limit). Level 1 → base, doubling per level, capped at Max.
func GetQuotaCooldown(backoffLevel int) int {
	level := backoffLevel - 1
	if level < 0 {
		level = 0
	}
	if level > 30 {
		level = 30 // guard against 1<<n overflow for absurd levels
	}
	cd := BackoffConfig.Base * (1 << uint(level))
	if cd <= 0 || cd > BackoffConfig.Max {
		cd = BackoffConfig.Max
	}
	return cd
}

// CheckFallbackError classifies an upstream failure by walking ErrorRules
// top-to-bottom (text first, then status). It ports checkFallbackError.
//
// The override param lets the caller inject a structured failure source
// (#2703 Fix 3): when override is non-nil it wins outright. The transport
// layer builds an override from a typed proxy.FailureSource so a proxy/
// relay outage returns ShouldFallback=false (do NOT lock the account) — the
// original JS bug was that every unmatched error, including proxy failures,
// defaulted to ShouldFallback=true and locked healthy accounts.
func CheckFallbackError(status int, errorText string, backoffLevel int, override *FallbackDecision) FallbackDecision {
	if override != nil {
		return *override
	}
	lower := strings.ToLower(errorText)
	for _, rule := range ErrorRules {
		if rule.Text != "" {
			if lower != "" && strings.Contains(lower, rule.Text) {
				return applyRule(rule, backoffLevel)
			}
			continue
		}
		if rule.Status != 0 && rule.Status == status {
			return applyRule(rule, backoffLevel)
		}
	}
	return FallbackDecision{ShouldFallback: true, CooldownMs: TransientCooldown}
}

func applyRule(rule ErrorRule, backoffLevel int) FallbackDecision {
	if rule.Backoff {
		newLevel := backoffLevel + 1
		if newLevel > BackoffConfig.MaxLevel {
			newLevel = BackoffConfig.MaxLevel
		}
		return FallbackDecision{ShouldFallback: true, CooldownMs: GetQuotaCooldown(newLevel), NewBackoffLevel: newLevel, backoffChanged: true}
	}
	return FallbackDecision{ShouldFallback: true, CooldownMs: rule.CooldownMs}
}

// NewBackoffLevel reports the updated backoff level (unchanged when the rule
// was not a backoff rule).
func (d FallbackDecision) NewBackoffLevelSet() (int, bool) {
	return d.NewBackoffLevel, d.backoffChanged
}

// ModelLockPrefix is the flat-field prefix for per-model locks on a
// connection record. modelLock_gpt-4 = ISO timestamp until which that model
// is locked on that account.
const ModelLockPrefix = "modelLock_"

// ModelLockAll is the account-level lock key (no model known).
const ModelLockAll = ModelLockPrefix + "__all"

// ModelLockKey returns the flat-field key for a model lock.
func ModelLockKey(model string) string {
	if model == "" {
		return ModelLockAll
	}
	return ModelLockPrefix + model
}

// IsModelLockActive reports whether the given model's lock (or the
// account-level lock) is still active on a connection's stored data. A lock
// is active when its ISO timestamp is in the future.
func IsModelLockActive(data map[string]any, model string) bool {
	if data == nil {
		return false
	}
	keys := []string{ModelLockKey(model), ModelLockAll}
	for _, k := range keys {
		v, ok := data[k]
		if !ok {
			continue
		}
		s, _ := v.(string)
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil && t.After(time.Now()) {
			return true
		}
	}
	return false
}

// ModelLockUpdate returns the flat-field patch that sets a model lock to
// now+cooldown. The caller merges this into the connection data blob.
func ModelLockUpdate(model string, cooldownMs int) map[string]any {
	return map[string]any{
		ModelLockKey(model): time.Now().Add(time.Duration(cooldownMs) * time.Millisecond).UTC().Format(time.RFC3339Nano),
	}
}

// ClearAccountErrorPatch mirrors the JS clearAccountError: it clears the
// succeeded model's lock plus all expired modelLock_* keys, and — only when
// no active locks remain — resets the error state (testStatus=active,
// lastError/lastErrorAt/backoffLevel cleared). Returns nil when there is
// nothing to write.
func ClearAccountErrorPatch(data map[string]any, model string) map[string]any {
	if data == nil {
		return nil
	}
	now := time.Now()
	var lockKeys []string
	for k := range data {
		if strings.HasPrefix(k, ModelLockPrefix) {
			lockKeys = append(lockKeys, k)
		}
	}
	testStatus, _ := data["testStatus"].(string)
	lastErrorStr, _ := data["lastError"].(string)
	hasErr := lastErrorStr != ""
	if testStatus == "" && !hasErr && len(lockKeys) == 0 {
		return nil
	}

	keysToClear := map[string]struct{}{}
	for _, k := range lockKeys {
		if model != "" && (k == ModelLockKey(model) || k == ModelLockAll) {
			keysToClear[k] = struct{}{}
			continue
		}
		s, _ := data[k].(string)
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil && !t.After(now) {
			keysToClear[k] = struct{}{}
		}
	}

	// Any active lock remaining after the clear?
	remaining := false
	for _, k := range lockKeys {
		if _, clearing := keysToClear[k]; clearing {
			continue
		}
		s, _ := data[k].(string)
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil && t.After(now) {
			remaining = true
			break
		}
	}

	if len(keysToClear) == 0 && testStatus != "unavailable" && !hasErr {
		return nil
	}

	patch := map[string]any{}
	for k := range keysToClear {
		patch[k] = nil
	}
	if !remaining {
		patch["testStatus"] = "active"
		patch["lastError"] = nil
		patch["lastErrorAt"] = nil
		patch["backoffLevel"] = 0
	}
	return patch
}

// MarkUnavailablePatch mirrors the JS markAccountUnavailable write: the model
// lock + testStatus=unavailable + lastError + errorCode + lastErrorAt +
// backoffLevel. Returns nil when shouldFallback is false (no lock written).
func MarkUnavailablePatch(model string, status int, errorText string, cooldownMs int, backoffLevel int, backoffChanged bool) map[string]any {
	patch := ModelLockUpdate(model, cooldownMs)
	patch["testStatus"] = "unavailable"
	reason := errorText
	if len(reason) > 100 {
		reason = reason[:100]
	}
	patch["lastError"] = reason
	patch["errorCode"] = status
	patch["lastErrorAt"] = time.Now().UTC().Format(time.RFC3339Nano)
	if backoffChanged {
		patch["backoffLevel"] = backoffLevel
	}
	return patch
}

// --- Typed failure sources (#2703 Fix 3) ---

// FailureSource categorises where a failure originated, mirroring
// proxy.FailureSource so account-selection can distinguish a proxy/relay
// outage (do NOT lock the account) from a provider/account failure (lock).
type FailureSource string

const (
	FailureSourceUnknown  FailureSource = "unknown"
	FailureSourceProxy     FailureSource = "proxy"
	FailureSourceRelay     FailureSource = "relay"
	FailureSourceUpstream  FailureSource = "upstream"
)

// ProxyRouteError is the typed failure a proxy/relay outage surfaces. It is
// the #2703 Fix 3 acceptance criterion: a proxy outage must not
// automatically lock a healthy account. The fallback loop converts it into a
// FallbackDecision{ShouldFallback:false} so the request fails hard against
// the original account without marking it unavailable.
type ProxyRouteError struct {
	Source FailureSource
	Err    error
}

func (e *ProxyRouteError) Error() string {
	if e.Err != nil {
		return "proxy route error: " + e.Err.Error()
	}
	return "proxy route error"
}

func (e *ProxyRouteError) Unwrap() error { return e.Err }

// OverrideForSource maps a typed FailureSource to a FallbackDecision override.
// Proxy/relay outages return ShouldFallback=false (do not lock the account);
// upstream/unknown return nil so CheckFallbackError falls back to the rule
// table.
func OverrideForSource(src FailureSource) *FallbackDecision {
	switch src {
	case FailureSourceProxy, FailureSourceRelay:
		return &FallbackDecision{ShouldFallback: false, CooldownMs: 0}
	default:
		return nil
	}
}

// AsProxyRouteError reports whether err wraps a *ProxyRouteError.
func AsProxyRouteError(err error) (*ProxyRouteError, bool) {
	var pe *ProxyRouteError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}