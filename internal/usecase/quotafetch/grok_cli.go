package quotafetch

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// grok_cli.go ports open-sse/services/usage/grok-cli.js getGrokCliUsage. Two
// GETs run (billing + user); the user call is best-effort (failure ignored).
// Headers carry the grok-shell fingerprint + optional x-email/x-userid from
// providerSpecificData. Billing values may be wrapped as {val: n} objects
// (unwrapVal). Emits "Monthly included", "On-demand", "Prepaid", "Credits"
// rows as available; total=0 → unlimited row. Plan resolves from the user
// subscription tier / hasGrokCodeAccess / isUnifiedBillingUser.
const (
	grokCliBillingURL = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	grokCliUserURL    = "https://cli-chat-proxy.grok.com/v1/user?include=subscription"

	grokCliVersion          = "0.2.99"
	grokCliClientIdentifier = "grok-shell"
	grokCliUserAgent        = "grok-shell/0.2.99 (linux; x86_64)"
)

type grokCliFetcher struct{}

func (grokCliFetcher) Fetch(ctx context.Context, conn *settings.ProviderConnection, do Doer) (*QuotaResult, error) {
	tok := accessToken(conn)
	if tok == "" {
		return msgResult("Grok CLI usage unavailable: no access token"), nil
	}
	p := psd(conn)
	hdr := func(h http.Header) {
		h.Set("Authorization", "Bearer "+tok)
		h.Set("Accept", "application/json")
		h.Set("User-Agent", grokCliUserAgent)
		h.Set("x-xai-token-auth", "xai-grok-cli")
		h.Set("x-grok-client-identifier", grokCliClientIdentifier)
		h.Set("x-grok-client-version", grokCliVersion)
		h.Set("x-grok-client-mode", "headless")
		if email := strField(p, "email"); email != "" {
			h.Set("x-email", email)
		}
		if uid := strField(p, "userId", "principalId"); uid != "" {
			h.Set("x-userid", uid)
		}
	}

	billReq, err := newGET(ctx, grokCliBillingURL, hdr)
	if err != nil {
		return nil, err
	}
	billBody, billStatus, billErr := doJSON(ctx, do, billReq)
	if billErr != nil || billStatus != 200 {
		return msgResult("Grok CLI connected. Billing API unavailable."), nil
	}

	// Best-effort user fetch.
	var user map[string]any
	if userReq, e := newGET(ctx, grokCliUserURL, hdr); e == nil {
		if ub, us, ue := doJSON(ctx, do, userReq); ue == nil && us == 200 {
			_ = json.Unmarshal(ub, &user)
		}
	}

	var billing map[string]any
	_ = json.Unmarshal(billBody, &billing)
	parsed := grokCliParse(billing, user)
	if len(parsed.Quotas) == 0 {
		res := msgResult(grokCliEmptyMessage(user, billing))
		if parsed.Plan != "" {
			res.Plan = parsed.Plan
		}
		return res, nil
	}
	parsed.Plan = grokCliResolvePlan(user, billing)
	return parsed, nil
}

func grokCliParse(billing, user map[string]any) *QuotaResult {
	root := billing
	config, _ := root["config"].(map[string]any)
	if config == nil {
		config = map[string]any{}
	}
	subscriptionAccess := grokCliSubscriptionAccess(user, config)
	periodEnd := grokCliPeriodEnd(root, config)
	quotas := map[string]Quota{}

	monthlyLimit := grokCliUnwrapRootOrConfig(config, root, []string{"monthlyLimit", "monthly_limit"}, []string{"monthlyLimit", "monthly_limit"})
	if monthlyLimit > 0 {
		includedUsed := grokCliUnwrapRootOrConfig(config, root, []string{"includedUsed", "included_used"}, []string{"includedUsed", "included_used"})
		if !(includedUsed > 0) {
			includedUsed = grokCliUnwrapRootOrConfig(config, root, []string{"totalUsed", "total_used"}, []string{"totalUsed", "total_used"})
		}
		quotas["Monthly included"] = grokCliMakeQuota(includedUsed, monthlyLimit, periodEnd, false)
	}

	onDemandCap := grokCliUnwrapRootOrConfig(config, root, []string{"onDemandCap"}, []string{"onDemandCap"})
	onDemandUsed := grokCliUnwrapRootOrConfig(config, root, []string{"onDemandUsed"}, []string{"onDemandUsed"})
	if onDemandCap > 0 {
		used := onDemandUsed
		if used < 0 {
			used = 0
		}
		quotas["On-demand"] = grokCliMakeQuota(used, onDemandCap, periodEnd, false)
	} else if !subscriptionAccess && onDemandCap == 0 && onDemandUsed > 0 {
		quotas["On-demand"] = Quota{Used: 1, Total: 1, RemainingPercentage: 0, ResetAt: periodEnd}
	}

	prepaid := grokCliUnwrapRootOrConfig(config, root, []string{"prepaidBalance"}, []string{"prepaidBalance"})
	if prepaid > 0 {
		quotas["Prepaid"] = Quota{Total: prepaid, RemainingPercentage: 100}
	}

	// Opportunistic credit bags.
	for _, bag := range grokCliCreditBags(root, config) {
		if _, dup := quotas["Credits"]; dup {
			break
		}
		total := grokCliUnwrap(bagVal(bag, "total", "limit", "cap", "allocation", "amount"), 0)
		used := grokCliUnwrap(bagVal(bag, "used", "spent", "consumed"), 0)
		remaining := grokCliUnwrap(bagVal(bag, "remaining", "balance", "left"), 0)
		if total > 0 {
			if !(used > 0) {
				if remaining > 0 {
					used = total - remaining
					if used < 0 {
						used = 0
					}
				} else {
					used = 0
				}
			}
			reset := parseResetTime(bagVal(bag, "resetAt", "resetsAt", "end"))
			if reset == "" {
				reset = periodEnd
			}
			quotas["Credits"] = grokCliMakeQuota(used, total, reset, false)
		} else if remaining >= 0 {
			quotas["Credits"] = Quota{Total: remainingVal(remaining), RemainingPercentage: remainingPct(remaining)}
		}
	}

	return &QuotaResult{Quotas: quotas}
}

