package quotafetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// claude.go ports open-sse/services/usage/claude.js getClaudeUsage. Primary path
// is the OAuth usage endpoint (Bearer accessToken + anthropic-beta
// oauth-2025-04-20 + anthropic-version); on HTTP error / unparseable body it
// falls back to the legacy settings→org-usage path. The OAuth body carries
// window objects (five_hour, seven_day, seven_day_<model>) each with a
// `utilization` percent-used number and `resets_at`; we surface them as quota
// rows scaled to total=100.
const (
	claudeOAuthUsageURL = "https://api.anthropic.com/api/oauth/usage"
	claudeSettingsURL   = "https://api.anthropic.com/v1/settings"
	claudeOrgUsageURL   = "https://api.anthropic.com/v1/organizations/%s/usage"
	claudeAPIVersion    = "2023-06-01"
	claudeOAuthBeta     = "oauth-2025-04-20"
)

type claudeFetcher struct{}

func (claudeFetcher) Fetch(ctx context.Context, conn *settings.ProviderConnection, do Doer) (*QuotaResult, error) {
	tok := accessToken(conn)
	if tok == "" {
		return msgResult("Claude connected. No access token available — please re-authorize."), nil
	}
	// Primary: OAuth usage endpoint.
	req, err := newGET(ctx, claudeOAuthUsageURL, func(h http.Header) {
		h.Set("Authorization", "Bearer "+tok)
		h.Set("anthropic-beta", claudeOAuthBeta)
		h.Set("anthropic-version", claudeAPIVersion)
		h.Set("Accept", "application/json")
	})
	if err != nil {
		return nil, err
	}
	body, status, err := doJSON(ctx, do, req)
	if err == nil && status == 200 {
		if res := claudeParseOAuth(body); res != nil {
			return res, nil
		}
	}
	// Fallback: legacy settings → org usage.
	return claudeLegacy(ctx, tok, do)
}

// claudeParseOAuth mirrors the JS OAuth parsing. Returns nil when no window
// carried a numeric utilization (caller falls back to legacy).
func claudeParseOAuth(body []byte) *QuotaResult {
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	quotas := map[string]Quota{}
	if w := claudeWindow(raw, "five_hour"); w != nil {
		quotas["session (5h)"] = *w
	}
	if w := claudeWindow(raw, "seven_day"); w != nil {
		quotas["weekly (7d)"] = *w
	}
	for k, v := range raw {
		if !startsWith(k, "seven_day_") || k == "seven_day" {
			continue
		}
		w := claudeWindowVal(v)
		if w == nil {
			continue
		}
		quotas["weekly "+k[len("seven_day_"):]+" (7d)"] = *w
	}
	if len(quotas) == 0 {
		return nil
	}
	res := &QuotaResult{Plan: "Claude Code", Quotas: quotas}
	if eu, ok := raw["extra_usage"]; ok {
		res.Extra = map[string]any{"extraUsage": eu}
	}
	return res
}

// claudeWindow resolves a top-level window object → Quota when it carries a
// numeric utilization; nil otherwise.
func claudeWindow(raw map[string]any, key string) *Quota {
	v, ok := raw[key]
	if !ok {
		return nil
	}
	return claudeWindowVal(v)
}

func claudeWindowVal(v any) *Quota {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	util, ok := m["utilization"].(float64)
	if !ok {
		return nil
	}
	used := clampUnit(util)
	remaining := 100 - used
	if remaining < 0 {
		remaining = 0
	}
	return &Quota{
		Used:                used,
		Total:               100,
		Remaining:           remaining,
		RemainingPercentage: remaining,
		ResetAt:             parseResetTime(m["resets_at"]),
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// claudeLegacy mirrors getClaudeUsageLegacy: GET /v1/settings → read
// organization_id + plan, then GET /v1/organizations/{org}/usage and pass the
// raw upstream usage object through as the quotas map.
func claudeLegacy(ctx context.Context, tok string, do Doer) (*QuotaResult, error) {
	req, err := newGET(ctx, claudeSettingsURL, func(h http.Header) {
		h.Set("Authorization", "Bearer "+tok)
		h.Set("anthropic-version", claudeAPIVersion)
		h.Set("Accept", "application/json")
	})
	if err != nil {
		return msgResult("Claude connected. Unable to fetch settings: " + err.Error()), nil
	}
	body, status, err := doJSON(ctx, do, req)
	if err != nil || status != 200 {
		return msgResult("Claude connected. Usage API unavailable (" + strconv.Itoa(status) + ")."), nil
	}
	var settings map[string]any
	_ = json.Unmarshal(body, &settings)
	orgID, _ := settings["organization_id"].(string)
	plan, _ := settings["plan"].(string)
	orgName, _ := settings["organization_name"].(string)
	if orgID == "" {
		return &QuotaResult{Plan: plan, Quotas: map[string]Quota{}}, nil
	}
	orgReq, err := newGET(ctx, fmt.Sprintf(claudeOrgUsageURL, orgID), func(h http.Header) {
		h.Set("Authorization", "Bearer "+tok)
		h.Set("anthropic-version", claudeAPIVersion)
		h.Set("Accept", "application/json")
	})
	if err != nil {
		return nil, err
	}
	orgBody, orgStatus, err := doJSON(ctx, do, orgReq)
	if err != nil || orgStatus != 200 {
		return &QuotaResult{Plan: plan, Quotas: map[string]Quota{}}, nil
	}
	var usage map[string]any
	_ = json.Unmarshal(orgBody, &usage)
	res := &QuotaResult{Plan: plan, Quotas: map[string]Quota{}}
	if orgName != "" {
		res.Extra = map[string]any{"organization": orgName}
	}
	// Pass the raw upstream usage object through keyed by the documented shape
	// (the JS handler returned it verbatim as `quotas`).
	if len(usage) > 0 {
		res.Extra = mergeExtra(res.Extra, map[string]any{"usage": usage})
	}
	return res, nil
}

func mergeExtra(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func init() {
	register("claude", claudeFetcher{})
}
