package sqlite

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const pragmaSQL = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA temp_store = MEMORY;
PRAGMA mmap_size = 30000000;
PRAGMA cache_size = -64000;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
`

// Open opens a SQLite database at path using the pure-Go modernc.org/sqlite
// driver and applies the fork's standard PRAGMAs.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite open %s: %w", path, err)
	}
	if _, err := db.Exec(pragmaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite pragma %s: %w", path, err)
	}
	return db, nil
}
