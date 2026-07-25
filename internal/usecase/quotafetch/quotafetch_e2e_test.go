package quotafetch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// E2E tests drive each fetcher against a real httptest.Server (no mocks). The
// Doer wires the server's client; the fetcher issues its real request and
// parses the canned upstream body the legacy JS handler expected.

// makeDoer returns a Doer that rewrites req.Host/URL to the test server and
// runs it through the server's client, preserving the original path + headers
// (which the test handler asserts on).
func makeDoer(server *httptest.Server) Doer {
	return func(ctx context.Context, req *http.Request) (*http.Response, error) {
		req.URL.Scheme = "http"
		req.URL.Host = server.Listener.Addr().String()
		req.Host = server.Listener.Addr().String()
		return server.Client().Do(req)
	}
}

func connWith(data map[string]any) *settings.ProviderConnection {
	b, _ := json.Marshal(data)
	return &settings.ProviderConnection{Provider: "", Data: b}
}

func connFor(provider string, data map[string]any) *settings.ProviderConnection {
	c := connWith(data)
	c.Provider = provider
	return c
}

func readReqJSON(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) == 0 {
		return nil
	}
	var v map[string]any
	_ = json.Unmarshal(body, &v)
	return v
}

func TestGLM_FetchParsesLimits(t *testing.T) {
	body := `{"data":{"level":"pro","limits":[{"type":"TOKENS_LIMIT","percentage":42,"nextResetTime":1753990400000}]}}`
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f := Lookup("glm")
	if f == nil {
		t.Fatal("glm fetcher not registered")
	}
	res, err := f.Fetch(context.Background(), connFor("glm", map[string]any{"apiKey": "k123"}), makeDoer(srv))
	if err != nil || res == nil {
		t.Fatalf("fetch: %v res=%v", err, res)
	}
	if gotAuth != "Bearer k123" {
		t.Fatalf("auth header = %q want Bearer k123", gotAuth)
	}
	if res.Plan != "Pro" {
		t.Fatalf("plan = %q want Pro", res.Plan)
	}
	q, ok := res.Quotas["session"]
	if !ok {
		t.Fatalf("no session quota: %#v", res.Quotas)
	}
	if q.Used != 42 {
		t.Fatalf("used = %v want 42", q.Used)
	}
	if q.Total != 100 {
		t.Fatalf("total = %v want 100", q.Total)
	}
}

func TestVercel_FetchParsesCredits(t *testing.T) {
	body := `{"balance":"3.25","total_used":"1.75"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer vkey" {
			t.Errorf("auth = %q want Bearer vkey", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	res, err := Lookup("vercel-ai-gateway").Fetch(context.Background(), connFor("vercel-ai-gateway", map[string]any{"apiKey": "vkey"}), makeDoer(srv))
	if err != nil || res == nil {
		t.Fatalf("fetch: %v", err)
	}
	used := res.Quotas["Used (USD)"]
	if used.Used != 1.75 {
		t.Fatalf("Used(USD).used = %v want 1.75", used.Used)
	}
	rem := res.Quotas["Remaining (USD)"]
	if rem.Used != 3.25 || rem.Total != 5 {
		t.Fatalf("Remaining(USD) = %#v want used=3.25 total=5", rem)
	}
}

func TestQoder_FetchParsesQuota(t *testing.T) {
	body := `{"userQuota":{"total":1000,"used":250,"remaining":750,"unit":"requests"},"totalUsagePercentage":25,"isQuotaExceeded":false,"expiresAt":1756608000000}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer qtok" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	res, err := Lookup("qoder").Fetch(context.Background(), connFor("qoder", map[string]any{"accessToken": "qtok"}), makeDoer(srv))
	if err != nil || res == nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.Quotas["user"].Used != 250 || res.Quotas["user"].Total != 1000 {
		t.Fatalf("user quota = %#v", res.Quotas["user"])
	}
	if res.Extra["totalUsagePercentage"].(float64) != 25 {
		t.Fatalf("totalUsagePercentage = %v want 25", res.Extra["totalUsagePercentage"])
	}
	// MarshalJSON must flatten Extra as siblings.
	out, _ := json.Marshal(res)
	if !strings.Contains(string(out), "\"totalUsagePercentage\":25") {
		t.Fatalf("marshaled missing flattened extra: %s", out)
	}
}

