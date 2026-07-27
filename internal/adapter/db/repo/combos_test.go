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

// TestComboRepo_NullKind reproduces the regression where legacy combos store
// kind as SQL NULL: scanning a NULL column into a plain string fails and
// /api/combos returned 500. List/GetByName/GetByID must accept NULL kind and
// surface it as an empty string.
func TestComboRepo_NullKind(t *testing.T) {
	db := testDB(t)
	r := NewComboRepo(db)
	ctx := context.Background()

	// Insert a row with kind = NULL, mirroring the JS-era rows.
	if _, err := db.ExecContext(ctx, `INSERT INTO combos(id, name, kind, models, createdAt, updatedAt) VALUES(?, ?, NULL, ?, ?, ?)`,
		"combo-null", "nulkind", `["a"]`, "2026-05-16T06:58:06.860Z", "2026-05-16T06:58:06.860Z"); err != nil {
		t.Fatalf("seed null-kind row: %v", err)
	}

	list, err := r.List(ctx)
	if err != nil {
		t.Fatalf("list with NULL kind should not error, got: %v", err)
	}
	var found bool
	for _, c := range list {
		if c.ID == "combo-null" {
			found = true
			if c.Kind != "" {
				t.Fatalf("NULL kind should scan to empty string, got %q", c.Kind)
			}
		}
	}
	if !found {
		t.Fatalf("NULL-kind row missing from list")
	}

	byName, err := r.GetByName(ctx, "nulkind")
	if err != nil || byName == nil {
		t.Fatalf("getByName null-kind: %v %v", err, byName)
	}
	if byName.Kind != "" {
		t.Fatalf("getByName NULL kind should be empty, got %q", byName.Kind)
	}
	byID, err := r.GetByID(ctx, "combo-null")
	if err != nil || byID == nil {
		t.Fatalf("getByID null-kind: %v %v", err, byID)
	}
	if byID.Kind != "" {
		t.Fatalf("getByID NULL kind should be empty, got %q", byID.Kind)
	}
}
