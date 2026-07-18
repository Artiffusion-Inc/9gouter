// Command smokeimport is a one-off smoke-test helper that imports a backup
// JSON into a Go-backend SQLite file by reusing the package api ImportDb
// logic directly (bypassing session auth). Removed after the smoke test.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Artiffusion-Inc/9router/internal/adapter/db/migrations"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/sqlite"
	"github.com/Artiffusion-Inc/9router/internal/adapter/transport/http/api"
)

func main() {
	backupPath := flag.String("backup", "", "path to backup JSON")
	dbPath := flag.String("db", "./data/9router.db", "path to SQLite db")
	flag.Parse()
	if *backupPath == "" {
		log.Fatal("--backup required")
	}
	raw, err := os.ReadFile(*backupPath)
	if err != nil {
		log.Fatalf("read backup: %v", err)
	}
	var p api.BackupPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		log.Fatalf("parse: %v", err)
	}
	if *dbPath != ":memory:" {
		_ = os.MkdirAll(filepath.Dir(*dbPath), 0o755)
	}
	db, err := sqlite.Open(*dbPath)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := migrations.Run(db, *dbPath); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	if err := api.ImportDb(r, db, &p); err != nil {
		log.Fatalf("import: %v", err)
	}
	fmt.Println("imported OK ->", *dbPath)
}