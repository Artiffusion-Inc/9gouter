package managedashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// PricingService exposes pricing override operations.
type PricingService struct {
	Repo interface {
		GetAll(ctx context.Context) (map[string]map[string]json.RawMessage, error)
		Update(ctx context.Context, pricingData map[string]map[string]json.RawMessage) (map[string]map[string]json.RawMessage, error)
		Reset(ctx context.Context, provider, model string) (map[string]map[string]json.RawMessage, error)
	}
}

// Get returns current user pricing overrides.
func (s *PricingService) Get(ctx context.Context) (map[string]map[string]json.RawMessage, error) {
	return s.Repo.GetAll(ctx)
}

// Validate checks the shape of incoming pricing data.
func (s *PricingService) Validate(body map[string]map[string]json.RawMessage) error {
	validFields := map[string]bool{"input": true, "output": true, "cached": true, "reasoning": true, "cache_creation": true}
	for provider, models := range body {
		if models == nil {
			return fmt.Errorf("invalid pricing for provider: %s", provider)
		}
		for model, pricing := range models {
			if pricing == nil {
				return fmt.Errorf("invalid pricing for model: %s/%s", provider, model)
			}
			var fields map[string]any
			if err := json.Unmarshal(pricing, &fields); err != nil {
				return fmt.Errorf("invalid pricing for model: %s/%s", provider, model)
			}
			for key, value := range fields {
				if !validFields[key] {
					return fmt.Errorf("invalid pricing field: %s for %s/%s", key, provider, model)
				}
				var num json.Number
				switch v := value.(type) {
				case float64:
					if v < 0 {
						return fmt.Errorf("invalid pricing value for %s in %s/%s: must be non-negative number", key, provider, model)
					}
				case json.Number:
					num = v
				case string:
					num = json.Number(v)
				default:
					return fmt.Errorf("invalid pricing value for %s in %s/%s: must be non-negative number", key, provider, model)
				}
				if num != "" {
					f, err := strconv.ParseFloat(string(num), 64)
					if err != nil || f < 0 {
						return fmt.Errorf("invalid pricing value for %s in %s/%s: must be non-negative number", key, provider, model)
					}
				}
			}
		}
	}
	return nil
}

// Update merges pricing overrides.
func (s *PricingService) Update(ctx context.Context, body map[string]map[string]json.RawMessage) (map[string]map[string]json.RawMessage, error) {
	return s.Repo.Update(ctx, body)
}

// Reset clears pricing overrides.
func (s *PricingService) Reset(ctx context.Context, provider, model string) (map[string]map[string]json.RawMessage, error) {
	return s.Repo.Reset(ctx, provider, model)
}
