package quotafetch

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// minimax.go ports open-sse/services/usage/minimax.js getMiniMaxUsage. Two
// usage URLs are tried in order; the coding_plan variant treats the count
// field as remaining (countMeansRemaining=true), the token_plan variant
// treats it as used. Each model in model_remains yields two rows: "<name> (5h)"
// (current_interval_*) and "<name> (7d)" (current_weekly_*). Field names are
// accepted in both snake_case and camelCase. resetAt is capturedAtMs +
// remains_time (ms) when present, else end_time.
var minimaxUsageURLs = []string{
	"https://www.minimax.io/v1/token_plan/remains",
	"https://api.minimax.io/v1/api/openplatform/coding_plan/remains",
}

type minimaxFetcher struct{}

func (minimaxFetcher) Fetch(ctx context.Context, conn *settings.ProviderConnection, do Doer) (*QuotaResult, error) {
	key := apiKey(conn)
	if key == "" {
		return msgResult("MiniMax usage unavailable: no API key"), nil
	}
	for _, url := range minimaxUsageURLs {
		req, err := newGET(ctx, url, func(h http.Header) {
			h.Set("Authorization", "Bearer "+key)
			h.Set("Accept", "application/json")
			h.Set("Content-Type", "application/json")
		})
		if err != nil {
			return nil, err
		}
		body, status, err := doJSON(ctx, do, req)
		if err != nil || status != 200 {
			continue
		}
		if res := minimaxParse(body, url); res != nil {
			return res, nil
		}
	}
	return msgResult("MiniMax connected. Unable to parse usage from any endpoint."), nil
}

func minimaxParse(body []byte, usageURL string) *QuotaResult {
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	models, _ := raw["model_remains"].([]any)
	if models == nil {
		models, _ = raw["modelRemains"].([]any)
	}
	countMeansRemaining := containsStr(usageURL, "/coding_plan/remains")
	capturedMs := nowMs()
	quotas := map[string]Quota{}
	for _, e := range models {
		m, ok := e.(map[string]any)
		if !ok || !minimaxHasQuota(m) {
			continue
		}
		display := minimaxName(m)
		quotas[display+" (5h)"] = minimaxRow(m, true, countMeansRemaining, capturedMs)
		quotas[display+" (7d)"] = minimaxRow(m, false, countMeansRemaining, capturedMs)
	}
	if len(quotas) == 0 {
		return nil
	}
	return &QuotaResult{Quotas: quotas}
}

func minimaxRow(m map[string]any, session, countMeansRemaining bool, capturedMs int64) Quota {
	var total float64
	if session {
		total = mmFieldMax0(m, "current_interval_total_count", "currentIntervalTotalCount")
	} else {
		total = mmFieldMax0(m, "current_weekly_total_count", "currentWeeklyTotalCount")
	}
	var countKey, countCamel, pctKey, pctCamel string
	if session {
		countKey, countCamel = "current_interval_usage_count", "currentIntervalUsageCount"
		pctKey, pctCamel = "current_interval_remaining_percent", "currentIntervalRemainingPercent"
	} else {
		countKey, countCamel = "current_weekly_usage_count", "currentWeeklyUsageCount"
		pctKey, pctCamel = "current_weekly_remaining_percent", "currentWeeklyRemainingPercent"
	}
	count := mmField(m, countKey, countCamel)
	providedPct := mmField(m, pctKey, pctCamel)

	var used float64
	if countMeansRemaining {
		used = total - count
		if used < 0 {
			used = 0
		}
	} else {
		used = count
		if used > total {
			used = total
		}
		if used < 0 {
			used = 0
		}
	}
	remaining := total - used
	if remaining < 0 {
		remaining = 0
	}
	pct := providedPct
	if pct == 0 && total > 0 {
		pct = (remaining / total) * 100
	}
	pct = clampUnit(pct)

	resetAt := ""
	var remainsKey, remainsCamel, endKey, endCamel string
	if session {
		remainsKey, remainsCamel = "remains_time", "remainsTime"
		endKey, endCamel = "end_time", "endTime"
	} else {
		remainsKey, remainsCamel = "weekly_remains_time", "weeklyRemainsTime"
		endKey, endCamel = "weekly_end_time", "weeklyEndTime"
	}
	remainsMs := mmField(m, remainsKey, remainsCamel)
	if remainsMs > 0 {
		resetAt = epochToISO(capturedMs + int64(remainsMs))
	} else {
		resetAt = parseResetTime(mmFieldVal(m, endKey, endCamel))
	}
	return Quota{Used: used, Total: total, Remaining: remaining, RemainingPercentage: pct, ResetAt: resetAt}
}

func mmField(m map[string]any, snake, camel string) float64 {
	if v, ok := m[snake]; ok {
		if f := toFinite(v, 0); f != 0 || v != nil {
			return f
		}
	}
	if v, ok := m[camel]; ok {
		return toFinite(v, 0)
	}
	return 0
}

func mmFieldVal(m map[string]any, snake, camel string) any {
	if v, ok := m[snake]; ok {
		return v
	}
	if v, ok := m[camel]; ok {
		return v
	}
	return nil
}

func mmFieldMax0(m map[string]any, snake, camel string) float64 {
	v := mmField(m, snake, camel)
	if v < 0 {
		return 0
	}
	return v
}

// minimaxHasQuota mirrors hasMiniMaxQuota: a model row counts when it carries a
// non-zero session total OR a session count field.
func minimaxHasQuota(m map[string]any) bool {
	if mmFieldMax0(m, "current_interval_total_count", "currentIntervalTotalCount") > 0 {
		return true
	}
	return mmField(m, "current_interval_usage_count", "currentIntervalUsageCount") > 0
}

func minimaxName(m map[string]any) string {
	if n := strField(m, "model", "name", "displayName"); n != "" {
		return n
	}
	return "model"
}

func containsStr(s, sub string) bool {
	return indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// nowMs is a time wrapper so tests can inject a fixed clock by reassigning it.
var nowMs = func() int64 { return time.Now().UnixMilli() }

func init() {
	register("minimax", minimaxFetcher{})
	register("minimax-cn", minimaxFetcher{})
	_ = strconv.Itoa
}
