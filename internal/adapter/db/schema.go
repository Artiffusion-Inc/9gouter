package db

import (
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// SCHEMA_VERSION must be bumped by +1 every time a table, column, or index in
// Schema is changed (add/remove/alter). It drives the pre-change safety backup
// in the migrations runner: when the stored version is lower, a lightweight DB
// backup is taken before applying schema changes. Forgetting to bump only skips
// that backup — it does NOT break the additive auto-sync.
const SCHEMA_VERSION = 2

// TableDef mirrors the JavaScript TABLES entry in src/lib/db/schema.js.
type TableDef struct {
	Columns    map[string]string
	PrimaryKey string
	Indexes    []string
}

// Schema is the declarative current schema. SyncSchema uses it to auto-add
// missing tables, columns, and indexes after versioned migrations. Destructive
// changes (drop/rename/type-change) require a migration file.
var Schema = map[string]TableDef{
	"_meta": {
		Columns: map[string]string{
			"key":   "TEXT PRIMARY KEY",
			"value": "TEXT NOT NULL",
		},
	},
	"settings": {
		Columns: map[string]string{
			"id":   "INTEGER PRIMARY KEY CHECK (id = 1)",
			"data": "TEXT NOT NULL",
		},
	},
	"providerConnections": {
		Columns: map[string]string{
			"id":         "TEXT PRIMARY KEY",
			"provider":   "TEXT NOT NULL",
			"authType":   "TEXT NOT NULL",
			"name":       "TEXT",
			"email":      "TEXT",
			"priority":   "INTEGER",
			"isActive":   "INTEGER DEFAULT 1",
			"data":       "TEXT NOT NULL",
			"createdAt":  "TEXT NOT NULL",
			"updatedAt":  "TEXT NOT NULL",
		},
		Indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_pc_provider ON providerConnections(provider)",
			"CREATE INDEX IF NOT EXISTS idx_pc_provider_active ON providerConnections(provider, isActive)",
			"CREATE INDEX IF NOT EXISTS idx_pc_priority ON providerConnections(provider, priority)",
		},
	},
	"providerNodes": {
		Columns: map[string]string{
			"id":        "TEXT PRIMARY KEY",
			"type":      "TEXT",
			"name":      "TEXT",
			"data":      "TEXT NOT NULL",
			"createdAt": "TEXT NOT NULL",
			"updatedAt": "TEXT NOT NULL",
		},
		Indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_pn_type ON providerNodes(type)",
		},
	},
	"proxyPools": {
		Columns: map[string]string{
			"id":         "TEXT PRIMARY KEY",
			"isActive":   "INTEGER DEFAULT 1",
			"testStatus": "TEXT",
			"data":       "TEXT NOT NULL",
			"createdAt":  "TEXT NOT NULL",
			"updatedAt":  "TEXT NOT NULL",
		},
		Indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_pp_active ON proxyPools(isActive)",
			"CREATE INDEX IF NOT EXISTS idx_pp_status ON proxyPools(testStatus)",
		},
	},
	"apiKeys": {
		Columns: map[string]string{
			"id":        "TEXT PRIMARY KEY",
			"key":       "TEXT UNIQUE NOT NULL",
			"name":      "TEXT",
			"machineId": "TEXT",
			"isActive":  "INTEGER DEFAULT 1",
			"createdAt": "TEXT NOT NULL",
		},
		Indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_ak_key ON apiKeys(key)",
		},
	},
	"combos": {
		Columns: map[string]string{
			"id":        "TEXT PRIMARY KEY",
			"name":      "TEXT UNIQUE NOT NULL",
			"kind":      "TEXT",
			"models":    "TEXT NOT NULL",
			"createdAt": "TEXT NOT NULL",
			"updatedAt": "TEXT NOT NULL",
		},
		Indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_combo_name ON combos(name)",
		},
	},
	"kv": {
		Columns: map[string]string{
			"scope": "TEXT NOT NULL",
			"key":   "TEXT NOT NULL",
			"value": "TEXT NOT NULL",
		},
		PrimaryKey: "PRIMARY KEY (scope, key)",
		Indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_kv_scope ON kv(scope)",
		},
	},
	"usageHistory": {
		Columns: map[string]string{
			"id":               "INTEGER PRIMARY KEY AUTOINCREMENT",
			"timestamp":        "TEXT NOT NULL",
			"provider":         "TEXT",
			"model":            "TEXT",
			"connectionId":     "TEXT",
			"apiKey":           "TEXT",
			"endpoint":         "TEXT",
			"promptTokens":     "INTEGER DEFAULT 0",
			"completionTokens": "INTEGER DEFAULT 0",
			"cost":             "REAL DEFAULT 0",
			"status":           "TEXT",
			"tokens":           "TEXT",
			"meta":             "TEXT",
			"streamMs":         "INTEGER",
			"tps":              "REAL",
		},
		Indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_uh_ts ON usageHistory(timestamp DESC)",
			"CREATE INDEX IF NOT EXISTS idx_uh_provider ON usageHistory(provider)",
			"CREATE INDEX IF NOT EXISTS idx_uh_model ON usageHistory(model)",
			"CREATE INDEX IF NOT EXISTS idx_uh_conn ON usageHistory(connectionId)",
			"CREATE INDEX IF NOT EXISTS idx_uh_tps ON usageHistory(tps) WHERE tps IS NOT NULL",
		},
	},
	"usageDaily": {
		Columns: map[string]string{
			"dateKey": "TEXT PRIMARY KEY",
			"data":    "TEXT NOT NULL",
		},
	},
	"requestDetails": {
		Columns: map[string]string{
			"id":           "TEXT PRIMARY KEY",
			"timestamp":    "TEXT NOT NULL",
			"provider":     "TEXT",
			"model":        "TEXT",
			"connectionId": "TEXT",
			"status":       "TEXT",
			"data":         "TEXT NOT NULL",
		},
		Indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_rd_ts ON requestDetails(timestamp DESC)",
			"CREATE INDEX IF NOT EXISTS idx_rd_provider ON requestDetails(provider)",
			"CREATE INDEX IF NOT EXISTS idx_rd_model ON requestDetails(model)",
			"CREATE INDEX IF NOT EXISTS idx_rd_conn ON requestDetails(connectionId)",
		},
	},
}

