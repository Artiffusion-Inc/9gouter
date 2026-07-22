package api

// usage_stats_contract_test.go is the HTTP-level regression test for #81:
// the /api/usage/stats endpoint must emit the legacy getUsageStats JSON shape
// (camelCase keys, byModel "model (provider)" buckets with rawModel/provider/
// requests/.../avgTps/lastUsed, recentRequests, last10Minutes, pending) that
// the dashboard UsageStats.js renders. The prior handler returned the
// tag-less domain usage.Aggregates struct (PascalCase) and the dashboard
// crashed on undefined. Driven through the real mux + real SQLite — no mocks.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapterauth "github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/usage"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/managedashboard"
)

// TestUsageStats_HTTPContract drives the full /api/usage/stats handler with a
// real seeded SQLite and asserts the JSON the dashboard reads.
func TestUsageStats_HTTPContract(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	deps.UsageTracker = managedashboard.NewEventTracker()
	mux := http.NewServeMux()
	RegisterUsage(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	if err := deps.Usage.Save(context.Background(), usage.UsageRecord{
		Provider: "openai", Model: "gpt-4",
		PromptTokens: 50, CompletionTokens: 20, Cost: 0.05, Status: "success",
	}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/usage/stats?period=7d", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var stats map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("unmarshal stats: %v; body=%s", err, rec.Body.String())
	}

	// Top-level camelCase totals (the bug: domain struct emitted PascalCase).
	for _, k := range []string{"totalRequests", "totalPromptTokens", "totalCompletionTokens", "totalCost", "totalCachedTokens", "byModel", "byAccount", "byApiKey", "byEndpoint", "byProvider", "recentRequests", "last10Minutes", "pending", "activeRequests", "errorProvider"} {
		if _, ok := stats[k]; !ok {
			t.Errorf("stats missing key %q (dashboard reads it); body=%s", k, rec.Body.String())
		}
	}
	if got, _ := stats["totalRequests"].(float64); got != 1 {
		t.Errorf("totalRequests = %v, want 1", stats["totalRequests"])
	}

	// byModel is a map keyed "model (provider)" with the per-bucket shape.
	byModel, ok := stats["byModel"].(map[string]any)
	if !ok {
		t.Fatalf("byModel not a map; got %T", stats["byModel"])
	}
	m, ok := byModel["gpt-4 (openai)"].(map[string]any)
	if !ok {
		t.Fatalf("byModel missing 'gpt-4 (openai)'; keys: %v", byModel)
	}
	for _, k := range []string{"rawModel", "provider", "requests", "promptTokens", "completionTokens", "cost", "tpsSum", "tpsCount", "avgTps", "lastUsed"} {
		if _, ok := m[k]; !ok {
			t.Errorf("byModel[gpt-4 (openai)] missing %q; entry=%+v", k, m)
		}
	}
	if m["rawModel"] != "gpt-4" {
		t.Errorf("byModel.rawModel = %v, want gpt-4", m["rawModel"])
	}
	if got, _ := m["promptTokens"].(float64); got != 50 {
		t.Errorf("byModel.promptTokens = %v, want 50", m["promptTokens"])
	}

	// recentRequests is an array of {timestamp, model, provider, promptTokens, ...}.
	rr, ok := stats["recentRequests"].([]any)
	if !ok {
		t.Fatalf("recentRequests not an array; got %T", stats["recentRequests"])
	}
	if len(rr) != 1 {
		t.Fatalf("recentRequests len = %d, want 1", len(rr))
	}
	first, _ := rr[0].(map[string]any)
	if first["model"] != "gpt-4" {
		t.Errorf("recentRequests[0].model = %v, want gpt-4", first["model"])
	}

	// last10Minutes is a 10-element array.
	if l10, _ := stats["last10Minutes"].([]any); len(l10) != 10 {
		t.Errorf("last10Minutes len = %d, want 10", len(l10))
	}

	// pending overlay present from the tracker.
	if _, ok := stats["pending"].(map[string]any); !ok {
		t.Errorf("pending missing or wrong type: %T", stats["pending"])
	}

	// No PascalCase leakage (the regression).
	body := rec.Body.String()
	for _, bad := range []string{"\"TotalRequests\"", "\"ByModel\"", "\"PromptTokens\"", "\"RecentRequests\""} {
		if strings.Contains(body, bad) {
			t.Errorf("stats JSON must not contain PascalCase %s; body=%s", bad, body)
		}
	}
}

// TestUsageStats_PeriodToday uses period=today to exercise the history-scan
// cutoff branch (start-of-day) — guarantees the seeded row is included.
func TestUsageStats_PeriodToday(t *testing.T) {
	db := mustOpenDB(t)
	deps := buildDeps(t, db)
	mux := http.NewServeMux()
	RegisterUsage(mux, deps)
	ck := authCookie(t, deps.SessionStore.(*adapterauth.CookieStore))

	if err := deps.Usage.Save(context.Background(), usage.UsageRecord{
		Provider: "openai", Model: "gpt-4", PromptTokens: 1, CompletionTokens: 1, Status: "success",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/usage/stats?period=today", nil)
	req.Header.Set("Cookie", "auth_token="+ck)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var stats map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &stats)
	if got, _ := stats["totalRequests"].(float64); got != 1 {
		t.Errorf("period=today totalRequests = %v, want 1", stats["totalRequests"])
	}
}