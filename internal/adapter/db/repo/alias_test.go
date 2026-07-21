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

// TestUnquoteJSONString verifies the alias value normalizer handles both the
// legacy JS storage format (stringifyJson → `"gpt-4"`, quoted) and the legacy
// Go AliasRepo format (raw `gpt-4`, unquoted), reading both back as the bare
// model id. This keeps the runtime alias resolver and the backup round-trip
// symmetric regardless of which backend wrote the row.
func TestUnquoteJSONString(t *testing.T) {
	cases := []struct{ in, want string }{
		{"gpt-4", "gpt-4"},              // legacy-Go raw
		{`"gpt-4"`, "gpt-4"},            // legacy-JS JSON-encoded
		{`"foo\"bar"`, `foo"bar`},       // escaped quote inside
		{"", ""},                        // empty
		{`"  "`, "  "},                  // quoted whitespace
		{"unquoted with space", "unquoted with space"},
	}
	for _, c := range cases {
		if got := unquoteJSONString(c.in); got != c.want {
			t.Errorf("unquoteJSONString(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestAliasRepo_GetAliases_ReadsLegacyJSFormat writes a model alias value in
// the legacy JS format (JSON-string-encoded, with surrounding quotes) directly
// into the kv table and verifies GetAliases reads it back as the bare model id.
// This guards the backup-import → runtime-read path: ImportDb writes aliases
// through kvSet with the json.RawMessage payload (quoted), and GetAliases must
// unwrap it rather than returning "gpt-4" with literal quote characters.
func TestAliasRepo_GetAliases_ReadsLegacyJSFormat(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `INSERT INTO kv(scope, key, value) VALUES('modelAliases', 'fast', ?)`, `"gpt-4"`)
	if err != nil {
		t.Fatalf("seed kv: %v", err)
	}
	r := NewAliasRepo(db)
	aliases, err := r.GetAliases(ctx)
	if err != nil {
		t.Fatalf("getAliases: %v", err)
	}
	if aliases["fast"] != "gpt-4" {
		t.Fatalf("aliases[fast] = %q, want gpt-4 (unwrapped JS JSON-string)", aliases["fast"])
	}
}