func TestClaude_FetchParsesOAuthWindows(t *testing.T) {
	body := `{"five_hour":{"utilization":60,"resets_at":"2026-07-24T20:00:00Z"},"seven_day":{"utilization":30,"resets_at":"2026-07-31T00:00:00Z"}}`
	var gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	res, err := Lookup("claude").Fetch(context.Background(), connFor("claude", map[string]any{"accessToken": "at"}), makeDoer(srv))
	if err != nil || res == nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotBeta != claudeOAuthBeta {
		t.Fatalf("beta header = %q want %q", gotBeta, claudeOAuthBeta)
	}
	if res.Plan != "Claude Code" {
		t.Fatalf("plan = %q", res.Plan)
	}
	sess := res.Quotas["session (5h)"]
	if sess.Used != 60 || sess.Total != 100 || sess.Remaining != 40 {
		t.Fatalf("session(5h) = %#v want used=60 total=100 rem=40", sess)
	}
	wk := res.Quotas["weekly (7d)"]
	if wk.Used != 30 || wk.Remaining != 70 {
		t.Fatalf("weekly(7d) = %#v", wk)
	}
}

func TestKiro_FetchTriesGetThenPost(t *testing.T) {
	// GET returns empty list → must fall through to POST which returns data.
	getCount := 0
	postCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getCount++
			_, _ = w.Write([]byte(`{"usageBreakdownList":[]}`))
		case http.MethodPost:
			postCount++
			if r.Header.Get("x-amz-target") != "AmazonCodeWhispererService.GetUsageLimits" {
				t.Errorf("post x-amz-target = %q", r.Header.Get("x-amz-target"))
			}
			b := readReqJSON(t, r)
			if b["resourceType"] != "AGENTIC_REQUEST" {
				t.Errorf("post body = %#v", b)
			}
			_, _ = w.Write([]byte(`{"usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","currentUsageWithPrecision":3,"usageLimitWithPrecision":10}],"subscriptionInfo":{"subscriptionTitle":"Kiro Pro"},"nextDateReset":"2026-08-01T00:00:00Z"}`))
		}
	}))
	defer srv.Close()

	res, err := Lookup("kiro").Fetch(context.Background(), connFor("kiro", map[string]any{"accessToken": "kt"}), makeDoer(srv))
	if err != nil || res == nil {
		t.Fatalf("fetch: %v", err)
	}
	if getCount == 0 || postCount == 0 {
		t.Fatalf("expected GET then POST; get=%d post=%d", getCount, postCount)
	}
	if res.Plan != "Kiro Pro" {
		t.Fatalf("plan = %q want Kiro Pro", res.Plan)
	}
	q := res.Quotas["agentic_request"]
	if q.Used != 3 || q.Total != 10 || q.Remaining != 7 {
		t.Fatalf("quota = %#v want used=3 total=10 rem=7", q)
	}
}

func TestMiniMax_FetchParses5h7dRows(t *testing.T) {
	body := `{"model_remains":[{"model":"abab6.5","current_interval_total_count":100,"current_interval_usage_count":20,"current_weekly_total_count":1000,"current_weekly_usage_count":200,"remains_time":3600000}]}`
	// pin nowMs so resetAt is deterministic
	nowMs = func() int64 { return 1753320000000 }
	defer func() { nowMs = func() int64 { return timeNowUnixMilli() } }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mk" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	res, err := Lookup("minimax").Fetch(context.Background(), connFor("minimax", map[string]any{"apiKey": "mk"}), makeDoer(srv))
	if err != nil || res == nil {
		t.Fatalf("fetch: %v", err)
	}
	sess := res.Quotas["abab6.5 (5h)"]
	if sess.Used != 20 || sess.Total != 100 {
		t.Fatalf("5h = %#v want used=20 total=100", sess)
	}
	wk := res.Quotas["abab6.5 (7d)"]
	if wk.Used != 200 || wk.Total != 1000 {
		t.Fatalf("7d = %#v want used=200 total=1000", wk)
	}
	if sess.ResetAt == "" {
		t.Fatalf("5h resetAt empty")
	}
}

