package antigravityexec

// retry_test.go pins the 639f1204 Antigravity transient-retry framework.
// The pure helpers (parseRetryHeaders, parseRetryFromErrorMessage,
// extractErrorMessage, isTransientAntigravityError, computeRetryDelay) are
// table-tested directly; the hook wiring into BaseExecutor.Execute is exercised
// E2E against a real httptest.Server whose upstream responses drive the retry
// loop — no mock executor, real BaseExecutor.Execute, real retry timing.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	domain "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// fixedNow is the reference time used by tests that parse HTTP-date headers or
// compute backoff against a stable clock. Tests pass it into computeRetryDelay
// (which accepts now) instead of relying on wall-clock time.
var fixedNow = time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

func TestParseRetryHeaders(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		now    time.Time
		wantMs int
		wantOk bool
	}{
		{"nil header", nil, fixedNow, 0, false},
		{"empty header", http.Header{}, fixedNow, 0, false},
		{"Retry-After seconds", http.Header{"Retry-After": {"5"}}, fixedNow, 5000, true},
		{"Retry-After zero ignored", http.Header{"Retry-After": {"0"}}, fixedNow, 0, false},
		{"Retry-After negative ignored", http.Header{"Retry-After": {"-3"}}, fixedNow, 0, false},
		{
			"Retry-After HTTP-date",
			http.Header{"Retry-After": {fixedNow.Add(30 * time.Second).UTC().Format(http.TimeFormat)}},
			fixedNow, 30000, true,
		},
		{
			"Retry-After HTTP-date in the past ignored",
			http.Header{"Retry-After": {fixedNow.Add(-30 * time.Second).UTC().Format(http.TimeFormat)}},
			fixedNow, 0, false,
		},
		{"x-ratelimit-reset-after", http.Header{"X-Ratelimit-Reset-After": {"7"}}, fixedNow, 7000, true},
		{
			"x-ratelimit-reset (epoch secs in the future)",
			http.Header{"X-Ratelimit-Reset": {strconv.FormatInt(fixedNow.Add(8*time.Second).Unix(), 10)}},
			fixedNow, 8000, true,
		},
		{
			"x-ratelimit-reset in the past ignored",
			http.Header{"X-Ratelimit-Reset": {strconv.FormatInt(fixedNow.Add(-8*time.Second).Unix(), 10)}},
			fixedNow, 0, false,
		},
		{"Retry-After garbage → no header", http.Header{"Retry-After": {"not-a-date"}}, fixedNow, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotMs, gotOk := parseRetryHeaders(c.header, c.now)
			if gotOk != c.wantOk {
				t.Fatalf("ok = %v, want %v", gotOk, c.wantOk)
			}
			if gotMs != c.wantMs {
				t.Errorf("ms = %d, want %d", gotMs, c.wantMs)
			}
		})
	}
}

func TestParseRetryFromErrorMessage(t *testing.T) {
	cases := []struct {
		msg    string
		wantMs int
		wantOk bool
	}{
		{"", 0, false},
		{"no quota text here", 0, false},
		{"Your quota will reset after 2h7m23s", (2*3600 + 7*60 + 23) * 1000, true},
		{"reset after 1h30m", (3600 + 30*60) * 1000, true},
		{"reset after 45m", 45 * 60 * 1000, true},
		{"reset after 30s", 30 * 1000, true},
		{"reset after 1h", 3600 * 1000, true},
		{"reset after 5m30s", (5*60 + 30) * 1000, true},
		{"reset after (empty — no numbers)", 0, false},
	}
	for _, c := range cases {
		t.Run(c.msg, func(t *testing.T) {
			gotMs, gotOk := parseRetryFromErrorMessage(c.msg)
			if gotOk != c.wantOk {
				t.Fatalf("ok = %v, want %v (msg=%q)", gotOk, c.wantOk, c.msg)
			}
			if gotMs != c.wantMs {
				t.Errorf("ms = %d, want %d (msg=%q)", gotMs, c.wantMs, c.msg)
			}
		})
	}
}

