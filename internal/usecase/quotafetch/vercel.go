package quotafetch

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// vercel.go ports open-sse/services/usage/misc.js getVercelAiGatewayUsage.
// GET https://ai-gateway.vercel.sh/v1/credits with Bearer apiKey →
// {balance:"95.50", total_used:"4.50"} (USD as decimal strings).
// Surfaced as two rows: "Used (USD)" (unlimited, no cap) and "Remaining (USD)"
// (out of the known $5 monthly free credit).
const vercelUsageURL = "https://ai-gateway.vercel.sh/v1/credits"

const vercelMonthlyCredit = 5.0

type vercelFetcher struct{}

func (vercelFetcher) Fetch(ctx context.Context, conn *settings.ProviderConnection, do Doer) (*QuotaResult, error) {
	key := apiKey(conn)
	if key == "" {
		return msgResult("Vercel AI Gateway API key not available."), nil
	}
	req, err := newGET(ctx, vercelUsageURL, func(h http.Header) {
		h.Set("Authorization", "Bearer "+key)
		h.Set("Accept", "application/json")
	})
	if err != nil {
		return nil, err
	}
	body, status, err := doJSON(ctx, do, req)
	if err != nil {
		return msgResult("Vercel AI Gateway error: " + err.Error()), nil
	}
	if status == 401 || status == 403 {
		return msgResult("Vercel AI Gateway API key invalid or expired."), nil
	}
	if status != 200 {
		return msgResult("Vercel AI Gateway credits API error (" + strconv.Itoa(status) + ")."), nil
	}
	var raw struct {
		Balance   any `json:"balance"`
		TotalUsed any `json:"total_used"`
	}
	_ = json.Unmarshal(body, &raw)
	balance := toFinite(raw.Balance, 0)
	totalUsed := toFinite(raw.TotalUsed, 0)
	if balance <= 0 && totalUsed <= 0 {
		return &QuotaResult{
			Plan:    "Pay-as-you-go",
			Message: "Vercel AI Gateway connected. No credit allocation found (BYOK or unfunded account).",
		}, nil
	}
	pct := 0.0
	if vercelMonthlyCredit > 0 {
		pct = clampUnit((balance / vercelMonthlyCredit) * 100)
	}
	return &QuotaResult{
		Plan: "Pay-as-you-go",
		Quotas: map[string]Quota{
			"Used (USD)": {
				Used:                totalUsed,
				Total:               0,
				RemainingPercentage: 100,
				Unlimited:           true,
			},
			"Remaining (USD)": {
				Used:                balance,
				Total:               vercelMonthlyCredit,
				Remaining:           balance,
				RemainingPercentage: pct,
			},
		},
	}, nil
}

func init() {
	register("vercel-ai-gateway", vercelFetcher{})
}