func TestCodeBuddyCn_FetchParsesRefillAndBonus(t *testing.T) {
	// CycleEndTime 2026-08-01 ≈ 1785542400000 ms; DeductionEndTime 10d later
	// → DeductionEndTime - CycleEndTime > 2d gap → refill (recurring).
	body := `{"data":{"Response":{"Data":{"Accounts":[
		{"CycleStartTime":"2026-07-01T00:00:00Z","CycleEndTime":"2026-08-01T00:00:00Z","DeductionEndTime":1786406400000,"CycleCapacityUsedPrecise":50,"CycleCapacitySizePrecise":200,"PackageName":"Monthly Pro"},
		{"CycleEndTime":"2026-07-30T00:00:00Z","CapacityUsedPrecise":5,"CapacitySizePrecise":30}
	]}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q want POST", r.Method)
		}
		// body is "{}" — an empty JSON object; we just assert the request was
		// issued (the exact body shape is the provider contract, tested via the
		// response parse).
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	res, err := Lookup("codebuddy-cn").Fetch(context.Background(), connFor("codebuddy-cn", map[string]any{"accessToken": "cb"}), makeDoer(srv))
	if err != nil || res == nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.Plan != "Monthly Pro" {
		t.Fatalf("plan = %q want Monthly Pro", res.Plan)
	}
	monthly := res.Quotas["Monthly"]
	if monthly.Used != 50 || monthly.Total != 200 || !monthly.Recurring {
		t.Fatalf("Monthly = %#v want used=50 total=200 recurring", monthly)
	}
	bonus := res.Quotas["Bonus Pack 1"]
	if bonus.Used != 5 || bonus.Total != 30 || bonus.Recurring {
		t.Fatalf("Bonus = %#v want used=5 total=30 non-recurring", bonus)
	}
}

func TestGrokCli_FetchParsesBillingRows(t *testing.T) {
	billing := `{"config":{"monthlyLimit":{"val":1000},"includedUsed":{"val":300},"onDemandCap":{"val":500},"onDemandUsed":{"val":50},"billingPeriodEnd":"2026-08-01T00:00:00Z","isUnifiedBillingUser":true}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-xai-token-auth") != "xai-grok-cli" {
			t.Errorf("x-xai-token-auth = %q", r.Header.Get("x-xai-token-auth"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(billing))
	}))
	defer srv.Close()

	res, err := Lookup("grok-cli").Fetch(context.Background(), connFor("grok-cli", map[string]any{"accessToken": "gt"}), makeDoer(srv))
	if err != nil || res == nil {
		t.Fatalf("fetch: %v", err)
	}
	mi := res.Quotas["Monthly included"]
	if mi.Used != 300 || mi.Total != 1000 {
		t.Fatalf("Monthly included = %#v want used=300 total=1000", mi)
	}
	od := res.Quotas["On-demand"]
	if od.Used != 50 || od.Total != 500 {
		t.Fatalf("On-demand = %#v want used=50 total=500", od)
	}
	if res.Plan != "Grok Build" {
		t.Fatalf("plan = %q want Grok Build", res.Plan)
	}
}

func TestGitHub_FetchParsesSnapshots(t *testing.T) {
	body := `{"copilot_plan":"Pro","quota_snapshots":{"chat":{"entitlement":100,"remaining":80,"unlimited":false},"completions":{"entitlement":500,"remaining":250}},"quota_reset_date":"2026-08-01T00:00:00Z"}`
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	res, err := Lookup("github").Fetch(context.Background(), connFor("github", map[string]any{"accessToken": "ghtok"}), makeDoer(srv))
	if err != nil || res == nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotAuth != "token ghtok" {
		t.Fatalf("auth = %q want 'token ghtok'", gotAuth)
	}
	if res.Plan != "Pro" {
		t.Fatalf("plan = %q want Pro", res.Plan)
	}
	chat := res.Quotas["chat"]
	if chat.Used != 20 || chat.Total != 100 || chat.Remaining != 80 {
		t.Fatalf("chat = %#v want used=20 total=100 rem=80", chat)
	}
}

func TestCodex_FetchParsesWindows(t *testing.T) {
	body := `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":40,"reset_at":"2026-07-24T20:00:00Z"},"secondary_window":{"used_percent":15,"reset_at":"2026-07-31T00:00:00Z"}},"rate_limit_reset_credits":{"available_count":3}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ct" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	res, err := Lookup("codex").Fetch(context.Background(), connFor("codex", map[string]any{"accessToken": "ct"}), makeDoer(srv))
	if err != nil || res == nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.Plan != "pro" {
		t.Fatalf("plan = %q want pro", res.Plan)
	}
	sess := res.Quotas["session"]
	if sess.Used != 40 || sess.Total != 100 || sess.Remaining != 60 {
		t.Fatalf("session = %#v want used=40 total=100 rem=60", sess)
	}
	wk := res.Quotas["weekly"]
	if wk.Used != 15 {
		t.Fatalf("weekly = %#v want used=15", wk)
	}
	if res.Extra["resetCredits"].(map[string]any)["availableCount"].(float64) != 3 {
		t.Fatalf("resetCredits = %#v", res.Extra["resetCredits"])
	}
}

// timeNowUnixMilli returns the real current ms (restore target for tests that pin nowMs).
func timeNowUnixMilli() int64 { return time.Now().UnixMilli() }
