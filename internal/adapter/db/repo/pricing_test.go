package repo

import (
	"context"
	"encoding/json"
	"testing"
)

func TestPricingRepo_RoundTrip(t *testing.T) {
	db := testDB(t)
	r := NewPricingRepo(db)
	ctx := context.Background()

	data := map[string]map[string]json.RawMessage{
		"openai": {
			"gpt-4o": json.RawMessage(`{"input":5,"output":15}`),
		},
	}
	if _, err := r.Update(ctx, data); err != nil {
		t.Fatalf("update: %v", err)
	}

	all, err := r.GetAll(ctx)
	if err != nil {
		t.Fatalf("getAll: %v", err)
	}
	if len(all["openai"]) != 1 {
		t.Fatalf("getAll mismatch: %+v", all)
	}

	pm, err := r.GetForModel(ctx, "openai", "gpt-4o")
	if err != nil {
		t.Fatalf("getForModel: %v", err)
	}
	if string(pm) != `{"input":5,"output":15}` {
		t.Fatalf("getForModel mismatch: %s", pm)
	}

	// Reset a single model.
	all, err = r.Reset(ctx, "openai", "gpt-4o")
	if err != nil {
		t.Fatalf("reset model: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("reset model mismatch: %+v", all)
	}

	// Update again, then reset all.
	if _, err := r.Update(ctx, data); err != nil {
		t.Fatalf("update again: %v", err)
	}
	all, err = r.Reset(ctx, "openai", "")
	if err != nil {
		t.Fatalf("reset provider: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("reset provider mismatch: %+v", all)
	}
}
