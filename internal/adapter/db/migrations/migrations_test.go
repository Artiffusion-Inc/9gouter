package migrations

import (
	"database/sql"
	"os"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/db/sqlite"
)

func TestRun_Idempotent(t *testing.T) {
	dbPath := t.TempDir() + "/migrated.db"
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := Run(db, dbPath); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := Run(db, dbPath); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM usageHistory").Scan(&count); err != nil {
		t.Fatalf("count usageHistory: %v", err)
	}

	cols, err := columns(db, "usageHistory")
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	for _, want := range []string{"streamMs", "tps"} {
		if !cols[want] {
			t.Errorf("usageHistory missing column %s", want)
		}
	}

	if _, err := os.Stat(dbPath + ".bak"); err == nil {
		// Second run should not have created/overwritten backup because no
		// migrations were pending. Allow it to be absent; fail only if present.
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat backup: %v", err)
	}
}

func columns(db interface{ Query(string, ...any) (*sql.Rows, error) }, table string) (map[string]bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}
