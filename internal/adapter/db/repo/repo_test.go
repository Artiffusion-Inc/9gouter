package repo

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/sqlite"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	dbConn, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	if err := db.SyncSchema(dbConn); err != nil {
		t.Fatalf("sync schema: %v", err)
	}
	t.Cleanup(func() { dbConn.Close() })
	return dbConn
}
