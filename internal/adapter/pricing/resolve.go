package pricing

// resolve.go ports the legacy JS resolution + cost math from
// open-sse/providers/pricing.js: matchPattern (glob, case-insensitive),
// getPricingForModel (3-step fallback chain), and calculateCostFromTokens.
// Resolve merges user overrides (repo.PricingRepo) on top of the hard-coded
// chain so saveUsage can compute cost with one call.

import (
	"regexp"
	"strings"
)

// patternCache compiles glob patterns once. Patterns use "*" as wildcard
// (any substring) and match case-insensitively, matching the JS regex built
// in matchPattern. Each pattern compiles to ^<escaped segments joined by .*>$.
var patternCache = mustCompilePatterns()

func mustCompilePatterns() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patternPricing))
	for i, p := range patternPricing {
		out[i] = compileGlob(p.pattern)
	}
	return out
}

// compileGlob turns a "*" glob into a case-insensitive anchored regexp.
// Mirrors: "^" + pattern.split("*").map(escape).join(".*") + "$" with "i" flag.
func compileGlob(pattern string) *regexp.Regexp {
	parts := strings.Split(pattern, "*")
	for i, s := range parts {
		parts[i] = regexp.QuoteMeta(s)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$")
}

// MatchPattern reports whether model matches the glob pattern. Exported for
// tests/capabilities consumers that mirror the JS matchPattern helper.
func MatchPattern(pattern, model string) bool {
	return compileGlob(pattern).MatchString(model)
}

// GetForModel resolves the hard-coded pricing for a model using the 3-step
// fallback chain (provider override → MODEL_PRICING → PATTERN_PRICING). It does
// NOT consider user overrides — use Resolve for that. Returns zero rate, false
// if no pricing is known (caller treats unknown as cost 0).
func GetForModel(provider, model string) (Rate, bool) {
	if model == "" {
		return Rate{}, false
	}
	// 1. Hard-coded provider-specific override.
	if provider != "" {
		if m, ok := providerPricing[provider]; ok {
			if r, ok := m[model]; ok {
				return r, true
			}
		}
	}
	// 2. Canonical model pricing (strip vendor prefix "deepseek/deepseek-chat").
	baseModel := model
	if i := strings.LastIndex(model, "/"); i >= 0 {
		baseModel = model[i+1:]
	}
	if r, ok := modelPricing[baseModel]; ok {
		return r, true
	}
	if r, ok := modelPricing[model]; ok {
		return r, true
	}
	// 3. Pattern match (first match wins).
	for i, p := range patternPricing {
		if patternCache[i].MatchString(baseModel) || patternCache[i].MatchString(model) {
			return p.rate, true
		}
	}
	return Rate{}, false
}

// UserOverrides is the subset of repo.PricingRepo the Resolver needs: given a
// provider+model, return the user-stored override rate (if any). Keeping it an
// interface lets saveUsage depend on a tiny seam rather than the concrete repo.
type UserOverrides interface {
	RateFor(provider, model string) (Rate, bool)
}

// Resolver resolves pricing for a model, merging user overrides on top of the
// hard-coded fallback chain, and computes cost. Construct once in the
// composition root with the user-override store; nil overrides degrades to the
// hard-coded chain only.
type Resolver struct {
	overrides UserOverrides
}

// NewResolver returns a Resolver. overrides may be nil.
func NewResolver(overrides UserOverrides) *Resolver {
	return &Resolver{overrides: overrides}
}

// Resolve returns the rate for provider+model. Order: user override →
// hard-coded provider override → MODEL_PRICING → PATTERN_PRICING. The second
// return is false when no pricing is known (cost should be 0).
func (r *Resolver) Resolve(provider, model string) (Rate, bool) {
	if model == "" {
		return Rate{}, false
	}
	if r != nil && r.overrides != nil {
		if rate, ok := r.overrides.RateFor(provider, model); ok {
			return rate, true
		}
	}
	return GetForModel(provider, model)
}

// CostFor computes the USD cost for a token breakdown at the resolved rate.
// Returns 0 when the resolver is nil, no pricing is known, or there are no
// tokens — mirroring the legacy calculateCostFromTokens(t, pricing) which
// returns 0 if either is nil.
func (r *Resolver) CostFor(provider, model string, t Tokens) float64 {
	if r == nil {
		return 0
	}
	rate, ok := r.Resolve(provider, model)
	if !ok {
		return 0
	}
	return CalculateCost(t, rate)
}

// CalculateCost is the pure cost formula, ported verbatim from
// calculateCostFromTokens. prompt_tokens is cache-inclusive: cached and
// cache_creation are subsets, so both are subtracted from the input before
// applying the full input rate, then charged at their own (cheaper) rates.
// cached/reasoning/cache_creation fall back to input/output respectively when
// the rate omits them (the JS `pricing.cached || pricing.input` idiom).
func CalculateCost(t Tokens, r Rate) float64 {
	const perMillion = 1_000_000.0

	inputTokens := t.PromptTokens
	cachedTokens := t.CachedTokens
	cacheCreationTokens := t.CacheCreationTokens

	// Subtract cached + cache_creation so they are not charged at full input rate.
	nonCachedInput := inputTokens - cachedTokens - cacheCreationTokens
	if nonCachedInput < 0 {
		nonCachedInput = 0
	}

	cost := float64(nonCachedInput) * (r.Input / perMillion)

	if cachedTokens > 0 {
		rate := r.Cached
		if rate == 0 {
			rate = r.Input
		}
		cost += float64(cachedTokens) * (rate / perMillion)
	}

	outputTokens := t.CompletionTokens
	cost += float64(outputTokens) * (r.Output / perMillion)

	if t.ReasoningTokens > 0 {
		rate := r.Reasoning
		if rate == 0 {
			rate = r.Output
		}
		cost += float64(t.ReasoningTokens) * (rate / perMillion)
	}

	if cacheCreationTokens > 0 {
		rate := r.CacheCreation
		if rate == 0 {
			rate = r.Input
		}
		cost += float64(cacheCreationTokens) * (rate / perMillion)
	}

	return cost
}