func TestExtractErrorMessage(t *testing.T) {
	// Mirrors the JS array [error.message, message, error, bodyText]: every
	// non-empty candidate is joined with "\n". error.message AND the JSON-
	// encoded error object are BOTH present when error is an object carrying a
	// message — that matches upstream (the array has both entries).
	got := extractErrorMessage(map[string]any{"error": map[string]any{"message": "boom"}}, "")
	if !strings.Contains(got, "boom") {
		t.Errorf("error.message path missing boom: %q", got)
	}
	// message field only.
	if got := extractErrorMessage(map[string]any{"message": "only message"}, ""); got != "only message" {
		t.Errorf("message path = %q", got)
	}
	// error as a string: error.message branch skipped (error is not a map),
	// then message (absent), then error ("string error").
	if got := extractErrorMessage(map[string]any{"error": "string error"}, ""); got != "string error" {
		t.Errorf("error string path = %q", got)
	}
	// error as an object (no .message) → JSON-encoded, contains code.
	if got := extractErrorMessage(map[string]any{"error": map[string]any{"code": 42}}, ""); !strings.Contains(got, "42") {
		t.Errorf("error object path = %q", got)
	}
	// bodyText appended last.
	got = extractErrorMessage(map[string]any{"message": "m"}, "raw body")
	if !strings.Contains(got, "m") || !strings.Contains(got, "raw body") {
		t.Errorf("join = %q", got)
	}
	// nil errorJson → bodyText only.
	if got := extractErrorMessage(nil, "just body"); got != "just body" {
		t.Errorf("nil json path = %q", got)
	}
}

func TestIsTransientAntigravityError(t *testing.T) {
	cases := []struct {
		status  int
		message string
		want    bool
	}{
		{429, "", true},
		{500, "", true},
		{502, "", true},
		{503, "", true},
		{504, "", true},
		{400, "", false},
		{401, "", false},
		{200, "", false},
		{400, "high traffic, try again", true},
		{400, "Agent execution terminated due to error", true},
		{400, "agent terminated due to error", true},
		{400, "service at capacity", true},
		{400, "temporarily unavailable", true},
		{400, "request timeout", true},
		{400, "stream ended unexpectedly", true},
		{400, "stream closed", true},
		{400, "stream terminated", true},
		{400, "stream interrupted", true},
		{400, "empty response from upstream", true},
		{400, "totally unrelated message", false},
	}
	for _, c := range cases {
		if got := isTransientAntigravityError(c.status, c.message); got != c.want {
			t.Errorf("isTransient(%d, %q) = %v, want %v", c.status, c.message, got, c.want)
		}
	}
}

func respWith(status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		Status:     http.StatusText(status),
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestComputeRetryDelay(t *testing.T) {
	cases := []struct {
		name      string
		response  *http.Response
		attempt   int
		delayMs   int
		wantMs    int
		wantRetry bool
	}{
		{
			"Retry-After seconds under cap",
			respWith(429, http.Header{"Retry-After": {"3"}}, ""),
			1, 2000, 3000, true,
		},
		{
			"Retry-After over cap vetoes",
			respWith(429, http.Header{"Retry-After": {"30"}}, ""),
			1, 2000, 0, false,
		},
		{
			"reset-after body message under cap",
			respWith(500, nil, `{"error":{"message":"quota will reset after 5s"}}`),
			1, 3000, 5000, true,
		},
		{
			"reset-after body message over cap vetoes",
			respWith(500, nil, `{"error":{"message":"quota will reset after 2h"}}`),
			1, 3000, 0, false,
		},
		{
			"transient 500 no hint → exponential backoff attempt 1",
			respWith(500, nil, `{"error":{"message":"high traffic"}}`),
			1, 3000, 2000, true, // 1000 * 2^1 = 2000
		},
		{
			"transient 500 no hint → exponential backoff attempt 2",
			respWith(500, nil, `{"error":{"message":"high traffic"}}`),
			2, 3000, 4000, true, // 1000 * 2^2 = 4000
		},
		{
			"transient 500 backoff capped at antigravityTransientRetryMaxMs",
			respWith(500, nil, `{"error":{"message":"high traffic"}}`),
			10, 3000, antigravityTransientRetryMaxMs, true,
		},
		{
			"429 no hint → backoff capped at maxRetryAfterMs",
			respWith(429, nil, ""),
			10, 2000, maxRetryAfterMs, true,
		},
		{
			"non-transient 400 vetoes",
			respWith(400, nil, `{"error":{"message":"bad request"}}`),
			1, 2000, 0, false,
		},
		{
			"transient status 503 with no body → backoff",
			respWith(503, nil, ""),
			1, 2000, 2000, true,
		},
		{
			"transient pattern in body even on 400 → backoff",
			respWith(400, nil, "stream terminated"),
			1, 2000, 2000, true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotMs, gotRetry, err := computeRetryDelay(c.response, c.attempt, c.delayMs, fixedNow)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if gotRetry != c.wantRetry {
				t.Fatalf("retry = %v, want %v", gotRetry, c.wantRetry)
			}
			if c.wantRetry && gotMs != c.wantMs {
				t.Errorf("ms = %d, want %d", gotMs, c.wantMs)
			}
		})
	}
}

