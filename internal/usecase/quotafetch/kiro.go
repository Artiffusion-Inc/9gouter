package quotafetch

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// kiro.go ports open-sse/services/usage/kiro.js getKiroUsage. Kiro exposes its
// usage limits through up to three AWS CodeWhisperer / Q endpoints; the handler
// tries codewhisperer-GET, then codewhisperer-POST (x-amz-target), then q-GET,
// returning the first parseable body. authMethod (psd) selects header shape:
// api_key → "tokentype: API_KEY", external_idp → "TokenType: EXTERNAL_IDP",
// default builder-id. profileArn (psd) is forwarded on POST/q-GET when set.
const (
	kiroCWHost   = "https://codewhisperer.us-east-1.amazonaws.com"
	kiroQHost    = "https://q.us-east-1.amazonaws.com"
	kiroLimitsEP = "/getUsageLimits"
)

type kiroFetcher struct{}

func (kiroFetcher) Fetch(ctx context.Context, conn *settings.ProviderConnection, do Doer) (*QuotaResult, error) {
	tok := accessToken(conn)
	if tok == "" {
		return msgResult("Kiro connected. No access token available."), nil
	}
	p := psd(conn)
	authMethod := strField(p, "authMethod")
	if authMethod == "" {
		authMethod = "builder-id"
	}
	profileArn := strField(p, "profileArn")

	// Attempt 1: codewhisperer GET /getUsageLimits?...
	getURL := kiroCWHost + kiroLimitsEP + "?isEmailRequired=true&origin=AI_EDITOR&resourceType=AGENTIC_REQUEST"
	if req, err := newGET(ctx, getURL, func(h http.Header) {
		kiroHeaders(h, tok, authMethod, false)
	}); err == nil {
		if res := kiroTry(ctx, do, req, true); res != nil {
			return res, nil
		}
	}

	// Attempt 2: codewhisperer POST with x-amz-target.
	postBody, _ := json.Marshal(map[string]any{
		"origin":       "AI_EDITOR",
		"resourceType": "AGENTIC_REQUEST",
	})
	if profileArn != "" {
		// rebuild with profileArn
		postBody, _ = json.Marshal(map[string]any{
			"origin":       "AI_EDITOR",
			"profileArn":   profileArn,
			"resourceType": "AGENTIC_REQUEST",
		})
	}
	if req, err := http.NewRequestWithContext(ctx, http.MethodPost, kiroCWHost, bytes.NewReader(postBody)); err == nil {
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		req.Header.Set("x-amz-target", "AmazonCodeWhispererService.GetUsageLimits")
		req.Header.Set("Accept", "application/json")
		kiroAuthHeaders(req.Header, authMethod)
		if res := kiroTry(ctx, do, req, true); res != nil {
			return res, nil
		}
	}

	// Attempt 3: q GET /getUsageLimits?...
	qURL := kiroQHost + kiroLimitsEP + "?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST"
	if profileArn != "" {
		qURL += "&profileArn=" + profileArn
	}
	if req, err := newGET(ctx, qURL, func(h http.Header) {
		kiroHeaders(h, tok, authMethod, false)
	}); err == nil {
		if res := kiroTry(ctx, do, req, true); res != nil {
			return res, nil
		}
	}

	return msgResult("Kiro connected. Unable to fetch usage limits from any endpoint."), nil
}

// kiroHeaders sets the common codewhisperer/q headers + the auth-method token
// header. userAmz=true adds the aws-sdk user-agent used by codewhisperer.
func kiroHeaders(h http.Header, tok, authMethod string, userAmz bool) {
	h.Set("Authorization", "Bearer "+tok)
	h.Set("Accept", "application/json")
	if userAmz {
		h.Set("x-amz-user-agent", "aws-sdk-js/1.0.0 KiroIDE")
		h.Set("User-Agent", "aws-sdk-js/1.0.0 KiroIDE")
	}
	kiroAuthHeaders(h, authMethod)
}

func kiroAuthHeaders(h http.Header, authMethod string) {
	switch authMethod {
	case "api_key":
		h.Set("tokentype", "API_KEY")
	case "external_idp":
		h.Set("TokenType", "EXTERNAL_IDP")
	}
}

// kiroTry runs req via do and, on 200 with a parseable usageBreakdownList,
// returns the parsed QuotaResult; nil otherwise.
func kiroTry(ctx context.Context, do Doer, req *http.Request, _ bool) *QuotaResult {
	body, status, err := doJSON(ctx, do, req)
	if err != nil || status != 200 {
		return nil
	}
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	list, _ := raw["usageBreakdownList"].([]any)
	if len(list) == 0 {
		return nil
	}
	return kiroParse(raw)
}

// kiroParse mirrors parseKiroQuotaData.
func kiroParse(raw map[string]any) *QuotaResult {
	resetAt := parseResetTime(firstNonNil(raw, "nextDateReset", "resetDate"))
	quotas := map[string]Quota{}
	list, _ := raw["usageBreakdownList"].([]any)
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		resource := strField(m, "resourceType")
		if resource == "" {
			resource = "unknown"
		} else {
			resource = lower(resource)
		}
		used := toFinite(firstNonNil(m, "currentUsageWithPrecision"), 0)
		total := toFinite(firstNonNil(m, "usageLimitWithPrecision"), 0)
		remaining := total - used
		if remaining < 0 {
			remaining = 0
		}
		quotas[resource] = Quota{Used: used, Total: total, Remaining: remaining, ResetAt: resetAt}
		if ft, ok := m["freeTrialInfo"].(map[string]any); ok {
			fUsed := toFinite(firstNonNil(ft, "currentUsageWithPrecision"), 0)
			fTotal := toFinite(firstNonNil(ft, "usageLimitWithPrecision"), 0)
			fRemaining := fTotal - fUsed
			if fRemaining < 0 {
				fRemaining = 0
			}
			ftReset := parseResetTime(firstNonNil(ft, "freeTrialExpiry"))
			if ftReset == "" {
				ftReset = resetAt
			}
			quotas[resource+"_freetrial"] = Quota{Used: fUsed, Total: fTotal, Remaining: fRemaining, ResetAt: ftReset}
		}
	}
	plan := "Kiro"
	if sub, ok := raw["subscriptionInfo"].(map[string]any); ok {
		if t := strField(sub, "subscriptionTitle"); t != "" {
			plan = t
		}
	}
	if len(quotas) == 0 {
		return nil
	}
	return &QuotaResult{Plan: plan, Quotas: quotas}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// strconv import kept for future status formatting; silence unused if needed.
var _ = strconv.Itoa

func init() {
	register("kiro", kiroFetcher{})
}