// BuildCreateTableSQL renders a CREATE TABLE IF NOT EXISTS statement from a
// TableDef, matching the JavaScript buildCreateTableSql helper.
func BuildCreateTableSQL(name string, def TableDef) string {
	cols := make([]string, 0, len(def.Columns))
	// Stable order so output is deterministic.
	for _, k := range sortedKeys(def.Columns) {
		cols = append(cols, fmt.Sprintf("%s %s", k, def.Columns[k]))
	}
	if def.PrimaryKey != "" {
		cols = append(cols, def.PrimaryKey)
	}
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)", name, strings.Join(cols, ", "))
}

// SyncSchema performs additive schema synchronization: it creates any missing
// tables, adds any missing columns, and creates any missing indexes declared in
// Schema. It never drops or renames existing objects.
func SyncSchema(db *sql.DB) error {
	for name, def := range Schema {
		if _, err := db.Exec(BuildCreateTableSQL(name, def)); err != nil {
			return fmt.Errorf("sync table %s: %w", name, err)
		}

		existing, err := existingColumns(db, name)
		if err != nil {
			return fmt.Errorf("sync pragma table_info %s: %w", name, err)
		}

		for _, colName := range sortedKeys(def.Columns) {
			if _, ok := existing[colName]; ok {
				continue
			}
			safeDef := makeSafeColumnDef(def.Columns[colName])
			stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", name, colName, safeDef)
			if _, err := db.Exec(stmt); err != nil {
				return fmt.Errorf("sync add column %s.%s: %w", name, colName, err)
			}
		}

		for _, idx := range def.Indexes {
			if _, err := db.Exec(idx); err != nil {
				return fmt.Errorf("sync index on %s: %w", name, err)
			}
		}
	}
	return nil
}

func existingColumns(db *sql.DB, table string) (map[string]struct{}, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = struct{}{}
	}
	return out, rows.Err()
}

var (
	pkRegex    = regexp.MustCompile(`(?i)PRIMARY KEY(\s+AUTOINCREMENT)?`)
	uniqueRegex = regexp.MustCompile(`(?i)\bUNIQUE\b`)
)

func makeSafeColumnDef(def string) string {
	// SQLite ADD COLUMN cannot add a PRIMARY KEY/UNIQUE/AUTOINCREMENT column.
	s := pkRegex.ReplaceAllString(def, "")
	s = uniqueRegex.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