// TestAntigravityComputeRetryDelayWired verifies New() wires the hook onto
// Config.ComputeRetryDelay so the embedded-method limitation (#142) is
// bypassed and BaseExecutor.Execute honours the antigravity veto.
func TestAntigravityComputeRetryDelayWired(t *testing.T) {
	e := New(base.Config{ID: "antigravity", BaseURL: "https://example.invalid"})
	if e.Config.ComputeRetryDelay == nil {
		t.Fatal("New did not wire Config.ComputeRetryDelay")
	}
	// 429 with a 30s Retry-After must veto (retry=false).
	r := respWith(429, http.Header{"Retry-After": {"30"}}, "")
	_, retry, err := e.Config.ComputeRetryDelay(r, 1, 2000)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if retry {
		t.Error("expected veto for Retry-After over cap")
	}
}

// TestExecute_AntigravityRetryOnTransient500 verifies the full E2E retry loop:
// a first 500 "high traffic" response is retried, then a 200 succeeds. The
// retry delay is real (exponential backoff) but the wired hook is wrapped to
// clamp it to a few ms so the test stays fast while preserving the retry/veto
// decision.
func TestExecute_AntigravityRetryOnTransient500(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"high traffic, try again"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"ok\":true}\n\n"))
	}))
	defer srv.Close()

	e := New(base.Config{
		ID:      "antigravity",
		BaseURL: srv.URL,
		Format:  "antigravity",
		Retry: map[int]base.RetryEntry{
			500: {Attempts: 3, DelayMs: 3000},
		},
	})
	original := e.Config.ComputeRetryDelay
	e.Config.ComputeRetryDelay = func(resp *http.Response, attempt, delayMs int) (int, bool, error) {
		ms, retry, err := original(resp, attempt, delayMs)
		if retry && ms > 5 {
			ms = 5
		}
		return ms, retry, err
	}
	e.Fetch = mockFetchTo(srv)

	out, err := e.Execute(context.Background(), agExecReq())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	defer out.Response.Body.Close()
	if hits != 2 {
		t.Errorf("upstream hits = %d, want 2 (retry then success)", hits)
	}
}

// TestExecute_AntigravityVetoFallsThrough verifies a Retry-After hint over the
// cap vetoes the retry and, with no fallback URL, surfaces the upstream 429
// (the request is NOT retried).
func TestExecute_AntigravityVetoFallsThrough(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	e := New(base.Config{
		ID:      "antigravity",
		BaseURL: srv.URL,
		Format:  "antigravity",
		Retry: map[int]base.RetryEntry{
			429: {Attempts: 6, DelayMs: 2000},
		},
	})
	e.Fetch = mockFetchTo(srv)

	out, err := e.Execute(context.Background(), agExecReq())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	defer out.Response.Body.Close()
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1 (veto must not retry)", hits)
	}
	if out.Response.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 (veto surfaces the upstream response)", out.Response.StatusCode)
	}
}

// mockFetchTo redirects the BaseExecutor's upstream request to the test server.
func mockFetchTo(srv *httptest.Server) base.Fetcher {
	host := strings.TrimPrefix(srv.URL, "http://")
	return func(ctx context.Context, client *http.Client, req *http.Request, opts proxy.Options, pfo proxy.ProxyFetchOptions, fb *proxy.Fallback) (*http.Response, error) {
		req.URL.Scheme = "http"
		req.URL.Host = host
		req.Host = host
		return client.Do(req)
	}
}

func agExecReq() domain.ExecRequest {
	return domain.ExecRequest{
		Model:  "gemini-2.5-pro",
		Body:   json.RawMessage(`{}`),
		Stream: true,
		Credentials: domain.Credentials{
			APIKey: "test-key",
		},
	}
}
