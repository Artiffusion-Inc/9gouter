package quotafetch

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// glm.go ports open-sse/services/usage/misc.js getGlmUsage. GLM Coding Plan
// usage: GET {international|china}/api/monitor/usage/quota/limit with Bearer
// apiKey, returns {data:{limits:[{type:"TOKENS_LIMIT",percentage,nextResetTime}],
// level}}. We surface one "session" quota row per TOKENS_LIMIT (percentage is
// % used), plus the plan from data.level.

const (
	glmUsageURL   = "https://api.z.ai/api/monitor/usage/quota/limit"
	glmCNUsageURL = "https://open.bigmodel.cn/api/monitor/usage/quota/limit"
)

type glmFetcher struct{ url string }

func (g glmFetcher) Fetch(ctx context.Context, conn *settings.ProviderConnection, do Doer) (*QuotaResult, error) {
	key := apiKey(conn)
	if key == "" {
		return msgResult("GLM API key not available."), nil
	}
	req, err := newGET(ctx, g.url, func(h http.Header) {
		h.Set("Authorization", "Bearer "+key)
		h.Set("Accept", "application/json")
	})
	if err != nil {
		return nil, err
	}
	body, status, err := doJSON(ctx, do, req)
	if err != nil {
		return msgResult("GLM error: " + err.Error()), nil
	}
	if status == 401 {
		return msgResult("GLM API key invalid or expired."), nil
	}
	if status != 200 {
		return msgResult("GLM quota API error (" + strconv.Itoa(status) + ")."), nil
	}
	var raw struct {
		Data struct {
			Level  string           `json:"level"`
			Limits []map[string]any `json:"limits"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &raw)

	quotas := map[string]Quota{}
	for _, limit := range raw.Data.Limits {
		if strField(limit, "type") != "TOKENS_LIMIT" {
			continue
		}
		usedPct := toFinite(limit["percentage"], 0)
		remaining := clampUnit(100 - usedPct)
		resetMS := toFinite(limit["nextResetTime"], 0)
		quotas["session"] = Quota{
			Used:                usedPct,
			Total:               100,
			Remaining:           remaining,
			RemainingPercentage: remaining,
			ResetAt:             epochToISO(int64(resetMS)),
		}
	}
	plan := "Unknown"
	if raw.Data.Level != "" {
		r := []rune(raw.Data.Level)
		plan = strings.ToUpper(string(r[0])) + strings.ToLower(string(r[1:]))
	}
	if len(quotas) == 0 {
		return msgResult("GLM connected. No quota data was returned."), nil
	}
	return &QuotaResult{Plan: plan, Quotas: quotas}, nil
}

func init() {
	register("glm", glmFetcher{url: glmUsageURL})
	register("glm-cn", glmFetcher{url: glmCNUsageURL})
}
