package pricing

// pricing_test.go covers the hard-coded pricing port + cost formula, mirroring
// the legacy tests/unit/cached-token-usage.test.js contract. All rates come from
// the real modelPricing/patternPricing tables — no mocks, no fakes. The cost
// formula is the cache-inclusive convention from canonicalizeUsage: prompt_tokens
// already contains cached + cache_creation, so both are subtracted before the
// full input rate is applied and recharged at their own (cheaper) rates.

import (
	"context"
	"encoding/json"
	"math"
	"testing"
)

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestGetForModel_FallbackChain exercises the 3-step resolution:
// exact MODEL_PRICING → PATTERN_PRICING → provider override precedence.
func TestGetForModel_FallbackChain(t *testing.T) {
	// Exact model entry.
	r, ok := GetForModel("openai", "gpt-4")
	if !ok || !approxEq(r.Input, 2.50) || !approxEq(r.Output, 10.00) {
		t.Fatalf("gpt-4 = %+v ok=%v, want input 2.50 output 10.00", r, ok)
	}
	// Pattern fallback: "gpt-5.9-anything" has no exact entry but matches gpt-5*.
	r, ok = GetForModel("openai", "gpt-5.9-foo")
	if !ok || !approxEq(r.Input, 1.25) {
		t.Fatalf("gpt-5.9-foo pattern = %+v ok=%v, want input 1.25 (gpt-5*)", r, ok)
	}
	// Vendor prefix stripping: "deepseek/deepseek-chat" → "deepseek-chat".
	r, ok = GetForModel("anything", "deepseek/deepseek-chat")
	if !ok || !approxEq(r.Input, 0.14) {
		t.Fatalf("prefixed model = %+v ok=%v, want deepseek-chat input 0.14", r, ok)
	}
	// Provider override beats MODEL_PRICING: gh/gpt-5.3-codex.
	r, ok = GetForModel("gh", "gpt-5.3-codex")
	if !ok || !approxEq(r.Input, 1.75) {
		t.Fatalf("gh override = %+v ok=%v, want input 1.75", r, ok)
	}
	// Unknown model with no pattern match.
	if _, ok := GetForModel("weird", "totally-unknown-model-xyz"); ok {
		t.Fatal("unknown model should resolve to no pricing")
	}
	// Case-insensitivity: MiniMax-M2.5 matches MiniMax-* pattern.
	r, ok = GetForModel("", "MiniMax-M9")
	if !ok || !approxEq(r.Input, 0.50) {
		t.Fatalf("MiniMax-M9 = %+v ok=%v, want input 0.50 (MiniMax-*)", r, ok)
	}
}

// TestGetForModel_KimiV0540 pins the upstream v0.5.40 kimi pricing additions
// (commit 68566f53: k3 / kimi-k2.7-code / kimi-for-coding / kimi-k2.6 exact
// entries + the kimi-k3* pattern). Without these the cost calc falls back to
// the kimi-* pattern (1.00 input) instead of the real k3 rate (3.00).
func TestGetForModel_KimiV0540(t *testing.T) {
	// Exact entries added in v0.5.40.
	cases := []struct {
		model         string
		input, output float64
	}{
		{"kimi-k3", 3.00, 15.00},
		{"k3", 3.00, 15.00},
		{"kimi-k2.7-code", 0.95, 4.00},
		{"kimi-k2.7-code-highspeed", 1.90, 8.00},
		{"kimi-for-coding", 0.95, 4.00},
		{"kimi-for-coding-highspeed", 1.90, 8.00},
		{"kimi-k2.6", 1.00, 4.00},
	}
	for _, c := range cases {
		r, ok := GetForModel("kimi", c.model)
		if !ok || !approxEq(r.Input, c.input) || !approxEq(r.Output, c.output) {
			t.Errorf("kimi %s = %+v ok=%v, want input %v output %v", c.model, r, ok, c.input, c.output)
		}
	}
	// Pattern fallback kimi-k3* matches a non-exact k3 variant at the k3 rate,
	// not the kimi-* fallback rate.
	r, ok := GetForModel("kimi", "kimi-k3-coder")
	if !ok || !approxEq(r.Input, 3.00) {
		t.Errorf("kimi-k3-coder pattern = %+v ok=%v, want input 3.00 (kimi-k3*)", r, ok)
	}
}

// TestCalculateCost_Basic mirrors the legacy calculateCostFromTokens basic
// case: 100 prompt + 50 completion at gpt-4 rates (2.50/1.25 input/output per 1M,
// no cache).
func TestCalculateCost_Basic(t *testing.T) {
	cost := CalculateCost(Tokens{PromptTokens: 100, CompletionTokens: 50}, Rate{Input: 2.50, Output: 10.00, Cached: 1.25})
	// 100 * 2.5/1e6 + 50 * 10/1e6 = 0.00025 + 0.0005 = 0.00075
	if !approxEq(cost, 0.00075) {
		t.Fatalf("basic cost = %v, want 0.00075", cost)
	}
}

