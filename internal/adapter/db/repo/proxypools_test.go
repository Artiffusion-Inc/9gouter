package repo

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

func TestProxyPoolRepo_RoundTrip(t *testing.T) {
	db := testDB(t)
	r := NewProxyPoolRepo(db)
	ctx := context.Background()

	p := settings.ProxyPool{
		ID:         "pool-1",
		IsActive:   true,
		TestStatus: "ok",
		Data:       json.RawMessage(`{"name":"us-east","proxyUrl":"http://p1","type":"http"}`),
	}
	if err := r.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := r.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || !got.IsActive || got.TestStatus != "ok" {
		t.Fatalf("get mismatch: %+v", got)
	}

	active := true
	list, err := r.List(ctx, ProxyPoolFilter{IsActive: &active})
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list active len = %d, want 1", len(list))
	}

	inactive := false
	list, err = r.List(ctx, ProxyPoolFilter{IsActive: &inactive})
	if err != nil {
		t.Fatalf("list inactive: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("list inactive len = %d, want 0", len(list))
	}

	found, err := r.FindByNameAndType(ctx, "us-east", "http")
	if err != nil {
		t.Fatalf("findByNameAndType: %v", err)
	}
	if found == nil || found.ID != p.ID {
		t.Fatalf("findByNameAndType mismatch: %+v", found)
	}

	p.TestStatus = "fail"
	if err := r.Update(ctx, p); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = r.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.TestStatus != "fail" {
		t.Fatalf("update status = %q, want fail", got.TestStatus)
	}

	removed, err := r.Delete(ctx, p.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if removed == nil || removed.ID != p.ID {
		t.Fatalf("delete return mismatch: %+v", removed)
	}
	got, err = r.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("get deleted: %v", err)
	}
	if got != nil {
		t.Fatalf("expected deleted pool, got %+v", got)
	}
}
