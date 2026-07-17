package repo

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRequestDetailRepo_RoundTrip(t *testing.T) {
	db := testDB(t)
	r := NewRequestDetailRepo(db)
	ctx := context.Background()

	d := RequestDetail{
		ID:           "rd-1",
		Timestamp:    "2026-07-17T12:00:00.000Z",
		Provider:     "openai",
		Model:        "gpt-4o",
		ConnectionID: "conn-1",
		Status:       "ok",
		Tokens:       json.RawMessage(`{"prompt_tokens":5}`),
	}
	if err := r.Save(ctx, d); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := r.GetByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.Provider != "openai" {
		t.Fatalf("get mismatch: %+v", got)
	}

	page, err := r.Query(ctx, RequestDetailFilter{Provider: "openai"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(page.Details) != 1 || page.Pagination.TotalItems != 1 {
		t.Fatalf("query mismatch: %+v", page)
	}

	providers, err := r.DistinctProviders(ctx)
	if err != nil {
		t.Fatalf("distinct: %v", err)
	}
	if len(providers) != 1 || providers[0] != "openai" {
		t.Fatalf("distinct mismatch: %v", providers)
	}
}
