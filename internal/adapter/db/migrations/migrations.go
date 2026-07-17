package migrations

import (
	"database/sql"
	"embed"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed *.sql
var sqlFS embed.FS

// migration holds a parsed versioned SQL migration.
type migration struct {
	version int
	name    string
	sql     string
}

// Run applies all versioned SQL migrations in order, tracking the latest
// applied version in the _meta table. It is idempotent: running twice produces
// no error and applies no additional migrations.
//
// If dbPath points to an existing file and at least one migration would be
// applied, a pre-change backup is copied to dbPath + ".bak" before any schema
// mutation occurs.
func Run(db *sql.DB, dbPath string) error {
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	if len(migs) == 0 {
		return nil
	}

	if err := ensureMeta(db); err != nil {
		return err
	}

	current, err := getSchemaVersion(db)
	if err != nil {
		return err
	}

	target := migs[len(migs)-1].version
	if current >= target {
		return nil
	}

	// Pre-change safety backup: only when an existing DB file is present and
	// the schema version would change.
	if dbPath != "" && dbPath != ":memory:" {
		if fi, err := os.Stat(dbPath); err == nil && !fi.IsDir() {
			if backupErr := copyFile(dbPath, dbPath+".bak"); backupErr != nil {
				return fmt.Errorf("pre-change backup failed: %w", backupErr)
			}
		}
	}

	for _, m := range migs {
		if m.version <= current {
			continue
		}
		if err := runMigration(db, m); err != nil {
			return fmt.Errorf("migration %d %s: %w", m.version, m.name, err)
		}
		if err := setSchemaVersion(db, m.version); err != nil {
			return err
		}
		current = m.version
	}

	return nil
}

func loadMigrations() ([]migration, error) {
	entries, err := sqlFS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	re := regexp.MustCompile(`^(\d{4})_([^.]+)\.sql$`)
	var migs []migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		matches := re.FindStringSubmatch(e.Name())
		if matches == nil {
			continue
		}
		ver, err := strconv.Atoi(matches[1])
		if err != nil {
			continue
		}
		b, err := sqlFS.ReadFile(e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		migs = append(migs, migration{
			version: ver,
			name:    matches[2],
			sql:     string(b),
		})
	}

	sort.Slice(migs, func(i, j int) bool {
		return migs[i].version < migs[j].version
	})
	return migs, nil
}

func runMigration(db *sql.DB, m migration) error {
	stmts := splitStatements(m.sql)
	for _, stmt := range stmts {
		if stmt == "" {
			continue
		}
		if ok, err := shouldSkipAddColumn(db, stmt); err != nil {
			return err
		} else if ok {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("exec: %s: %w", stmt, err)
		}
	}
	return nil
}

// shouldSkipAddColumn mirrors the JavaScript 002-add-stream-ms-tps.js guard:
// it checks PRAGMA table_info before executing ALTER TABLE ... ADD COLUMN so
// the migration stays idempotent even when the initial schema already contains
// the column.
func shouldSkipAddColumn(db *sql.DB, stmt string) (bool, error) {
	re := regexp.MustCompile(`(?i)^ALTER\s+TABLE\s+(\S+)\s+ADD\s+COLUMN\s+(\S+)`)
	m := re.FindStringSubmatch(stmt)
	if m == nil {
		return false, nil
	}
	table := strings.Trim(m[1], "\"")
	col := strings.Trim(m[2], "\"")

	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if strings.EqualFold(name, col) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func splitStatements(sql string) []string {
	var out []string
	var buf strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	for _, raw := range strings.Split(buf.String(), ";") {
		stmt := strings.TrimSpace(raw)
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

func ensureMeta(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS _meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
	return err
}

func getSchemaVersion(db *sql.DB) (int, error) {
	var val sql.NullString
	err := db.QueryRow(`SELECT value FROM _meta WHERE key = 'schema_version'`).Scan(&val)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !val.Valid || val.String == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(val.String)
	if err != nil {
		return 0, fmt.Errorf("invalid schema_version %q: %w", val.String, err)
	}
	return n, nil
}

func setSchemaVersion(db *sql.DB, version int) error {
	_, err := db.Exec(
		`INSERT INTO _meta(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		strconv.Itoa(version),
	)
	return err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// path is imported for future expansion and to avoid "imported and not used"
// if we later need path manipulation. It is intentionally referenced below.
var _ = path.Base
