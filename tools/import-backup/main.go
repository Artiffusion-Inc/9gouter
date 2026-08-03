package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	api "github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/http/api"
	dbschema "github.com/Artiffusion-Inc/9gouter/internal/adapter/db"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/migrations"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/sqlite"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: import-backup <db-path> <backup.json>")
		os.Exit(2)
	}
	dbPath, backupPath := os.Args[1], os.Args[2]

	db, err := sqlite.Open(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db:", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := migrations.Run(db, dbPath); err != nil {
		fmt.Fprintln(os.Stderr, "migrations:", err)
		os.Exit(1)
	}
	if err := dbschema.SyncSchema(db); err != nil {
		fmt.Fprintln(os.Stderr, "sync schema:", err)
		os.Exit(1)
	}

	f, err := os.Open(backupPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open backup:", err)
		os.Exit(1)
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read backup:", err)
		os.Exit(1)
	}
	var payload api.BackupPayload
	dec := json.NewDecoder(os.Stdin)
	_ = dec
	if err := json.Unmarshal(raw, &payload); err != nil {
		fmt.Fprintln(os.Stderr, "unmarshal backup:", err)
		os.Exit(1)
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil)
	if err := api.ImportDb(req, db, &payload); err != nil {
		fmt.Fprintln(os.Stderr, "import:", err)
		os.Exit(1)
	}
	fmt.Println("import ok")
}