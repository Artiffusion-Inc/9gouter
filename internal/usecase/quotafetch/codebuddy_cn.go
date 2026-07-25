package quotafetch

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// codebuddy_cn.go ports open-sse/services/usage/codebuddy-cn.js
// getCodeBuddyCnUsage. POST copilot.tencent.com/v2/billing/meter/get-user-resource
// with body "{}", Authorization Bearer (accessToken || apiKey) + provider
// transport headers. Response: data.Response.Data.Accounts[]. An account is a
// "refill" pack when DeductionEndTime − CycleEndTime > 2d (gap → recurring),
// else a one-shot "Bonus Pack". Refill packs are named Daily/Weekly/Monthly by
// the CycleStartTime→CycleEndTime span, with a numeric suffix when duplicated.
const codebuddyCnUsageURL = "https://copilot.tencent.com/v2/billing/meter/get-user-resource"

// codebuddyCnRefillGapMs mirrors the JS REFILL_GAP_MS (2 days).
const codebuddyCnRefillGapMs = int64(2 * 24 * 60 * 60 * 1000)

type codebuddyCnFetcher struct{}

func (codebuddyCnFetcher) Fetch(ctx context.Context, conn *settings.ProviderConnection, do Doer) (*QuotaResult, error) {
	tok := accessToken(conn)
	if tok == "" {
		tok = apiKey(conn)
	}
	if tok == "" {
		return msgResult("CodeBuddy CN usage unavailable: no token"), nil
	}
	req, err := newJSON(ctx, http.MethodPost, codebuddyCnUsageURL, []byte("{}"), func(h http.Header) {
		h.Set("Authorization", "Bearer "+tok)
		h.Set("Content-Type", "application/json")
		h.Set("Accept", "application/json")
		h.Set("User-Agent", "CodeBuddy")
		h.Set("X-Product", "codebuddy")
		h.Set("X-IDE-Type", "vscode")
		h.Set("X-IDE-Name", "vscode")
		h.Set("x-requested-with", "XMLHttpRequest")
		h.Set("x-codebuddy-request", "true")
	})
	if err != nil {
		return nil, err
	}
	body, status, err := doJSON(ctx, do, req)
	if err != nil || status != 200 {
		return msgResult("CodeBuddy CN connected. Usage API unavailable."), nil
	}
	var raw struct {
		Data struct {
			Response struct {
				Data struct {
					Accounts []map[string]any `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &raw)
	accounts := raw.Data.Response.Data.Accounts
	if len(accounts) == 0 {
		return msgResult("CodeBuddy CN connected. No billing accounts returned."), nil
	}
	return codebuddyParse(accounts), nil
}

func codebuddyParse(accounts []map[string]any) *QuotaResult {
	var refills, bonuses []map[string]any
	for _, acc := range accounts {
		if codebuddyIsRefill(acc) {
			refills = append(refills, acc)
		} else {
			bonuses = append(bonuses, acc)
		}
	}
	refills = sortByCycleEnd(refills)
	bonuses = sortByCycleEnd(bonuses)

	quotas := map[string]Quota{}
	seen := map[string]int{}
	for _, acc := range refills {
		base := codebuddyCadence(acc)
		seen[base]++
		name := base
		if seen[base] > 1 {
			name = base + " " + itoa(seen[base])
		}
		quotas[name] = Quota{
			Used:      codebuddyNum(acc, "CycleCapacityUsedPrecise", "CycleCapacityUsed"),
			Total:     codebuddyNum(acc, "CycleCapacitySizePrecise", "CycleCapacitySize"),
			ResetAt:   parseResetTime(acc["CycleEndTime"]),
			Recurring: true,
		}
	}
	for i, acc := range bonuses {
		quotas["Bonus Pack "+itoa(i+1)] = Quota{
			Used:    codebuddyNum(acc, "CapacityUsedPrecise", "CapacityUsed"),
			Total:   codebuddyNum(acc, "CapacitySizePrecise", "CapacitySize"),
			ResetAt: parseResetTime(acc["CycleEndTime"]),
		}
	}
	plan := "CodeBuddy CN"
	if len(refills) > 0 {
		plan = firstNonEmptyStr(refills[0], "PackageName", "SubProductName")
	} else if len(accounts) > 0 {
		plan = firstNonEmptyStr(accounts[0], "PackageName", "SubProductName")
	}
	return &QuotaResult{Plan: plan, Quotas: quotas}
}

func codebuddyIsRefill(acc map[string]any) bool {
	ce := codebuddyCycleEndMs(acc)
	if ce < 0 {
		return false
	}
	de := toFinite(acc["DeductionEndTime"], 0)
	return de-float64(ce) > float64(codebuddyCnRefillGapMs)
}

func codebuddyCycleEndMs(acc map[string]any) int64 {
	r := parseResetTime(acc["CycleEndTime"])
	if r == "" {
		return -1
	}
	t, err := time.Parse(time.RFC3339, r)
	if err != nil {
		return -1
	}
	return t.UnixMilli()
}

func codebuddyCadence(acc map[string]any) string {
	start := parseResetTime(acc["CycleStartTime"])
	end := parseResetTime(acc["CycleEndTime"])
	if start != "" && end != "" {
		ts, e1 := time.Parse(time.RFC3339, start)
		te, e2 := time.Parse(time.RFC3339, end)
		if e1 == nil && e2 == nil {
			days := te.Sub(ts).Hours() / 24
			if days <= 1.5 {
				return "Daily"
			}
			if days <= 10 {
				return "Weekly"
			}
		}
	}
	return "Monthly"
}

func codebuddyNum(m map[string]any, precise, plain string) float64 {
	if v, ok := m[precise]; ok {
		if f := toFinite(v, 0); f != 0 || v != nil {
			return f
		}
	}
	return toFinite(m[plain], 0)
}

func firstNonEmptyStr(m map[string]any, keys ...string) string {
	return strField(m, keys...)
}

func sortByCycleEnd(accs []map[string]any) []map[string]any {
	// stable insertion sort by CycleEndMs ascending (earliest-expiring first),
	// matching the JS byExpiry comparator. accounts with no parseable end sort last.
	for i := 1; i < len(accs); i++ {
		for j := i; j > 0; j-- {
			a := codebuddyCycleEndMs(accs[j-1])
			b := codebuddyCycleEndMs(accs[j])
			if a < 0 {
				a = 1 << 62
			}
			if b < 0 {
				b = 1 << 62
			}
			if b < a {
				accs[j-1], accs[j] = accs[j], accs[j-1]
			} else {
				break
			}
		}
	}
	return accs
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func init() {
	register("codebuddy-cn", codebuddyCnFetcher{})
}
