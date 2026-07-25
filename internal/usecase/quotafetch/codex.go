package quotafetch

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// codex.go ports open-sse/services/usage/codex.js getCodexUsage (the main
// usage endpoint — the reset-credits GET/POST is already ported in the api
// package). GET chatgpt.com/backend-api/wham/usage with Bearer accessToken →
// {plan_type, rate_limit:{primary_window,secondary_window,...}, code_review_rate_limit,
// rate_limit_reset_credits:{available_count}}. Each window carries used_percent
// + reset_at; we surface "session" and "weekly" rows (plus review_ variants).
const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type codexFetcher struct{}

func (codexFetcher) Fetch(ctx context.Context, conn *settings.ProviderConnection, do Doer) (*QuotaResult, error) {
	tok := accessToken(conn)
	if tok == "" {
		return msgResult("Codex connected. Usage API temporarily unavailable."), nil
	}
	req, err := newGET(ctx, codexUsageURL, func(h http.Header) {
		h.Set("Authorization", "Bearer "+tok)
		h.Set("Accept", "application/json")
	})
	if err != nil {
		return nil, err
	}
	body, status, err := doJSON(ctx, do, req)
	if err != nil {
		return msgResult("Codex connected. Usage API temporarily unavailable."), nil
	}
	if status != 200 {
		return msgResult("Codex connected. Usage API temporarily unavailable (" + strconv.Itoa(status) + ")."), nil
	}
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)

	quotas := map[string]Quota{}
	normal := codexRateLimit(raw, "rate_limit", "rate_limits")
	codexAppendWindows(quotas, "", normal)
	review := codexReviewRateLimit(raw)
	codexAppendWindows(quotas, "review", review)

	plan := anyString(raw["plan_type"])
	if plan == "" {
		if summ, ok := raw["summary"].(map[string]any); ok {
			plan = anyString(summ["plan"])
		}
	}
	res := &QuotaResult{Plan: plan, Quotas: quotas}
	if creds, ok := raw["rate_limit_reset_credits"].(map[string]any); ok {
		res.Extra = map[string]any{
			"resetCredits": map[string]any{
				"availableCount": toFinite(creds["available_count"], 0),
			},
		}
	}
	if len(quotas) == 0 && plan == "" {
		return msgResult("Codex connected. Unable to parse usage data."), nil
	}
	return res, nil
}

// codexRateLimit resolves the normal rate_limit body across the shapes the JS
// handler tolerates: data.rate_limit | data.rate_limits | data.rate_limits_by_limit_id.codex.
func codexRateLimit(data map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if m, ok := data[k].(map[string]any); ok {
			return m
		}
	}
	if byID, ok := data["rate_limits_by_limit_id"].(map[string]any); ok {
		if m, ok := byID["codex"].(map[string]any); ok {
			return m
		}
	}
	return nil
}

// codexReviewRateLimit mirrors getCodexReviewRateLimit: code_review_rate_limit
// | review_rate_limit | rate_limits_by_limit_id.code_review/codex_review/review
// | additional_rate_limits[*].limit_name contains "review".
func codexReviewRateLimit(data map[string]any) map[string]any {
	for _, k := range []string{"code_review_rate_limit", "review_rate_limit"} {
		if m, ok := data[k].(map[string]any); ok {
			return m
		}
	}
	if byID, ok := data["rate_limits_by_limit_id"].(map[string]any); ok {
		for _, k := range []string{"code_review", "codex_review", "review"} {
			if m, ok := byID[k].(map[string]any); ok {
				return m
			}
		}
	}
	if add, ok := data["additional_rate_limits"].([]any); ok {
		for _, e := range add {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			id := strField(m, "limit_name", "metered_feature", "id")
			if strings.Contains(strings.ToLower(id), "review") {
				return m
			}
		}
	}
	return nil
}

func codexAppendWindows(quotas map[string]Quota, prefix string, body map[string]any) {
	if body == nil {
		return
	}
	rl := body
	if inner, ok := body["rate_limit"].(map[string]any); ok {
		rl = inner
	}
	primary := objFirst(rl, "primary_window", "primary")
	secondary := objFirst(rl, "secondary_window", "secondary")
	if primary != nil {
		quotas[codexWindowName(prefix, "session")] = codexFormatWindow(primary)
	}
	if secondary != nil {
		quotas[codexWindowName(prefix, "weekly")] = codexFormatWindow(secondary)
	}
}

func codexWindowName(prefix, kind string) string {
	if prefix == "" {
		return kind
	}
	return prefix + "_" + kind
}

func codexFormatWindow(w map[string]any) Quota {
	used := toFinite(firstNonNil(w, "used_percent", "percent_used"), 0)
	used = clampUnit(used)
	reset := parseResetTime(firstNonNil(w, "reset_at", "resets_at", "resetAt"))
	return Quota{
		Used:      used,
		Total:     100,
		Remaining: clampUnit(100 - used),
		ResetAt:   reset,
	}
}

func firstNonNil(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

func objFirst(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if o, ok := m[k].(map[string]any); ok {
			return o
		}
	}
	return nil
}

func init() {
	register("codex", codexFetcher{})
}
