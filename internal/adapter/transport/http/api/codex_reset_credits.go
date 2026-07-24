package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// codex_reset_credits.go ports 5cc4f222 (codex #2290): both the read-only GET
// (reset-credits inventory) and the irreversible POST (consume one credit).
// Mirrors open-sse/services/usage/codex.js getCodexRateLimitResetCredits +
// consumeCodexRateLimitResetCredit and the JS route's response shaping
// (getResponseForConsumeResult). Both upstream calls route through the proxy
// stack via connectionProxyFetch (proxy-awareness was the deferred half of
// #154); the route handler (usage_extra.go) drives them.

// codexResetCreditsURL is the GET endpoint (inventory). A var so tests can
// point it at an httptest.Server.
var codexResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

// codexResetCreditsConsumeURL is the POST endpoint (spend 1 credit,
// irreversible). A var for the same reason.
var codexResetCreditsConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"

// fetchCodexResetCredits fetches the live reset-credits inventory for a codex
// OAuth connection through the proxy stack. Returns the dashboard-shaped
// payload (credits + available count) or a user-facing message payload.
func fetchCodexResetCredits(ctx context.Context, pools *repo.ProxyPoolRepo, proxyOpts proxy.Options, conn *settings.ProviderConnection) (map[string]any, error) {
	token, accountID, ok := codexConnectionCreds(conn)
	if !ok || token == "" {
		return map[string]any{
			"credits":      []any{},
			"message":      "No Codex access token available. Please re-authorize the connection.",
			"connectionId": conn.ID,
		}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexResetCreditsURL, nil)
	if err != nil {
		return nil, err
	}
	codexFingerprintHeaders(req, token, accountID)

	resp, err := connectionProxyFetch(ctx, pools, proxyOpts, conn, req)
	if err != nil {
		return map[string]any{
			"credits":      []any{},
			"connectionId": conn.ID,
			"message":      "Codex reset credits API request failed: " + err.Error(),
		}, nil
	}
	defer resp.Body.Close()
	body := readBodySafe(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return map[string]any{
			"credits":      []any{},
			"connectionId": conn.ID,
			"message":      codexUpstreamErrorMessage(body, resp.StatusCode),
		}, nil
	}

	return parseCodexResetCredits(body, conn.ID), nil
}

