package quotafetch

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// github.go ports open-sse/services/usage/github.js getGitHubUsage. Uses the
// GitHub OAuth accessToken (not the copilotToken) to call
// GET api.github.com/copilot_internal/user with the Copilot editor fingerprint
// headers. Two response shapes:
//   - paid plan: {copilot_plan, quota_snapshots:{chat,completions,premium_interactions},
//     quota_reset_date} → entitlement/remaining per snapshot.
//   - free/limited: {monthly_quotas:{chat,completions}, limited_user_quotas:{...},
//     limited_user_reset_date} → used=limited, total=monthly.
const githubUsageURL = "https://api.github.com/copilot_internal/user"

const (
	githubAPIVersion    = "2022-11-28"
	githubUserAgent     = "GitHubCopilotChat"
	githubEditorVersion = "vscode/1.100.0"
	githubEditorPlugin  = "copilot-chat/0.26.7"
)

type githubFetcher struct{}

func (githubFetcher) Fetch(ctx context.Context, conn *settings.ProviderConnection, do Doer) (*QuotaResult, error) {
	tok := accessToken(conn)
	if tok == "" {
		return msgResult("No GitHub access token available. Please re-authorize the connection."), nil
	}
	req, err := newGET(ctx, githubUsageURL, func(h http.Header) {
		h.Set("Authorization", "token "+tok)
		h.Set("Accept", "application/json")
		h.Set("X-GitHub-Api-Version", githubAPIVersion)
		h.Set("User-Agent", githubUserAgent)
		h.Set("Editor-Version", githubEditorVersion)
		h.Set("Editor-Plugin-Version", githubEditorPlugin)
	})
	if err != nil {
		return nil, err
	}
	body, status, err := doJSON(ctx, do, req)
	if err != nil {
		return msgResult("Failed to fetch GitHub usage: " + err.Error()), nil
	}
	if status != 200 {
		return msgResult("GitHub API error: HTTP " + strconv.Itoa(status)), nil
	}
	var raw struct {
		CopilotPlan          any            `json:"copilot_plan"`
		AccessTypeSku        any            `json:"access_type_sku"`
		QuotaSnapshots       map[string]any `json:"quota_snapshots"`
		QuotaResetDate       any            `json:"quota_reset_date"`
		MonthlyQuotas        map[string]any `json:"monthly_quotas"`
		LimitedUserQuotas    map[string]any `json:"limited_user_quotas"`
		LimitedUserResetDate any            `json:"limited_user_reset_date"`
	}
	_ = json.Unmarshal(body, &raw)

	if len(raw.QuotaSnapshots) > 0 {
		resetAt := parseResetTime(raw.QuotaResetDate)
		quotas := map[string]Quota{}
		for _, name := range []string{"chat", "completions", "premium_interactions"} {
			if snap, ok := raw.QuotaSnapshots[name].(map[string]any); ok {
				quotas[name] = githubSnapshotQuota(snap, resetAt)
			}
		}
		return &QuotaResult{Plan: anyString(raw.CopilotPlan), Quotas: quotas}, nil
	}
	if len(raw.MonthlyQuotas) > 0 || len(raw.LimitedUserQuotas) > 0 {
		resetAt := parseResetTime(raw.LimitedUserResetDate)
		plan := anyString(raw.CopilotPlan)
		if plan == "" {
			plan = anyString(raw.AccessTypeSku)
		}
		quotas := map[string]Quota{}
		for _, name := range []string{"chat", "completions"} {
			total := toFinite(raw.MonthlyQuotas[name], 0)
			used := toFinite(raw.LimitedUserQuotas[name], 0)
			quotas[name] = Quota{Used: used, Total: total, ResetAt: resetAt}
		}
		return &QuotaResult{Plan: plan, Quotas: quotas}, nil
	}
	return msgResult("GitHub Copilot connected. Unable to parse quota data."), nil
}

func githubSnapshotQuota(snap map[string]any, resetAt string) Quota {
	if snap == nil {
		return Quota{Unlimited: true}
	}
	entitlement := toFinite(snap["entitlement"], 0)
	remaining := toFinite(snap["remaining"], 0)
	unlimited, _ := snap["unlimited"].(bool)
	return Quota{
		Used:      entitlement - remaining,
		Total:     entitlement,
		Remaining: remaining,
		Unlimited: unlimited,
		ResetAt:   resetAt,
	}
}

// anyString stringifies a JSON string value (float64/bool yield "").
func anyString(v any) string {
	s, _ := v.(string)
	return s
}

func init() {
	register("github", githubFetcher{})
}
