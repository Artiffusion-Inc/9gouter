package sqlite

import (
	"strings"
	"testing"
)

func TestOpen_PRAGMAs(t *testing.T) {
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	cases := []struct {
		pragma string
		want   string
	}{
		// SQLite returns the textual journal mode.
		{"journal_mode", "wal"},
		// SQLite returns synchronous as a numeric code: 1 = NORMAL.
		{"synchronous", "1"},
		// foreign_keys is returned as 0/1.
		{"foreign_keys", "1"},
	}
	for _, c := range cases {
		var got string
		if err := db.QueryRow("PRAGMA " + c.pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", c.pragma, err)
		}
		if !strings.EqualFold(strings.TrimSpace(got), c.want) {
			t.Errorf("PRAGMA %s = %q, want %q", c.pragma, got, c.want)
		}
	}
}