// consumeCodexResetCredits spends one reset credit via POST. redeemRequestID
// is a server-generated UUID (the JS route uses crypto.randomUUID) so a
// client cannot control the redeem id for replay. Returns the dashboard
// consume-response shape + the HTTP status the JS route's
// getResponseForConsumeResult maps to:
//   - ok (code=="reset" or windows_reset>0): 200 {code, reset:true, windows_reset, redeemRequestId, credit}
//   - no_credit (ok && code=="no_credit"): 409 {code:"no_credit", reset:false, windows_reset, message}
//   - else: 502 (or the upstream 4xx) {code||"unknown_response", reset:false, windows_reset, message}
func consumeCodexResetCredits(ctx context.Context, pools *repo.ProxyPoolRepo, proxyOpts proxy.Options, conn *settings.ProviderConnection, redeemRequestID string) (map[string]any, int) {
	token, accountID, ok := codexConnectionCreds(conn)
	if !ok || token == "" {
		return map[string]any{
			"code":          "no_token",
			"reset":         false,
			"windows_reset": 0,
			"message":       "No Codex access token available. Please re-authorize the connection.",
			"connectionId":  conn.ID,
		}, http.StatusUnauthorized
	}

	body, _ := json.Marshal(map[string]any{"redeem_request_id": redeemRequestID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexResetCreditsConsumeURL, strings.NewReader(string(body)))
	if err != nil {
		return map[string]any{
			"code":          "request_error",
			"reset":         false,
			"windows_reset": 0,
			"message":       "Failed to build Codex consume request: " + err.Error(),
			"connectionId":  conn.ID,
		}, http.StatusInternalServerError
	}
	codexFingerprintHeaders(req, token, accountID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := connectionProxyFetch(ctx, pools, proxyOpts, conn, req)
	if err != nil {
		return map[string]any{
			"code":          "request_error",
			"reset":         false,
			"windows_reset": 0,
			"message":       "Codex consume API request failed: " + err.Error(),
			"connectionId":  conn.ID,
		}, http.StatusBadGateway
	}
	defer resp.Body.Close()
	respBody := readBodySafe(resp.Body)

	var data map[string]any
	_ = json.Unmarshal(respBody, &data)

	code, _ := data["code"].(string)
	windowsReset := toFiniteInt(data["windows_reset"], data["windowsReset"])
	success := resp.StatusCode == http.StatusOK && (code == "reset" || windowsReset > 0)
	noCredit := resp.StatusCode == http.StatusOK && code == "no_credit"

	if success {
		credit, _ := data["credit"].(map[string]any)
		return map[string]any{
			"code":            code,
			"reset":           true,
			"windows_reset":   windowsReset,
			"redeemRequestId": redeemRequestID,
			"credit":          credit,
			"connectionId":    conn.ID,
		}, http.StatusOK
	}
	if noCredit {
		return map[string]any{
			"code":          "no_credit",
			"reset":         false,
			"windows_reset": windowsReset,
			"message":       "No Codex reset credits available.",
			"connectionId":  conn.ID,
		}, http.StatusConflict
	}
	// Unexpected response: mirror getResponseForConsumeResult — 4xx upstream
	// passes through, else 502.
	status := http.StatusBadGateway
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		status = resp.StatusCode
	}
	msg, _ := data["message"].(string)
	if msg == "" {
		msg = "Codex reset credit consume returned an unexpected response."
	}
	if code == "" {
		code = "unknown_response"
	}
	return map[string]any{
		"code":          code,
		"reset":         false,
		"windows_reset": windowsReset,
		"message":       msg,
		"connectionId":  conn.ID,
	}, status
}

// codexFingerprintHeaders sets the codex fingerprint headers both the GET and
// POST consume calls share (Authorization Bearer, Accept JSON, OpenAI-Beta
// codex-1, originator codex_cli_rs, ChatGPT-Account-ID from the connection).
func codexFingerprintHeaders(req *http.Request, token, accountID string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OpenAI-Beta", "codex-1")
	req.Header.Set("originator", "codex_cli_rs")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
}

// codexNewRedeemRequestID returns a fresh server-generated redeem request id
// (the JS route uses crypto.randomUUID). A server-generated id prevents a
// client from controlling the redeem id for replay.
func codexNewRedeemRequestID() string {
	return uuid.NewString()
}

// parseCodexResetCredits maps the upstream JSON into the dashboard shape:
//   - availableCount ← data.available_count | data.availableCount (>=0)
//   - credits[]      ← data.credits[] → {status, grantedAt, expiresAt}
//
// Mirrors getCodexRateLimitResetCredits in open-sse/services/usage/codex.js.
func parseCodexResetCredits(body []byte, connectionID string) map[string]any {
	var raw struct {
		AvailableCount  any              `json:"available_count"`
		AvailableCount2 any              `json:"availableCount"`
		Credits         []map[string]any `json:"credits"`
	}
	_ = json.Unmarshal(body, &raw)

	out := map[string]any{
		"connectionId": connectionID,
	}
	out["availableCount"] = toFiniteInt(raw.AvailableCount, raw.AvailableCount2)

	credits := []any{}
	for _, c := range raw.Credits {
		status := "unknown"
		if s, ok := stringField(c, "status", "Status"); ok && s != "" {
			status = s
		}
		credits = append(credits, map[string]any{
			"status":    status,
			"grantedAt": codexISODate(firstStringField(c, "granted_at", "grantedAt")),
			"expiresAt": codexISODate(firstStringField(c, "expires_at", "expiresAt")),
		})
	}
	out["credits"] = credits
	return out
}

// codexConnectionCreds extracts the OAuth access token + account id from a
// connection's Data blob (the same fields v1.go's credential resolution reads:
// data.accessToken + data.providerSpecificData.{workspaceId,chatgptAccountId,
// accountId}). Returns ok=false when the connection is not a codex OAuth row.
func codexConnectionCreds(conn *settings.ProviderConnection) (token, accountID string, ok bool) {
	if conn == nil || conn.Provider != "codex" {
		return "", "", false
	}
	var data map[string]any
	if err := json.Unmarshal(conn.Data, &data); err != nil {
		return "", "", false
	}
	if v, ok := data["accessToken"].(string); ok {
		token = v
	}
	if psd, ok := data["providerSpecificData"].(map[string]any); ok {
		for _, k := range []string{"workspaceId", "chatgptAccountId", "accountId"} {
			if v, ok := psd[k].(string); ok && v != "" {
				accountID = v
				break
			}
		}
	}
	return token, accountID, true
}

// codexUpstreamErrorMessage extracts a user-facing message from a non-200
// upstream body, falling back to a status-based message.
func codexUpstreamErrorMessage(body []byte, status int) string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		for _, k := range []string{"message", "error", "detail"} {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
	}
	return "Codex reset credits API unavailable (" + itoa(status) + ")."
}

// codexISODate normalizes a granted/expires timestamp to an ISO 8601 string.
// Accepts an existing ISO string (passed through) or a numeric epoch (seconds
// when < 1e12, milliseconds otherwise), mirroring the JS toIsoDate helper.
func codexISODate(v string) any {
	if v == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	// Numeric epoch string?
	if n := parseEpochString(v); n != 0 {
		return time.UnixMilli(n).UTC().Format(time.RFC3339)
	}
	return v
}

func firstStringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func stringField(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			return v, true
		}
	}
	return "", false
}

func toFiniteInt(vals ...any) int {
	for _, v := range vals {
		switch n := v.(type) {
		case float64:
			if n < 0 {
				return 0
			}
			return int(n)
		case int:
			if n < 0 {
				return 0
			}
			return n
		case int64:
			if n < 0 {
				return 0
			}
			return int(n)
		}
	}
	return 0
}

// parseEpochString parses a numeric string into a millisecond epoch.
// Values < 1e12 are treated as seconds (×1000); larger as milliseconds.
// Returns 0 on any parse failure.
func parseEpochString(s string) int64 {
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
		if n > 1<<62 {
			return 0
		}
	}
	if n == 0 {
		return 0
	}
	if n < 1e12 {
		return n * 1000
	}
	return n
}

// itoa is a tiny strconv.Itoa stand-in to keep imports minimal here.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

var _ = strings.TrimSpace
