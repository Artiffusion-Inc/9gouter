package proxychat

// usage_cost_test.go is the end-to-end regression for #85: cost in usage was
// always 0 because saveUsage never computed it and the Go rewrite had not
// ported the hard-coded MODEL_PRICING/PATTERN_PRICING tables + the
// calculateCostFromTokens formula from open-sse/providers/pricing.js.
//
// These tests drive the full non-stream Handle path against a REAL on-disk
// SQLite UsageRepo with a real pricing.Resolver (the hard-coded tables — no
// mock), feed an upstream usage object carrying cached/reasoning/cache_creation
// tokens, and assert the persisted usageHistory row carries a non-zero cost
// matching the cache-inclusive formula. No mocks of the repo or the pricing
// tables.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/pricing"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/usage"
)

// claudeUsageExecutor returns an OpenAI-shaped response whose usage object
// carries the full cache-inclusive breakdown so extractTokens + the cost
// formula have something to chew on. Provider id "anthropic" + model
// "claude-sonnet-4" resolves to the real canonical rate via the hard-coded
// MODEL_PRICING table.
type claudeUsageExecutor struct{ body string }

func (claudeUsageExecutor) BuildURL(string, bool, int, provider.Credentials) string { return "http://upstream" }
func (claudeUsageExecutor) BuildHeaders(provider.Credentials, bool) http.Header     { return http.Header{} }
func (claudeUsageExecutor) TransformRequest(string, json.RawMessage, bool, provider.Credentials) (json.RawMessage, error) {
	return json.RawMessage(`{"model":"claude-sonnet-4","messages":[]}`), nil
}
func (e claudeUsageExecutor) Execute(context.Context, provider.ExecRequest) (provider.Resp, error) {
	return provider.Resp{
		Response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(e.body)),
		},
	}, nil
}

