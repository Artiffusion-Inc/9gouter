package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// codex_reset_credits.go ports the GET half of 5cc4f222 (codex #2290):
// a read-only upstream fetch of the Codex per-credit rate-limit reset
// inventory so the dashboard Quota Tracker can render status / granted /
// expiry / remaining. Mirrors open-sse/services/usage/codex.js
// getCodexRateLimitResetCredits: GET {resetCreditsUrl} with the codex
// fingerprint headers (Authorization Bearer, OpenAI-Beta codex-1, originator
// codex_cli_rs, ChatGPT-Account-ID from the connection's account id), then
// map data.credits[] → {status, grantedAt, expiresAt} and data.available_count.
//
// The consume POST (spend 1 credit) stays a stub — it is irreversible and out
// of scope for the read-only #2290 view.
//
// Proxy-awareness is deferred: the JS handler routes through proxyAwareFetch
// using the connection's proxy options. The dashboard Deps does not expose a
// proxy-aware HTTP client to usage handlers today, so this uses a plain
// timeout client. When a proxy-aware fetcher is wired into Deps, swap the
// client here.

// codexResetCreditsURL is the upstream endpoint. It is a var (not a const) so
// tests can point it at an httptest.Server.
var codexResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

// codexResetCreditsClient is the upstream HTTP client. Overridable in tests.
var codexResetCreditsClient = &http.Client{Timeout: 30 * time.Second}

// fetchCodexResetCredits fetches the live reset-credits inventory for a codex
// OAuth connection. Returns the dashboard-shaped payload (credits + available
// count) or an error carrying a user-facing message.
func fetchCodexResetCredits(ctx context.Context, conn *settings.ProviderConnection) (map[string]any, error) {
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
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OpenAI-Beta", "codex-1")
	req.Header.Set("originator", "codex_cli_rs")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}

	resp, err := codexResetCreditsClient.Do(req)
	if err != nil {
		return map[string]any{
			"credits":      []any{},
			"connectionId": conn.ID,
			"message":      "Codex reset credits API request failed: " + err.Error(),
		}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		return map[string]any{
			"credits":      []any{},
			"connectionId": conn.ID,
			"message":      codexUpstreamErrorMessage(body, resp.StatusCode),
		}, nil
	}

	return parseCodexResetCredits(body, conn.ID), nil
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
