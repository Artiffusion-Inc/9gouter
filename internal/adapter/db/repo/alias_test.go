package repo

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAliasRepo_RoundTrip(t *testing.T) {
	db := testDB(t)
	r := NewAliasRepo(db)
	ctx := context.Background()

	if err := r.SetAlias(ctx, "4o", "gpt-4o"); err != nil {
		t.Fatalf("setAlias: %v", err)
	}
	aliases, err := r.GetAliases(ctx)
	if err != nil {
		t.Fatalf("getAliases: %v", err)
	}
	if len(aliases) != 1 || aliases["4o"] != "gpt-4o" {
		t.Fatalf("aliases mismatch: %+v", aliases)
	}
	if err := r.DeleteAlias(ctx, "4o"); err != nil {
		t.Fatalf("deleteAlias: %v", err)
	}
	aliases, err = r.GetAliases(ctx)
	if err != nil {
		t.Fatalf("getAliases after delete: %v", err)
	}
	if len(aliases) != 0 {
		t.Fatalf("aliases after delete len = %d, want 0", len(aliases))
	}

	cm := CustomModel{ProviderAlias: "openai", ID: "custom-1", Type: "llm", Name: "Custom GPT"}
	added, err := r.AddCustomModel(ctx, cm)
	if err != nil {
		t.Fatalf("addCustomModel: %v", err)
	}
	if !added {
		t.Fatalf("addCustomModel want true")
	}
	models, err := r.GetCustomModels(ctx)
	if err != nil {
		t.Fatalf("getCustomModels: %v", err)
	}
	if len(models) != 1 || models[0].Name != "Custom GPT" {
		t.Fatalf("custom models mismatch: %+v", models)
	}
	// Duplicate add should be false.
	added, err = r.AddCustomModel(ctx, cm)
	if err != nil {
		t.Fatalf("add duplicate: %v", err)
	}
	if added {
		t.Fatalf("add duplicate want false")
	}
	if err := r.DeleteCustomModel(ctx, "openai", "custom-1", "llm"); err != nil {
		t.Fatalf("deleteCustomModel: %v", err)
	}
	models, err = r.GetCustomModels(ctx)
	if err != nil {
		t.Fatalf("getCustomModels after delete: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("custom models after delete len = %d, want 0", len(models))
	}

	if err := r.SetMitmAliases(ctx, "tool1", json.RawMessage(`{"a":"b"}`)); err != nil {
		t.Fatalf("setMitmAliases: %v", err)
	}
	mitm, err := r.GetMitmAliases(ctx, "tool1")
	if err != nil {
		t.Fatalf("getMitmAliases: %v", err)
	}
	if string(mitm["a"]) != `"b"` {
		t.Fatalf("mitm mismatch: %+v", mitm)
	}
}
