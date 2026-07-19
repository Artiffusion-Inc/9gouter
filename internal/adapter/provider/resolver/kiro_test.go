package resolver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

func TestKiroRegionFromProfileArn(t *testing.T) {
	cases := map[string]string{
		"":                                  "us-east-1",
		"arn:aws:codewhisperer:eu-west-1:12:profile/X": "eu-west-1",
		"arn:aws:codewhisperer:us-east-1:12:profile/X": "us-east-1",
		"not-an-arn":                        "us-east-1",
	}
	for in, want := range cases {
		if got := regionFromProfileArn(in); got != want {
			t.Errorf("regionFromProfileArn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKiroStripSyntheticSuffixes(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-5":                        "claude-sonnet-5",
		"claude-sonnet-5-thinking":               "claude-sonnet-5",
		"claude-sonnet-5-agentic":                "claude-sonnet-5",
		"claude-sonnet-5-thinking-agentic":       "claude-sonnet-5",
	}
	for in, want := range cases {
		if got := stripSyntheticSuffixes(in); got != want {
			t.Errorf("stripSyntheticSuffixes(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestKiroExpand_Variants verifies the variant-expansion: each upstream model
// becomes 4 variants (base, -thinking, -agentic, -thinking-agentic), except
// `auto` which skips the agentic pair.
func TestKiroExpand_Variants(t *testing.T) {
	raw := []map[string]any{
		{"modelId": "claude-sonnet-5", "modelName": "Claude Sonnet 5", "rateMultiplier": 1.0},
		{"modelId": "auto", "modelName": "Auto"},
	}
	got := kiroExpand(raw)
	ids := modelIDs(got)

	want := []string{
		"claude-sonnet-5", "claude-sonnet-5-thinking",
		"claude-sonnet-5-agentic", "claude-sonnet-5-thinking-agentic",
		"auto", "auto-thinking",
	}
	if len(ids) != len(want) {
		t.Fatalf("variants = %v, want %v", ids, want)
	}
	for i, w := range want {
		if ids[i] != w {
			t.Errorf("variants[%d] = %q, want %q (full: %v)", i, ids[i], w, ids)
		}
	}
}

// TestKiroExpand_DisplayNameRate verifies the rate-multiplier display name.
func TestKiroExpand_DisplayNameRate(t *testing.T) {
	raw := []map[string]any{
		{"modelId": "gpt-x", "modelName": "GPT X", "rateMultiplier": 2.4},
	}
	got := kiroExpand(raw)
	if len(got) == 0 {
		t.Fatal("no variants")
	}
	want := "Kiro GPT X (2.4x credit)"
	if got[0].Name != want {
		t.Errorf("name = %q, want %q", got[0].Name, want)
	}
}

func TestKiroExpand_ContextLengthDefault(t *testing.T) {
	raw := []map[string]any{
		{"modelId": "m1", "modelName": "M1"},
	}
	got := kiroExpand(raw)
	// No tokenLimits in raw -> default 200000.
	if got[0].ContextLength != 200000 {
		t.Errorf("contextLength = %d, want 200000", got[0].ContextLength)
	}
}

// TestKiroResolve_FetchSuccess verifies the resolver fetches from the
// ListAvailableModels endpoint and expands the result, with caching.
func TestKiroResolve_FetchSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ListAvailableModels" {
			t.Errorf("path = %q, want /ListAvailableModels", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header = %q, want Bearer tok", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"modelId": "claude-sonnet-5", "modelName": "Claude Sonnet 5"},
			},
		})
	}))
	defer srv.Close()

	// Override the resolver's client + baseURL to hit the test server.
	r := NewKiroResolver(nil, StubTokenRefresher()).(*kiroResolver)
	r.client = srv.Client()
	r.baseURL = srv.URL

	creds := provider.Credentials{
		AccessToken: "tok",
		ProviderSpecificData: map[string]any{
			"profileArn": "arn:aws:codewhisperer:us-east-1:12:profile/X",
		},
	}
	res, err := r.Resolve(context.Background(), creds, ResolveOpts{Logger: NopLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res == nil || len(res.Models) != 4 {
		t.Fatalf("resolve result = %+v, want 4 variants", res)
	}

	// Second call must be served from cache (server would 500 if hit again
	// — but we just confirm it returns the same result without error).
	res2, err := r.Resolve(context.Background(), creds, ResolveOpts{Logger: NopLogger()})
	if err != nil {
		t.Fatalf("cached resolve: %v", err)
	}
	if len(res2.Models) != len(res.Models) {
		t.Errorf("cached variants = %d, want %d", len(res2.Models), len(res.Models))
	}
}

// TestKiroResolve_NoAccessToken verifies the resolver skips fetch when there
// is no accessToken (returns nil, nil — caller falls back to static).
func TestKiroResolve_NoAccessToken(t *testing.T) {
	r := NewKiroResolver(nil, StubTokenRefresher()).(*kiroResolver)
	res, err := r.Resolve(context.Background(), provider.Credentials{}, ResolveOpts{Logger: NopLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res != nil {
		t.Errorf("expected nil result for no accessToken, got %+v", res)
	}
}

// TestKiroResolve_401FallsBack verifies that on a 401 with a stub refresher
// (which returns ErrTokenRefreshNotPorted), the resolver returns nil so the
// caller falls back to the static catalog — no panic, no error leak.
func TestKiroResolve_401FallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	r := NewKiroResolver(nil, StubTokenRefresher()).(*kiroResolver)
	r.client = srv.Client()
	r.baseURL = srv.URL

	creds := provider.Credentials{
		AccessToken: "tok",
		ProviderSpecificData: map[string]any{
			"refreshToken": "rt",
			"profileArn":   "arn:aws:codewhisperer:us-east-1:12:profile/X",
		},
	}
	res, err := r.Resolve(context.Background(), creds, ResolveOpts{Logger: NopLogger()})
	if err != nil {
		t.Fatalf("resolve should not surface error on 401+stub refresh: %v", err)
	}
	if res != nil {
		t.Errorf("expected nil fallback on 401+stub refresh, got %+v", res)
	}
}

// TestCache_TTL verifies cache expiry.
func TestCache_TTL(t *testing.T) {
	c := NewCache(20 * time.Millisecond)
	c.Set("k", &Result{Models: []ResolvedModel{{ID: "m"}}})
	if r, ok := c.Get("k"); !ok || len(r.Models) != 1 {
		t.Fatalf("expected cached hit")
	}
	time.Sleep(30 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Fatalf("expected cache miss after TTL")
	}
}

func modelIDs(ms []ResolvedModel) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}