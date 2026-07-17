package db

import (
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/db/sqlite"
)

func TestSyncSchemaCreatesTables(t *testing.T) {
	dbConn, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dbConn.Close()
	if err := SyncSchema(dbConn); err != nil {
		t.Fatalf("sync: %v", err)
	}
	var count int
	for _, name := range []string{"providerConnections", "usageHistory", "apiKeys", "settings"} {
		if err := dbConn.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count); err != nil {
			t.Fatalf("check %s: %v", name, err)
		}
		if count == 0 {
			t.Fatalf("table %s not created", name)
		}
	}
}