// TestHandle_NonStream_RecordsCost verifies the full path: upstream usage with
// cached + cache_creation + reasoning → persisted row has cost > 0 matching the
// cache-inclusive formula at the canonical claude-sonnet-4 rate.
func TestHandle_NonStream_RecordsCost(t *testing.T) {
	usageRepo := realUsageRepo(t)

	// prompt_tokens is cache-inclusive: 500 total = 400 plain + 80 cached + 20 cache_creation.
	// 30 reasoning tokens, 100 completion.
	body := `{"id":"m-1","object":"chat.completion","model":"claude-sonnet-4","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":500,"completion_tokens":100,"total_tokens":600,"cached_tokens":80,"cache_creation_input_tokens":20,"reasoning_tokens":30}}`
	h := New(Dependencies{
		Registry:  func(id string) (DomainProvider, error) { return &stubProvider{id: "anthropic", exec: claudeUsageExecutor{body: body}}, nil },
		UsageRepo: usageRepo,
		StreamPipe: fakeStreamPiper{},
		JSONToSSE:  fakeJSONToSSE{},
		Config:     configForTest(),
		Pricing:    pricing.NewResolver(nil), // hard-coded tables only (no user overrides)
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:            context.Background(),
		Endpoint:       "/v1/chat/completions",
		Body:           json.RawMessage(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}],"stream":false}`),
		ProviderID:     "anthropic",
		Model:          "claude-sonnet-4",
		Stream:         false,
		APIKey:         "sk-test",
		ConnectionID:   "conn-cost",
		ResponseWriter: rec,
	}
	if _, err := h.Handle(context.Background(), req); err != nil {
		t.Fatalf("Handle error: %v", err)
	}

	rows, err := usageRepo.Query(context.Background(), usage.Query{Limit: 100})
	if err != nil {
		t.Fatalf("usage Query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.PromptTokens != 500 || r.CompletionTokens != 100 {
		t.Fatalf("tokens = %d/%d, want 500/100", r.PromptTokens, r.CompletionTokens)
	}

	// claude-sonnet-4 canonical rate: input 3, output 15, cached 0.30, reasoning 22.50, cache_creation 3.00.
	// nonCachedInput = 500-80-20 = 400 → 400 * 3/1e6
	// cached 80 * 0.30/1e6
	// completion 100 * 15/1e6
	// reasoning 30 * 22.50/1e6
	// cache_creation 20 * 3.00/1e6
	want := 400*3.0/1e6 + 80*0.30/1e6 + 100*15.0/1e6 + 30*22.50/1e6 + 20*3.00/1e6
	if r.Cost == 0 {
		t.Fatalf("persisted cost = 0, want %v (regression: cost was always 0 before pricing port)", want)
	}
	if diff := r.Cost - want; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("persisted cost = %v, want %v", r.Cost, want)
	}

	// The tokens blob must carry the detailed breakdown for backup/restore parity.
	if len(r.Tokens) == 0 {
		t.Fatal("persisted tokens blob empty; saveUsageWith must store the detailed breakdown")
	}
	var tok map[string]int
	if err := json.Unmarshal(r.Tokens, &tok); err != nil {
		t.Fatalf("unmarshal tokens blob: %v (raw=%s)", err, string(r.Tokens))
	}
	if tok["cached_tokens"] != 80 || tok["reasoning_tokens"] != 30 || tok["cache_creation_input_tokens"] != 20 {
		t.Errorf("tokens blob = %+v, want cached=80 reasoning=30 cache_creation=20", tok)
	}
}

// TestHandle_NonStream_CostZeroWithoutResolver pins the nil-Resolver contract:
// when no pricing resolver is wired (legacy/tests), cost stays 0 even with
// tokens present — saveUsage must not panic and must still persist the row.
func TestHandle_NonStream_CostZeroWithoutResolver(t *testing.T) {
	usageRepo := realUsageRepo(t)
	body := `{"id":"m-1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":25,"total_tokens":125}}`
	h := New(Dependencies{
		Registry:  func(id string) (DomainProvider, error) { return &stubProvider{id: "openai", exec: claudeUsageExecutor{body: body}}, nil },
		UsageRepo: usageRepo,
		StreamPipe: fakeStreamPiper{},
		JSONToSSE:  fakeJSONToSSE{},
		Config:     configForTest(),
		// Pricing left nil.
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:            context.Background(),
		Endpoint:       "/v1/chat/completions",
		Body:           json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":false}`),
		ProviderID:     "openai",
		Model:          "gpt-4",
		Stream:         false,
		APIKey:         "sk-test",
		ConnectionID:   "conn-nocost",
		ResponseWriter: rec,
	}
	if _, err := h.Handle(context.Background(), req); err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	rows, err := usageRepo.Query(context.Background(), usage.Query{Limit: 100})
	if err != nil {
		t.Fatalf("usage Query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	if rows[0].Cost != 0 {
		t.Errorf("cost with nil resolver = %v, want 0", rows[0].Cost)
	}
}

// TestExtractTokens_UsageShapes covers the extractTokens helper directly
// against the three real usage shapes: OpenAI o-series (nested
// completion_tokens_details.reasoning_tokens), Claude (cache_read_input_tokens
// + cache_creation_input_tokens), and a flat translated shape.
func TestExtractTokens_UsageShapes(t *testing.T) {
	t.Parallel()
	// OpenAI o-series: reasoning nested in completion_tokens_details.
	openai := map[string]any{"usage": map[string]any{
		"prompt_tokens": 100, "completion_tokens": 50,
		"completion_tokens_details": map[string]any{"reasoning_tokens": 40},
	}}
	got := extractTokens(openai, 100, 50)
	if got.ReasoningTokens != 40 {
		t.Errorf("openai reasoning = %d, want 40", got.ReasoningTokens)
	}

	// Claude: cache_read_input_tokens + cache_creation_input_tokens.
	claude := map[string]any{"usage": map[string]any{
		"prompt_tokens": 200, "completion_tokens": 30,
		"cache_read_input_tokens": 60, "cache_creation_input_tokens": 15,
	}}
	got = extractTokens(claude, 200, 30)
	if got.CachedTokens != 60 || got.CacheCreationTokens != 15 {
		t.Errorf("claude cached/creation = %d/%d, want 60/15", got.CachedTokens, got.CacheCreationTokens)
	}

	// No usage object → nil (formula degrades to flat prompt/completion only).
	if got := extractTokens(map[string]any{}, 10, 5); got != nil {
		t.Errorf("missing usage → got %+v, want nil", got)
	}
}