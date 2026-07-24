package antigravityexec

// retry.go ports the Antigravity transient-retry framework from
// decolua/9router 639f1204 (open-sse/executors/antigravity.js): parseRetryHeaders,
// parseRetryFromErrorMessage, extractErrorMessage, isTransientAntigravityError,
// and the computeRetryDelay hook. The hook is surfaced to BaseExecutor via
// Config.ComputeRetryDelay (base.ComputeRetryDelayFunc) — the embedded-method
// override does NOT dispatch from the promoted BaseExecutor.Execute (same
// embedded-method limitation documented in #142), so the hook is wired as a
// config field, not a method override.
//
// Semantics mirror the JS hook exactly:
//   - Retry-After (header) → seconds/HTTP-date → x-ratelimit-reset-after → x-ratelimit-reset.
//   - "reset after NhNmNs" in the error body → ms.
//   - If a parsed retry hint exceeds maxRetryAfterMs the hook VETOES the retry
//     (returns retry=false) so the request falls through to the next URL, the
//     same as JS `if (retryMs) return retryMs <= MAX_RETRY_AFTER_MS ? retryMs : false`.
//   - Otherwise retry transient Antigravity failures (429 + 500/502/503/504 +
//     error patterns) with exponential backoff capped at maxRetryAfterMs (429)
//     or antigravityTransientRetryMaxMs (transient).
//   - Non-transient / non-retryable (e.g. 400) → veto (retry=false).

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// maxRetryAfterMs mirrors MAX_RETRY_AFTER_MS: the upper bound for an
	// explicit Retry-After hint. A hint above this vetoes the retry.
	maxRetryAfterMs = 10000
	// antigravityTransientRetryMaxMs mirrors ANTIGRAVITY_TRANSIENT_RETRY_MAX_MS:
	// the exponential-backoff cap for transient (non-429) Antigravity failures.
	antigravityTransientRetryMaxMs = 15000
)

