package repo

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

func TestConnectionRepo_RoundTrip(t *testing.T) {
	db := testDB(t)
	r := NewConnectionRepo(db)
	ctx := context.Background()

	conn := settings.ProviderConnection{
		ID:       "conn-1",
		Provider: "openai",
		AuthType: "apikey",
		Name:     "Prod",
		Priority: 5,
		IsActive: true,
		Data:     json.RawMessage(`{"apiKey":"sk-123"}`),
	}
	if err := r.Create(ctx, conn); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := r.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.Provider != "openai" || string(got.Data) != string(conn.Data) {
		t.Fatalf("get mismatch: %+v", got)
	}

	// Create a second connection with same provider; reorder should assign 1,2.
	conn2 := settings.ProviderConnection{
		ID:       "conn-2",
		Provider: "openai",
		AuthType: "apikey",
		Name:     "Backup",
		Priority: 1,
		IsActive: true,
		Data:     json.RawMessage(`{"apiKey":"sk-456"}`),
	}
	if err := r.Create(ctx, conn2); err != nil {
		t.Fatalf("create second: %v", err)
	}
	list, err := r.List(ctx, ConnectionFilter{Provider: "openai"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	if list[0].Priority != 1 || list[1].Priority != 2 {
		t.Fatalf("priorities not reordered: %+v", list)
	}

	// Update priority triggers reorder.
	conn.Priority = 0
	if err := r.Update(ctx, conn); err != nil {
		t.Fatalf("update priority: %v", err)
	}
	list, err = r.List(ctx, ConnectionFilter{})
	if err != nil {
		t.Fatalf("list after priority update: %v", err)
	}
	for _, c := range list {
		if c.Priority == 0 {
			t.Fatalf("priority remained zero: %+v", c)
		}
	}

	// Delete triggers reorder.
	if err := r.Delete(ctx, conn2.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, err = r.List(ctx, ConnectionFilter{Provider: "openai"})
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list after delete len = %d, want 1", len(list))
	}

	// Delete by provider.
	if _, err := r.DeleteByProvider(ctx, "openai"); err != nil {
		t.Fatalf("deleteByProvider: %v", err)
	}
	list, err = r.List(ctx, ConnectionFilter{})
	if err != nil {
		t.Fatalf("list all after deleteByProvider: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("list after deleteByProvider len = %d, want 0", len(list))
	}
}
