package repo

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// boolToInt converts a bool to the SQLite INTEGER convention used by the JS
// backend (1 = true, 0 = false).
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// scanBool reads an INTEGER boolean column, treating NULL as false.
func scanBool(v interface{}) bool {
	switch x := v.(type) {
	case int64:
		return x == 1
	case sql.NullInt64:
		return x.Valid && x.Int64 == 1
	case sql.NullBool:
		return x.Valid && x.Bool
	}
	return false
}

// parseTime parses JS ISO timestamps (RFC3339Nano, e.g. 2026-07-17T12:34:56.789Z).
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t, nil
}

// formatTime formats a time as JS ISO.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func now() time.Time { return time.Now().UTC() }

// jsonText normalises a json.RawMessage for storage as SQLite TEXT, matching
// the JS jsonCol.stringifyJson behaviour (nil → "null"). It returns a string
// so the driver binds the parameter with TEXT affinity, not BLOB.
func jsonText(v json.RawMessage) string {
	if len(v) == 0 {
		return "null"
	}
	return string(v)
}

// joinAnd joins WHERE clauses with " AND ".
func joinAnd(parts []string) string {
	return strings.Join(parts, " AND ")
}