// antigravityTransientErrorPatterns mirrors ANTIGRAVITY_TRANSIENT_ERROR_PATTERNS.
// Order preserved from upstream so matching is stable.
var antigravityTransientErrorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)high\s+traffic`),
	regexp.MustCompile(`(?i)agent\s+(execution\s+)?terminated\s+due\s+to\s+error`),
	regexp.MustCompile(`(?i)capacity`),
	regexp.MustCompile(`(?i)temporarily\s+unavailable`),
	regexp.MustCompile(`(?i)timeout`),
	regexp.MustCompile(`(?i)stream\s+(ended|closed|terminated|interrupted)`),
	regexp.MustCompile(`(?i)empty\s+response`),
}

// antigravityTransientStatuses mirrors ANTIGRAVITY_TRANSIENT_STATUSES.
var antigravityTransientStatuses = map[int]bool{
	http.StatusInternalServerError: true, // 500
	http.StatusBadGateway:          true, // 502
	http.StatusServiceUnavailable:  true, // 503
	http.StatusGatewayTimeout:      true, // 504
}

// antigravityRetryAfterHeader matches the "reset after 2h7m23s" / "1h30m" /
// "45m" / "30s" family of Antigravity quota-reset messages. Mirrors
// parseRetryFromErrorMessage's /reset after (\d+h)?(\d+m)?(\d+s)?/i.
var antigravityRetryAfterHeader = regexp.MustCompile(`(?i)reset after (?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?`)

// parseRetryHeaders mirrors parseRetryHeaders: derive a retry delay in ms from
// the response headers (Retry-After → x-ratelimit-reset-after → x-ratelimit-reset).
// Returns 0 and ok=false when no usable header is present. Retry-After accepts
// either an integer (seconds) or an HTTP-date.
func parseRetryHeaders(h http.Header, now time.Time) (int, bool) {
	if h == nil {
		return 0, false
	}
	if ra := h.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
			return secs * 1000, true
		}
		// HTTP-date form.
		if t, err := http.ParseTime(ra); err == nil {
			diff := int(t.Sub(now).Milliseconds())
			if diff > 0 {
				return diff, true
			}
			return 0, false
		}
	}
	if resetAfter := h.Get("x-ratelimit-reset-after"); resetAfter != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(resetAfter)); err == nil && secs > 0 {
			return secs * 1000, true
		}
	}
	if resetTs := h.Get("x-ratelimit-reset"); resetTs != "" {
		if ts, err := strconv.ParseInt(strings.TrimSpace(resetTs), 10, 64); err == nil && ts > 0 {
			diff := int(ts*1000 - now.UnixMilli())
			if diff > 0 {
				return diff, true
			}
		}
	}
	return 0, false
}

// parseRetryFromErrorMessage mirrors parseRetryFromErrorMessage: parse
// "reset after 2h7m23s" / "1h30m" / "45m" / "30s" from the error body. Returns
// 0, false when the pattern is absent.
func parseRetryFromErrorMessage(errorMessage string) (int, bool) {
	if errorMessage == "" {
		return 0, false
	}
	m := antigravityRetryAfterHeader.FindStringSubmatch(errorMessage)
	if m == nil {
		return 0, false
	}
	totalMs := 0
	if m[1] != "" {
		h, _ := strconv.Atoi(m[1])
		totalMs += h * 3600 * 1000
	}
	if m[2] != "" {
		mm, _ := strconv.Atoi(m[2])
		totalMs += mm * 60 * 1000
	}
	if m[3] != "" {
		s, _ := strconv.Atoi(m[3])
		totalMs += s * 1000
	}
	if totalMs <= 0 {
		return 0, false
	}
	return totalMs, true
}

// extractErrorMessage mirrors extractErrorMessage: join the candidate error
// message fields (errorJson.error.message / message / error / bodyText) into a
// single newline-separated string, JSON-encoding non-string candidates.
func extractErrorMessage(errorJson map[string]any, bodyText string) string {
	parts := []string{}
	add := func(v any) {
		if v == nil {
			return
		}
		if s, ok := v.(string); ok {
			if s != "" {
				parts = append(parts, s)
			}
			return
		}
		b, _ := json.Marshal(v)
		parts = append(parts, string(b))
	}
	if errorJson != nil {
		// Mirror the JS array: [error.message, message, error, bodyText].
		// error.message is only meaningful when error is an object; the raw
		// error value itself (string or object) is always a candidate too.
		if e, ok := errorJson["error"].(map[string]any); ok {
			add(e["message"])
		}
		add(errorJson["message"])
		add(errorJson["error"])
	}
	add(bodyText)
	return strings.Join(parts, "\n")
}

// isTransientAntigravityError mirrors isTransientAntigravityError: 429 or a
// transient status (500/502/503/504) or a matching error pattern in the body.
func isTransientAntigravityError(status int, message string) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if antigravityTransientStatuses[status] {
		return true
	}
	for _, p := range antigravityTransientErrorPatterns {
		if p.MatchString(message) {
			return true
		}
	}
	return false
}

// computeRetryDelay mirrors the JS computeRetryDelay hook. It reads the
// response body (best-effort, limited) so it can parse both Retry-After headers
// and the "reset after ..." error-message form, then decides:
//
//   - explicit hint > maxRetryAfterMs → veto (retry=false).
//   - explicit hint ≤ maxRetryAfterMs  → wait that many ms.
//   - no hint + transient failure      → exponential backoff
//     (1000 * 2^attempt) capped at maxRetryAfterMs (429) or
//     antigravityTransientRetryMaxMs (transient).
//   - no hint + non-transient          → veto (retry=false).
//
// The body is consumed; callers that need it afterwards (none in the retry
// path — a vetoed retry falls through to the next URL) must clone it. The JS
// upstream uses response.clone().text(); here we drain the body into a buffer
// since the retry path does not reuse it.
func computeRetryDelay(response *http.Response, attempt, delayMs int, now time.Time) (int, bool, error) {
	retryMs, ok := parseRetryHeaders(response.Header, now)

	var bodyText string
	var errorJson map[string]any
	if response.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
		bodyText = string(raw)
		if bodyText != "" {
			_ = json.Unmarshal([]byte(bodyText), &errorJson)
		}
	}
	errorMessage := extractErrorMessage(errorJson, bodyText)

	if !ok {
		retryMs, ok = parseRetryFromErrorMessage(errorMessage)
	}
	if ok {
		if retryMs > maxRetryAfterMs {
			return 0, false, nil // veto — hint too long, fall through to next URL
		}
		return retryMs, true, nil
	}
	if !isTransientAntigravityError(response.StatusCode, errorMessage) {
		return 0, false, nil // veto — not retryable
	}
	cap := antigravityTransientRetryMaxMs
	if response.StatusCode == http.StatusTooManyRequests {
		cap = maxRetryAfterMs
	}
	backoff := 1000 * (1 << attempt) // 1000 * 2**attempt
	if backoff > cap {
		backoff = cap
	}
	if backoff < 1 {
		backoff = 1
	}
	return backoff, true, nil
}

// antigravityComputeRetryDelay is the Config.ComputeRetryDelay closure wired
// by New. It binds now to time.Now at call time.
func antigravityComputeRetryDelay(response *http.Response, attempt, delayMs int) (int, bool, error) {
	return computeRetryDelay(response, attempt, delayMs, time.Now())
}
