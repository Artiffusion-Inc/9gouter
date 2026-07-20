package db

import (
	"sort"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/sqlite"
)

func TestSyncSchema_AllTables(t *testing.T) {
	db, err := sqlite.Open(t.TempDir() + "/schema.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := SyncSchema(db); err != nil {
		t.Fatalf("SyncSchema: %v", err)
	}

	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	sort.Strings(got)

	want := []string{"_meta", "apiKeys", "combos", "kv", "providerConnections", "providerNodes", "proxyPools", "requestDetails", "settings", "usageDaily", "usageHistory"}
	if len(got) != len(want) {
		t.Fatalf("tables = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("table[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}