// TestCalculateCost_CacheInclusive verifies cached + cache_creation are
// subtracted from prompt before the full input rate and recharged cheaper.
func TestCalculateCost_CacheInclusive(t *testing.T) {
	// 200 prompt (of which 80 cached, 20 cache_creation), 100 completion, 30 reasoning.
	// nonCachedInput = 200-80-20 = 100 → 100 * 5/1e6
	// cached 80 * 0.5/1e6
	// completion 100 * 25/1e6
	// reasoning 30 * 25/1e6
	// cache_creation 20 * 6.25/1e6
	r := Rate{Input: 5.00, Output: 25.00, Cached: 0.50, Reasoning: 25.00, CacheCreation: 6.25}
	tok := Tokens{PromptTokens: 200, CompletionTokens: 100, CachedTokens: 80, ReasoningTokens: 30, CacheCreationTokens: 20}
	cost := CalculateCost(tok, r)
	want := 100*5.0/1e6 + 80*0.50/1e6 + 100*25.0/1e6 + 30*25.0/1e6 + 20*6.25/1e6
	if !approxEq(cost, want) {
		t.Fatalf("cache-inclusive cost = %v, want %v", cost, want)
	}
}

// TestCalculateCost_FallbackRates verifies the cached/reasoning/cache_creation
// fallback to input/output when the rate omits those fields (JS
// `pricing.cached || pricing.input` idiom).
func TestCalculateCost_FallbackRates(t *testing.T) {
	// Rate with only input/output: cached→input, reasoning→output, cache_creation→input.
	r := Rate{Input: 2.00, Output: 8.00}
	tok := Tokens{PromptTokens: 300, CompletionTokens: 50, CachedTokens: 100, ReasoningTokens: 40, CacheCreationTokens: 50}
	cost := CalculateCost(tok, r)
	// nonCachedInput = 300-100-50 = 150 → 150 * 2/1e6
	// cached 100 * 2/1e6 (fallback to input)
	// completion 50 * 8/1e6
	// reasoning 40 * 8/1e6 (fallback to output)
	// cache_creation 50 * 2/1e6 (fallback to input)
	want := 150*2.0/1e6 + 100*2.0/1e6 + 50*8.0/1e6 + 40*8.0/1e6 + 50*2.0/1e6
	if !approxEq(cost, want) {
		t.Fatalf("fallback cost = %v, want %v", cost, want)
	}
}

// TestCalculateCost_NoPricing ensures a missing rate yields 0 (legacy returns 0
// when pricing is null).
func TestCalculateCost_NoPricing(t *testing.T) {
	if cost := CalculateCost(Tokens{PromptTokens: 100, CompletionTokens: 50}, Rate{}); !approxEq(cost, 0) {
		t.Fatalf("zero-rate cost = %v, want 0", cost)
	}
}

// TestResolve_UserOverride verifies a user override beats the hard-coded chain
// and that nil overrides degrade to the hard-coded chain only.
func TestResolve_UserOverride(t *testing.T) {
	// Stub override store: claims gpt-4 costs 100x more than the canonical rate.
	overrides := stubOverrides{"openai": {"gpt-4": Rate{Input: 250.00, Output: 1000.00}}}
	r := NewResolver(overrides)
	rate, ok := r.Resolve("openai", "gpt-4")
	if !ok || !approxEq(rate.Input, 250.00) {
		t.Fatalf("override resolve = %+v ok=%v, want input 250 (override)", rate, ok)
	}
	// Cost reflects the override, not the canonical 2.50.
	cost := r.CostFor("openai", "gpt-4", Tokens{PromptTokens: 1_000_000, CompletionTokens: 0})
	if !approxEq(cost, 250.00) {
		t.Fatalf("override cost = %v, want 250.00 (1M input @ 250)", cost)
	}
	// A model with no override still resolves via the hard-coded chain.
	cost = r.CostFor("openai", "gpt-5", Tokens{PromptTokens: 1_000_000, CompletionTokens: 0})
	if !approxEq(cost, 1.25) {
		t.Fatalf("hardcoded fallback cost = %v, want 1.25 (1M input @ 1.25)", cost)
	}
	// nil resolver → 0 cost.
	var nilRes *Resolver
	if cost := nilRes.CostFor("openai", "gpt-4", Tokens{PromptTokens: 100, CompletionTokens: 50}); cost != 0 {
		t.Fatalf("nil resolver cost = %v, want 0", cost)
	}
}

// stubOverrides implements UserOverrides from an in-memory map for the resolver
// tests. This is the only "fake" in the package and it tests the resolver's
// merge logic, not the cost formula or tables (those use the real data above).
type stubOverrides map[string]map[string]Rate

func (s stubOverrides) RateFor(provider, model string) (Rate, bool) {
	if m, ok := s[provider]; ok {
		if r, ok := m[model]; ok {
			return r, true
		}
	}
	return Rate{}, false
}

// TestRepoOverrides_Decode is a smoke test that the kv adapter shape decodes
// the JSON the legacy JS stored (camelCase-ish fields: input/output/cached/
// reasoning/cache_creation).
func TestRepoOverrides_Decode(t *testing.T) {
	store := stubJSONStore{raw: []byte(`{"input":2.5,"output":10,"cached":1.25,"reasoning":15,"cache_creation":2.5}`)}
	ro := &RepoOverrides{Store: store}
	r, ok := ro.RateFor("openai", "gpt-4")
	if !ok || !approxEq(r.Input, 2.5) || !approxEq(r.Output, 10) || !approxEq(r.CacheCreation, 2.5) {
		t.Fatalf("decode = %+v ok=%v", r, ok)
	}
	// Missing entry → false.
	empty := &RepoOverrides{Store: stubJSONStore{raw: nil}}
	if _, ok := empty.RateFor("x", "y"); ok {
		t.Fatal("missing override should return false")
	}
}

type stubJSONStore struct{ raw []byte }

func (s stubJSONStore) GetForModel(_ context.Context, _, _ string) (json.RawMessage, error) {
	return json.RawMessage(s.raw), nil
}