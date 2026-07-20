package repo

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

func TestNodeRepo_RoundTrip(t *testing.T) {
	db := testDB(t)
	r := NewNodeRepo(db)
	ctx := context.Background()

	n := settings.ProviderNode{
		ID:   "node-1",
		Type: "openai",
		Name: "OpenAI Official",
		Data: json.RawMessage(`{"baseUrl":"https://api.openai.com"}`),
	}
	if err := r.Create(ctx, n); err != nil {
		t.Fatalf("create: %v", err)
	}

	list, err := r.List(ctx, NodeFilter{Type: "openai"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Name != "OpenAI Official" {
		t.Fatalf("list mismatch: %+v", list)
	}

	got, err := r.GetByID(ctx, n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || string(got.Data) != string(n.Data) {
		t.Fatalf("get mismatch: %+v", got)
	}

	n.Name = "OpenAI Updated"
	n.Data = json.RawMessage(`{"baseUrl":"https://api.openai.com/v2"}`)
	if err := r.Update(ctx, n); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = r.GetByID(ctx, n.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Name != "OpenAI Updated" {
		t.Fatalf("update mismatch: %+v", got)
	}

	if err := r.Delete(ctx, n.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, err = r.List(ctx, NodeFilter{})
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("list after delete len = %d, want 0", len(list))
	}
}
