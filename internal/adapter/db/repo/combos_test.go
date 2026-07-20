package repo

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

func TestComboRepo_RoundTrip(t *testing.T) {
	db := testDB(t)
	r := NewComboRepo(db)
	ctx := context.Background()

	c := settings.Combo{
		ID:     "combo-1",
		Name:   "fast",
		Kind:   "fallback",
		Models: json.RawMessage(`["gpt-4o","claude-opus"]`),
	}
	if err := r.Create(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}

	list, err := r.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
	if string(list[0].Models) != string(c.Models) {
		t.Fatalf("models mismatch: %s vs %s", list[0].Models, c.Models)
	}

	byName, err := r.GetByName(ctx, "fast")
	if err != nil {
		t.Fatalf("getByName: %v", err)
	}
	if byName == nil || byName.Kind != "fallback" {
		t.Fatalf("getByName mismatch: %+v", byName)
	}

	c.Name = "superfast"
	c.Models = json.RawMessage(`["gpt-4o-mini"]`)
	if err := r.Update(ctx, c); err != nil {
		t.Fatalf("update: %v", err)
	}
	byName, err = r.GetByName(ctx, "superfast")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if byName == nil || string(byName.Models) != `["gpt-4o-mini"]` {
		t.Fatalf("update mismatch: %+v", byName)
	}

	if err := r.Delete(ctx, c.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, err = r.List(ctx)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("list after delete len = %d, want 0", len(list))
	}
}
