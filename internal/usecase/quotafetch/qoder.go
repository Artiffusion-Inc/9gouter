package quotafetch

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// qoder.go ports open-sse/services/usage/misc.js getQoderUsage.
// GET https://openapi.qoder.sh/api/v2/quota/usage with Bearer accessToken →
// {userQuota:{total,used,remaining,unit}, orgResourcePackage:{...},
// totalUsagePercentage, isQuotaExceeded, expiresAt(ms)}. Surfaced as two
// quota rows (user / organization) carrying unit + resetAt, plus scalar
// metadata siblings (totalUsagePercentage, isQuotaExceeded, expiresAt).
const qoderUsageURL = "https://openapi.qoder.sh/api/v2/quota/usage"

type qoderFetcher struct{}

func (qoderFetcher) Fetch(ctx context.Context, conn *settings.ProviderConnection, do Doer) (*QuotaResult, error) {
	tok := accessToken(conn)
	if tok == "" {
		return msgResult("Qoder usage unavailable: no access token"), nil
	}
	req, err := newGET(ctx, qoderUsageURL, func(h http.Header) {
		h.Set("Authorization", "Bearer "+tok)
		h.Set("Accept", "application/json")
	})
	if err != nil {
		return nil, err
	}
	body, status, err := doJSON(ctx, do, req)
	if err != nil {
		return msgResult("Qoder connected. Unable to fetch usage: " + err.Error()), nil
	}
	if status != 200 {
		return msgResult("Qoder connected. Usage fetch returned " + strconv.Itoa(status) + "."), nil
	}
	var raw struct {
		UserQuota     map[string]any `json:"userQuota"`
		OrgQuota      map[string]any `json:"orgResourcePackage"`
		TotalUsagePct any            `json:"totalUsagePercentage"`
		IsQuotaExceed any            `json:"isQuotaExceeded"`
		ExpiresAt     any            `json:"expiresAt"`
	}
	_ = json.Unmarshal(body, &raw)

	expiresMs := toFinite(raw.ExpiresAt, 0)
	resetAt := ""
	if expiresMs > 0 {
		resetAt = epochToISO(int64(expiresMs))
	}

	quotas := map[string]Quota{}
	if raw.UserQuota != nil {
		quotas["user"] = qoderQuota(raw.UserQuota, resetAt)
	}
	if raw.OrgQuota != nil {
		quotas["organization"] = qoderQuota(raw.OrgQuota, resetAt)
	}
	res := &QuotaResult{Quotas: quotas, Extra: map[string]any{}}
	res.Extra["totalUsagePercentage"] = toFinite(raw.TotalUsagePct, 0)
	res.Extra["isQuotaExceeded"] = raw.IsQuotaExceed == true
	if expiresMs > 0 {
		res.Extra["expiresAt"] = int64(expiresMs)
	}
	return res, nil
}

func qoderQuota(m map[string]any, resetAt string) Quota {
	return Quota{
		Used:      toFinite(m["used"], 0),
		Total:     toFinite(m["total"], 0),
		Remaining: toFinite(m["remaining"], 0),
		ResetAt:   resetAt,
	}
}

func init() {
	register("qoder", qoderFetcher{})
}
