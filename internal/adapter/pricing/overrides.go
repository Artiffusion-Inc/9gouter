package pricing

// overrides.go adapts repo.PricingRepo (user overrides stored as JSON in the kv
// table) to the pricing.UserOverrides seam. The repo returns
// map[provider]map[model]json.RawMessage; each value is a Rate-shaped object
// (input/output/cached/reasoning/cache_creation). We decode lazily on lookup
// so the hot saveUsage path only parses the one model it needs.

import (
	"context"
	"encoding/json"
)

// OverrideStore is the subset of repo.PricingRepo the adapter uses. Kept as an
// interface so the pricing package does not import the db/repo package
// (avoiding an adapter/db → adapter/pricing cycle via composition root wiring).
type OverrideStore interface {
	GetForModel(ctx context.Context, provider, model string) (json.RawMessage, error)
}

// RepoOverrides implements UserOverrides backed by repo.PricingRepo. Each
// RateFor lookup pulls the raw override JSON for provider+model and decodes it
// into a Rate. Missing/malformed → no override (falls through to hard-coded).
type RepoOverrides struct {
	Store OverrideStore
}

// RateFor returns the user-stored override rate for provider+model, if any.
// A missing entry or a decode error yields (zero, false) so the resolver
// falls back to the hard-coded chain — matches the JS getPricingForModel
// behavior where getUserPricing() misses are ignored.
func (o *RepoOverrides) RateFor(provider, model string) (Rate, bool) {
	if o == nil || o.Store == nil || model == "" {
		return Rate{}, false
	}
	raw, err := o.Store.GetForModel(context.Background(), provider, model)
	if err != nil || len(raw) == 0 {
		return Rate{}, false
	}
	var r Rate
	if err := json.Unmarshal(raw, &r); err != nil {
		return Rate{}, false
	}
	return r, true
}