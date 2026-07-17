package repo

import (
	"context"
	"testing"
)

func TestDisabledModelsRepo_RoundTrip(t *testing.T) {
	db := testDB(t)
	r := NewDisabledModelsRepo(db)
	ctx := context.Background()

	if err := r.Disable(ctx, "openai", []string{"gpt-4o", "gpt-4o-mini"}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	ids, err := r.GetByProvider(ctx, "openai")
	if err != nil {
		t.Fatalf("getByProvider: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids len = %d, want 2: %v", len(ids), ids)
	}

	all, err := r.GetAll(ctx)
	if err != nil {
		t.Fatalf("getAll: %v", err)
	}
	if len(all["openai"]) != 2 {
		t.Fatalf("getAll mismatch: %+v", all)
	}

	if err := r.Enable(ctx, "openai", []string{"gpt-4o"}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	ids, err = r.GetByProvider(ctx, "openai")
	if err != nil {
		t.Fatalf("getByProvider after enable: %v", err)
	}
	if len(ids) != 1 || ids[0] != "gpt-4o-mini" {
		t.Fatalf("ids after enable = %v", ids)
	}

	if err := r.Enable(ctx, "openai", []string{"gpt-4o-mini"}); err != nil {
		t.Fatalf("enable last: %v", err)
	}
	ids, err = r.GetByProvider(ctx, "openai")
	if err != nil {
		t.Fatalf("getByProvider after enable all: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids after enable all = %v", ids)
	}
}
