package repo

import (
	"context"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

func TestAPIKeyRepo_RoundTrip(t *testing.T) {
	db := testDB(t)
	r := NewAPIKeyRepo(db)
	ctx := context.Background()

	k := settings.APIKey{
		ID:        "ak-1",
		Key:       "key-secret-1",
		Name:      "Test Key",
		MachineID: "machine-a",
		IsActive:  true,
	}
	if err := r.Create(ctx, k); err != nil {
		t.Fatalf("create: %v", err)
	}

	list, err := r.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
	if got := list[0]; got.ID != k.ID || got.Key != k.Key || got.Name != k.Name || got.MachineID != k.MachineID || !got.IsActive {
		t.Fatalf("list mismatch: %+v", got)
	}

	byID, err := r.GetByID(ctx, k.ID)
	if err != nil {
		t.Fatalf("getByID: %v", err)
	}
	if byID == nil || byID.Key != k.Key {
		t.Fatalf("getByID mismatch: %+v", byID)
	}

	byKey, err := r.GetByKey(ctx, k.Key)
	if err != nil {
		t.Fatalf("getByKey: %v", err)
	}
	if byKey == nil || byKey.ID != k.ID {
		t.Fatalf("getByKey mismatch: %+v", byKey)
	}

	ok, err := r.Validate(ctx, k.Key)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !ok {
		t.Fatalf("validate want true")
	}

	if err := r.SetActive(ctx, k.ID, false); err != nil {
		t.Fatalf("setActive: %v", err)
	}
	ok, err = r.Validate(ctx, k.Key)
	if err != nil {
		t.Fatalf("validate after disable: %v", err)
	}
	if ok {
		t.Fatalf("validate want false after disable")
	}

	k.Name = "Updated"
	if err := r.Update(ctx, k); err != nil {
		t.Fatalf("update: %v", err)
	}
	byID, err = r.GetByID(ctx, k.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if byID.Name != "Updated" {
		t.Fatalf("update name = %q, want Updated", byID.Name)
	}

	if err := r.Delete(ctx, k.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, err = r.List(ctx)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("list after delete len = %d, want 0", len(list))
	}

	_, err = r.GetByID(ctx, k.ID)
	if err != nil {
		t.Fatalf("get deleted: %v", err)
	}
}