func grokCliMakeQuota(used, total float64, resetAt string, unlimited bool) Quota {
	if used < 0 {
		used = 0
	}
	if total < 0 {
		total = 0
	}
	if unlimited || total == 0 {
		return Quota{Used: used, Total: 0, RemainingPercentage: boolPct(unlimited), ResetAt: resetAt, Unlimited: true}
	}
	remaining := total - used
	if remaining < 0 {
		remaining = 0
	}
	return Quota{Used: used, Total: total, RemainingPercentage: clampUnit((remaining / total) * 100), ResetAt: resetAt}
}

// grokCliUnwrap mirrors unwrapVal: a value may be {val: n} or a bare number.
func grokCliUnwrap(v any, fallback float64) float64 {
	if v == nil {
		return fallback
	}
	if m, ok := v.(map[string]any); ok {
		if val, ok := m["val"]; ok {
			return toFinite(val, fallback)
		}
		return fallback
	}
	return toFinite(v, fallback)
}

// grokCliUnwrap with a root fallback: tries config keys then root keys.
func grokCliUnwrapRootOrConfig(cfg, root map[string]any, cfgKeys, rootKeys []string) float64 {
	for _, k := range cfgKeys {
		if v, ok := cfg[k]; ok {
			if f := grokCliUnwrap(v, 0); f != 0 || v != nil {
				return f
			}
		}
	}
	for _, k := range rootKeys {
		if v, ok := root[k]; ok {
			return grokCliUnwrap(v, 0)
		}
	}
	return 0
}

func bagVal(bag map[string]any, keys ...string) any {
	return firstNonNil(bag, keys...)
}

func grokCliCreditBags(root, config map[string]any) []map[string]any {
	var out []map[string]any
	for _, src := range []map[string]any{root, config} {
		for _, k := range []string{"credits", "creditBalance", "usage", "includedCredits", "subscriptionCredits"} {
			if v, ok := src[k]; ok {
				if m, ok := v.(map[string]any); ok {
					out = append(out, m)
				}
			}
		}
	}
	return out
}

func grokCliPeriodEnd(root, config map[string]any) string {
	cands := []any{
		firstNonNil(config, "billingPeriodEnd", "billing_period_end"),
	}
	if cp, ok := config["currentPeriod"].(map[string]any); ok {
		cands = append(cands, cp["end"])
	}
	cands = append(cands, firstNonNil(config, "resetAt", "resetsAt", "periodEnd"))
	cands = append(cands, firstNonNil(root, "billingPeriodEnd", "billing_period_end", "resetAt", "resetsAt", "periodEnd"))
	for _, c := range cands {
		if s := parseResetTime(c); s != "" {
			return s
		}
	}
	return ""
}

func grokCliSubscriptionAccess(user, config map[string]any) bool {
	if user == nil {
		return false
	}
	if v, ok := user["hasGrokCodeAccess"].(bool); ok && v {
		return true
	}
	if v, ok := config["isUnifiedBillingUser"].(bool); ok && v {
		return true
	}
	return false
}

func grokCliResolvePlan(user, config map[string]any) string {
	if user != nil {
		if t := strField(user, "subscriptionTier", "tier"); t != "" {
			return t
		}
		if v, ok := user["hasGrokCodeAccess"].(bool); ok && v {
			return "Grok Code"
		}
	}
	if v, ok := config["isUnifiedBillingUser"].(bool); ok && v {
		return "Grok Build"
	}
	return "Grok Build"
}

func grokCliEmptyMessage(user, config map[string]any) string {
	if grokCliSubscriptionAccess(user, config) {
		return "Subscription access is active; Grok does not expose a numeric included quota."
	}
	return "Grok Build connected, but no credit allotment was returned. Free promo may be exhausted."
}

func boolPct(b bool) float64 {
	if b {
		return 100
	}
	return 0
}

func remainingVal(r float64) float64 {
	if r > 0 {
		return r
	}
	return 1
}

func remainingPct(r float64) float64 {
	if r > 0 {
		return 100
	}
	return 0
}

func init() {
	register("grok-cli", grokCliFetcher{})
}
