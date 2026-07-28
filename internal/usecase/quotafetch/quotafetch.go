// Package quotafetch ports the legacy JS per-provider usage fetchers
// (open-sse/services/usage/*.js) into Go. Each provider exposes a live quota /
// balance / usage endpoint; the dashboard "Usage & Analytics" → ProviderLimits
// calls GET /api/usage/{connectionId} which dispatches to the matching
// Fetcher here and renders the returned quotas map.
//
// Before this package, byConnection returned a hard-coded
// "Quota fetch not implemented for this provider in the Go backend yet"
// stub for every eligible connection — the dashboard showed no quota rows.
//
// Design: a Fetcher is a pure transport — it takes the connection's credential
// blob and an injected HTTP doer (Doer) so tests can drive it against a real
// httptest.Server with no mocks. The api package wraps connectionProxyFetch
// into a Doer so the live fetch rides the proxy stack exactly like the JS
// proxyAwareFetch path. QuotaResult is the canonical response shape the JS
// handlers returned (quotas map + optional plan / message / extra scalar
// fields); it serializes to the JSON the dashboard parseQuotaData expects.
package quotafetch

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// Doer runs an *http.Request and returns its response. The api package wires
// this to connectionProxyFetch (proxy-aware); tests wire it to an
// httptest.Server client. The caller owns closing resp.Body.
type Doer func(ctx context.Context, req *http.Request) (*http.Response, error)

// Quota is a single dashboard quota row, mirroring the JS shape the
// QuotaTable / parseQuotaData UI renders. RemainingPercentage is computed by
// the handler when meaningful (used/total or an upstream-provided percent).
// ResetAt is an ISO 8601 timestamp or empty. Recurring marks a refill
// allowance (UI shows "Resets in") vs a one-shot bonus ("Expires in").
type Quota struct {
	Used                float64 `json:"used"`
	Total               float64 `json:"total"`
	Remaining           float64 `json:"remaining,omitempty"`
	RemainingPercentage float64 `json:"remainingPercentage,omitempty"`
	ResetAt             string  `json:"resetAt,omitempty"`
	Unlimited           bool    `json:"unlimited,omitempty"`
	Recurring           bool    `json:"recurring,omitempty"`
}

// QuotaResult is the canonical per-provider response. Quotas is the
// name→row map the UI renders; Plan/Message are optional siblings; Extra
// carries provider-specific scalar metadata (e.g. qoder's totalUsagePercentage)
// serialized as top-level JSON siblings so the dashboard parser does not try to
// render them as rows. A non-empty Message causes the dashboard to hide the
// quota table and show the message instead.
type QuotaResult struct {
	Quotas  map[string]Quota `json:"quotas,omitempty"`
	Plan    string           `json:"plan,omitempty"`
	Message string           `json:"message,omitempty"`
	Extra   map[string]any   `json:"-"`
}

// Fetcher returns the live quota for one provider connection.
type Fetcher interface {
	Fetch(ctx context.Context, conn *settings.ProviderConnection, do Doer) (*QuotaResult, error)
}

// MarshalJSON flattens Extra as top-level siblings alongside the structured
// fields, mirroring the JS handlers' ad-hoc response objects (qoder returns
// {quotas, totalUsagePercentage, isQuotaExceeded, expiresAt} as siblings).
func (r QuotaResult) MarshalJSON() ([]byte, error) {
	base := map[string]any{}
	if len(r.Quotas) > 0 {
		base["quotas"] = r.Quotas
	} else {
		base["quotas"] = map[string]Quota{}
	}
	if r.Plan != "" {
		base["plan"] = r.Plan
	}
	if r.Message != "" {
		base["message"] = r.Message
	}
	for k, v := range r.Extra {
		if _, dup := base[k]; !dup {
			base[k] = v
		}
	}
	return json.Marshal(base)
}

// connData unmarshals a connection's Data blob into a map (nil on error).
func connData(conn *settings.ProviderConnection) map[string]any {
	if conn == nil {
		return nil
	}
	var data map[string]any
	_ = json.Unmarshal(conn.Data, &data)
	return data
}

// accessToken reads data.accessToken.
func accessToken(conn *settings.ProviderConnection) string {
	d := connData(conn)
	if s, ok := d["accessToken"].(string); ok {
		return s
	}
	return ""
}

// apiKey reads data.apiKey.
func apiKey(conn *settings.ProviderConnection) string {
	d := connData(conn)
	if s, ok := d["apiKey"].(string); ok {
		return s
	}
	return ""
}

// psd returns data.providerSpecificData as a map (nil when absent).
func psd(conn *settings.ProviderConnection) map[string]any {
	d := connData(conn)
	if m, ok := d["providerSpecificData"].(map[string]any); ok {
		return m
	}
	return nil
}

// doJSON issues req via do, returns the response body bytes + status + error.
// The caller does NOT need to close the body — this helper drains it.
func doJSON(ctx context.Context, do Doer, req *http.Request) (body []byte, status int, err error) {
	resp, err := do(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b := readAll(resp.Body)
	return b, resp.StatusCode, nil
}

// newGET builds a GET request bound to ctx with optional extra header setters.
func newGET(ctx context.Context, url string, headers func(http.Header)) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		headers(req.Header)
	}
	return req, nil
}

// newJSON builds a POST/PUT request bound to ctx carrying a JSON body with
// optional extra header setters.
func newJSON(ctx context.Context, method, url string, body []byte, headers func(http.Header)) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if headers != nil {
		headers(req.Header)
	}
	return req, nil
}

// readAll reads up to 1 MiB.
func readAll(r interface{ Read(p []byte) (int, error) }) []byte {
	const max = 1 << 20
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for len(buf) < max {
		n, err := r.Read(tmp)
		if n > 0 {
			if len(buf)+n > max {
				n = max - len(buf)
			}
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf
}

// parseResetTime mirrors the JS parseResetTime: accepts a Date-ish value (unix
// seconds when <1e12, milliseconds otherwise, numeric string, or ISO string)
// and returns an RFC3339 string. Empty/invalid → "".
func parseResetTime(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case float64:
		return epochToISO(int64(t))
	case int:
		return epochToISO(int64(t))
	case int64:
		return epochToISO(t)
	case string:
		if t == "" {
			return ""
		}
		// all-digits → epoch
		allDigits := true
		for _, r := range t {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			var n int64
			for _, r := range t {
				n = n*10 + int64(r-'0')
			}
			return epochToISO(n)
		}
		if ts, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return ts.UTC().Format(time.RFC3339)
		}
		// best-effort generic parse
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			return ts.UTC().Format(time.RFC3339)
		}
		return ""
	}
	return ""
}

func epochToISO(epoch int64) string {
	if epoch == 0 {
		return ""
	}
	if epoch < 1e12 {
		epoch = epoch * 1000
	}
	return time.UnixMilli(epoch).UTC().Format(time.RFC3339)
}

// toFinite mirrors the JS toFiniteNumber: numeric (int/float/json.Number) or
// numeric string → float64, else fallback.
func toFinite(v any, fallback float64) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return fallback
		}
		return f
	case string:
		var f float64
		if err := json.Unmarshal([]byte(n), &f); err == nil {
			return f
		}
		return fallback
	}
	return fallback
}

// strField returns the first non-empty string among keys.
func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// clampUnit clamps a percentage to [0,100].
func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// msgResult is the "connected but unable to fetch" payload the JS handlers
// return on transient errors.
func msgResult(msg string) *QuotaResult {
	return &QuotaResult{Message: msg}
}
