package api

// codex_account_info.go ports the JS extractCodexAccountInfo (open-sse/
// src/lib/oauth/providerHelpers.js) used by the codex bulk-import route
// (src/app/api/oauth/codex/bulk-import/route.js): a codex OAuth import may
// arrive with only accessToken + idToken, so the server backfills the
// identity fields (email, chatgptAccountId, chatgptPlanType) from the JWT
// claims before persisting. Without this backfill the c73c419d dedup rule
// (codex → match only if BOTH rows expose an equal chatgptAccountId) never
// fires on the bulk-import path — a re-import of the same account would
// create a duplicate row instead of merging.
//
// The JWT signature is NOT verified, mirroring decodeIDTokenClaims /
// DecodeJWTPayload: the dashboard already trusts the token from the OAuth
// handshake; we only read the claims for identity + dedup.

import (
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/resolver/tokenrefresh"
)

// codexAccountInfo is the identity backfilled from a codex JWT. Zero values
// mean "not present in the token" (the caller leaves the incoming field as-is).
type codexAccountInfo struct {
	Email            string
	ChatGPTAccountID string
	ChatGPTPlanType  string
}

// extractCodexAccountInfo decodes the codex JWT (idToken preferred, else the
// accessToken) and reads the identity claims the bulk-import backfills. Mirrors
// JS extractCodexAccountInfo: chatgptAccountId ← namespace
// chatgpt_account_id | chatgpt_id | chatgptAccountId, fallback top-level
// account_id; chatgptPlanType ← namespace chatgpt_plan_type | top-level
// plan_type; email ← email | preferred_username | sub. Returns the zero value
// when the JWT is malformed or carries no claims.
func extractCodexAccountInfo(jwt string) codexAccountInfo {
	jwt = strings.TrimSpace(jwt)
	if jwt == "" {
		return codexAccountInfo{}
	}
	claims := tokenrefresh.DecodeJWTPayload(jwt)
	if claims == nil {
		return codexAccountInfo{}
	}
	info := codexAccountInfo{}
	for _, k := range []string{"email", "preferred_username", "sub"} {
		if v, ok := claims[k].(string); ok && v != "" {
			info.Email = v
			break
		}
	}
	ns, _ := claims["https://api.openai.com/auth"].(map[string]any)
	// chatgptAccountId: namespace first, then top-level account_id (JS parity).
	for _, k := range []string{"chatgpt_account_id", "chatgpt_id", "chatgptAccountId"} {
		if v, ok := ns[k].(string); ok && v != "" {
			info.ChatGPTAccountID = v
			break
		}
	}
	if info.ChatGPTAccountID == "" {
		if v, ok := claims["account_id"].(string); ok && v != "" {
			info.ChatGPTAccountID = v
		}
	}
	// chatgptPlanType: namespace first, then top-level plan_type.
	for _, k := range []string{"chatgpt_plan_type", "chatgptPlanType"} {
		if v, ok := ns[k].(string); ok && v != "" {
			info.ChatGPTPlanType = v
			break
		}
	}
	if info.ChatGPTPlanType == "" {
		if v, ok := claims["plan_type"].(string); ok && v != "" {
			info.ChatGPTPlanType = v
		}
	}
	return info
}

// codexExpiresAtFromExpiresIn computes an ISO-8601 expiry from an expires_in
// seconds value (the JS route does `new Date(Date.now()+expiresIn*1000).toISOString()`).
// Returns "" when expiresIn <= 0.
func codexExpiresAtFromExpiresIn(expiresIn float64, now time.Time) string {
	if expiresIn <= 0 {
		return ""
	}
	return now.Add(time.Duration(expiresIn*1000) * time.Millisecond).UTC().Format(time.RFC3339)
}
