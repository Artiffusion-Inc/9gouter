package repo

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSettingsRepo_RoundTrip(t *testing.T) {
	db := testDB(t)
	r := NewSettingsRepo(db)
	ctx := context.Background()

	s, err := r.Get(ctx)
	if err != nil {
		t.Fatalf("get empty: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(s.Data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["authMode"] != "password" {
		t.Fatalf("default authMode = %v, want password", m["authMode"])
	}
	if m["requireLogin"] != true {
		t.Fatalf("default requireLogin = %v, want true", m["requireLogin"])
	}

	updated, err := r.Update(ctx, json.RawMessage(`{"authMode":"oidc","tunnelUrl":"https://t"}`))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := json.Unmarshal(updated.Data, &m); err != nil {
		t.Fatalf("unmarshal updated: %v", err)
	}
	if m["authMode"] != "oidc" {
		t.Fatalf("authMode after update = %v, want oidc", m["authMode"])
	}
	if m["tunnelUrl"] != "https://t" {
		t.Fatalf("tunnelUrl after update = %v", m["tunnelUrl"])
	}
	// Defaults remain for untouched keys.
	if m["requireLogin"] != true {
		t.Fatalf("requireLogin should still be true: %v", m["requireLogin"])
	}

	exported, err := r.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if err := json.Unmarshal(exported, &m); err != nil {
		t.Fatalf("unmarshal export: %v", err)
	}
	if _, ok := m["authMode"]; !ok {
		t.Fatalf("export missing authMode")
	}
}
