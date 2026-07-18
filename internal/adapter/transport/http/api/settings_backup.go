package api

// Database backup import/export mirroring the legacy src/lib/db/index.js
// ExportDb()/ImportDb() 1:1. The payload shape is the one produced by the JS
// dashboard "Download backup" button and accepted by its "Restore" button.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
)

// BackupPayload mirrors ExportDb()'s output / ImportDb()'s input.
type BackupPayload struct {
	Settings           json.RawMessage           `json:"settings"`
	ProviderConnections []map[string]any          `json:"providerConnections"`
	ProviderNodes      []map[string]any           `json:"providerNodes"`
	ProxyPools         []map[string]any           `json:"proxyPools"`
	APIKeys            []map[string]any           `json:"apiKeys"`
	Combos             []map[string]any           `json:"combos"`
	ModelAliases       map[string]json.RawMessage `json:"modelAliases"`
	CustomModels       []map[string]any           `json:"customModels"`
	MitmAlias          map[string]json.RawMessage `json:"mitmAlias"`
	Pricing            map[string]json.RawMessage `json:"pricing"`
}

// ExportDb reads every config table and assembles the legacy backup payload.
func ExportDb(r *http.Request, db *sql.DB) (*BackupPayload, error) {
	ctx := r.Context()
	out := &BackupPayload{}

	// settings (single row, id=1, raw data).
	var settingsData []byte
	err := db.QueryRowContext(ctx, `SELECT data FROM settings WHERE id = 1`).Scan(&settingsData)
	if err == nil && len(settingsData) > 0 {
		out.Settings = json.RawMessage(settingsData)
	}

	// providerConnections: data is merged onto the row.
	rows, err := db.QueryContext(ctx, `SELECT id, provider, authType, name, email, priority, isActive, data, createdAt, updatedAt FROM providerConnections`)
	if err != nil {
		return nil, fmt.Errorf("export connections: %w", err)
	}
	out.ProviderConnections, err = scanRows(rows, func(s []sql.NullString, data []byte) map[string]any {
		m := parseJSONMap(data)
		mergeKnown(m, map[string]any{
			"id": s[0].String, "provider": s[1].String, "authType": s[2].String,
			"name": nullStr(s[3]), "email": nullStr(s[4]), "priority": nullInt(s[5]),
			"isActive": s[6].String == "1", "createdAt": s[7].String, "updatedAt": s[8].String,
		})
		return m
	}, 9)
	if err != nil {
		return nil, err
	}

	// providerNodes.
	rows, err = db.QueryContext(ctx, `SELECT id, type, name, data, createdAt, updatedAt FROM providerNodes`)
	if err != nil {
		return nil, fmt.Errorf("export nodes: %w", err)
	}
	out.ProviderNodes, err = scanRows(rows, func(s []sql.NullString, data []byte) map[string]any {
		m := parseJSONMap(data)
		mergeKnown(m, map[string]any{
			"id": s[0].String, "type": s[1].String, "name": nullStr(s[2]),
			"createdAt": s[3].String, "updatedAt": s[4].String,
		})
		return m
	}, 5)
	if err != nil {
		return nil, err
	}

	// proxyPools.
	rows, err = db.QueryContext(ctx, `SELECT id, isActive, testStatus, data, createdAt, updatedAt FROM proxyPools`)
	if err != nil {
		return nil, fmt.Errorf("export proxyPools: %w", err)
	}
	out.ProxyPools, err = scanRows(rows, func(s []sql.NullString, data []byte) map[string]any {
		m := parseJSONMap(data)
		mergeKnown(m, map[string]any{
			"id": s[0].String, "isActive": s[1].String == "1", "testStatus": s[2].String,
			"createdAt": s[3].String, "updatedAt": s[4].String,
		})
		return m
	}, 5)
	if err != nil {
		return nil, err
	}

	// apiKeys.
	rows, err = db.QueryContext(ctx, `SELECT id, key, name, machineId, isActive, createdAt FROM apiKeys`)
	if err != nil {
		return nil, fmt.Errorf("export apiKeys: %w", err)
	}
	out.APIKeys, err = scanStringRows(rows, func(s []sql.NullString) map[string]any {
		return map[string]any{
			"id": s[0].String, "key": s[1].String, "name": nullStr(s[2]),
			"machineId": nullStr(s[3]), "isActive": s[4].String == "1", "createdAt": s[5].String,
		}
	}, 6)
	if err != nil {
		return nil, err
	}

	// combos.
	rows, err = db.QueryContext(ctx, `SELECT id, name, kind, models, createdAt, updatedAt FROM combos`)
	if err != nil {
		return nil, fmt.Errorf("export combos: %w", err)
	}
	out.Combos, err = scanRows(rows, func(s []sql.NullString, data []byte) map[string]any {
		return map[string]any{
			"id": s[0].String, "name": s[1].String, "kind": s[2].String,
			"models": parseJSONArray(data), "createdAt": s[3].String, "updatedAt": s[4].String,
		}
	}, 5)
	if err != nil {
		return nil, err
	}

	// kv-backed maps.
	out.ModelAliases, err = kvMap(ctx, db, "modelAliases")
	if err != nil {
		return nil, err
	}
	out.MitmAlias, err = kvMap(ctx, db, "mitmAlias")
	if err != nil {
		return nil, err
	}
	out.Pricing, err = kvMap(ctx, db, "pricing")
	if err != nil {
		return nil, err
	}
	customRows, err := kvList(ctx, db, "customModels")
	if err != nil {
		return nil, err
	}
	out.CustomModels = make([]map[string]any, 0, len(customRows))
	for _, v := range customRows {
		out.CustomModels = append(out.CustomModels, parseJSONMap(v))
	}
	return out, nil
}

// ImportDb wipes the config tables and inserts the payload, mirroring the
// legacy ImportDb() transaction. Everything outside the well-known columns
// goes back into the per-row data JSON blob.
func ImportDb(r *http.Request, db *sql.DB, p *BackupPayload) error {
	ctx := r.Context()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("import begin: %w", err)
	}
	defer tx.Rollback()

	for _, stmt := range []string{
		`DELETE FROM settings`,
		`DELETE FROM providerConnections`,
		`DELETE FROM providerNodes`,
		`DELETE FROM proxyPools`,
		`DELETE FROM apiKeys`,
		`DELETE FROM combos`,
		`DELETE FROM kv WHERE scope IN ('modelAliases','customModels','mitmAlias','pricing')`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("wipe %q: %w", stmt, err)
		}
	}

	// settings (single row, id=1).
	if len(p.Settings) > 0 && string(p.Settings) != "null" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings(id, data) VALUES(1, ?)
			 ON CONFLICT(id) DO UPDATE SET data = excluded.data`, []byte(p.Settings)); err != nil {
			return fmt.Errorf("insert settings: %w", err)
		}
	}

	for _, c := range p.ProviderConnections {
		data := stripKeys(c, "id", "provider", "authType", "name", "email", "priority", "isActive", "createdAt", "updatedAt")
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO providerConnections(id, provider, authType, name, email, priority, isActive, data, createdAt, updatedAt)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			str(c, "id"), str(c, "provider"), strOr(c, "authType", "oauth"), nullAny(c, "name"), nullAny(c, "email"), intOr(c, "priority", 0),
			bool01(c, "isActive"), mustJSON(data), strOr(c, "createdAt", nowISO()), strOr(c, "updatedAt", nowISO()),
		); err != nil {
			return fmt.Errorf("insert providerConnection: %w", err)
		}
	}

	for _, n := range p.ProviderNodes {
		data := stripKeys(n, "id", "type", "name", "createdAt", "updatedAt")
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO providerNodes(id, type, name, data, createdAt, updatedAt) VALUES(?, ?, ?, ?, ?, ?)`,
			str(n, "id"), nullAny(n, "type"), nullAny(n, "name"), mustJSON(data),
			strOr(n, "createdAt", nowISO()), strOr(n, "updatedAt", nowISO()),
		); err != nil {
			return fmt.Errorf("insert providerNode: %w", err)
		}
	}

	for _, pp := range p.ProxyPools {
		data := stripKeys(pp, "id", "isActive", "testStatus", "createdAt", "updatedAt")
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO proxyPools(id, isActive, testStatus, data, createdAt, updatedAt) VALUES(?, ?, ?, ?, ?, ?)`,
			str(pp, "id"), bool01(pp, "isActive"), strOr(pp, "testStatus", "unknown"), mustJSON(data),
			strOr(pp, "createdAt", nowISO()), strOr(pp, "updatedAt", nowISO()),
		); err != nil {
			return fmt.Errorf("insert proxyPool: %w", err)
		}
	}

	for _, k := range p.APIKeys {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO apiKeys(id, key, name, machineId, isActive, createdAt) VALUES(?, ?, ?, ?, ?, ?)`,
			str(k, "id"), str(k, "key"), nullAny(k, "name"), nullAny(k, "machineId"),
			bool01(k, "isActive"), strOr(k, "createdAt", nowISO()),
		); err != nil {
			return fmt.Errorf("insert apiKey: %w", err)
		}
	}

	for _, c := range p.Combos {
		models := []byte("[]")
		if v, ok := c["models"]; ok && v != nil {
			if b, err := json.Marshal(v); err == nil {
				models = b
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO combos(id, name, kind, models, createdAt, updatedAt) VALUES(?, ?, ?, ?, ?, ?)`,
			str(c, "id"), str(c, "name"), nullAny(c, "kind"), models,
			strOr(c, "createdAt", nowISO()), strOr(c, "updatedAt", nowISO()),
		); err != nil {
			return fmt.Errorf("insert combo: %w", err)
		}
	}

	for k, v := range p.ModelAliases {
		if err := kvSet(ctx, tx, "modelAliases", k, v); err != nil {
			return fmt.Errorf("insert modelAlias %q: %w", k, err)
		}
	}
	for _, m := range p.CustomModels {
		key := fmt.Sprintf("%s|%s|%s", strOr(m, "providerAlias", ""), strOr(m, "id", ""), strOr(m, "type", "llm"))
		if err := kvSet(ctx, tx, "customModels", key, mustJSON(m)); err != nil {
			return fmt.Errorf("insert customModel %q: %w", key, err)
		}
	}
	for tool, mappings := range p.MitmAlias {
		if mappings == nil {
			mappings = []byte("{}")
		}
		if err := kvSet(ctx, tx, "mitmAlias", tool, mappings); err != nil {
			return fmt.Errorf("insert mitmAlias %q: %w", tool, err)
		}
	}
	for provider, models := range p.Pricing {
		if models == nil {
			models = []byte("{}")
		}
		if err := kvSet(ctx, tx, "pricing", provider, models); err != nil {
			return fmt.Errorf("insert pricing %q: %w", provider, err)
		}
	}

	return tx.Commit()
}

// kvSet inserts/replaces a kv row inside an existing transaction.
func kvSet(ctx context.Context, tx *sql.Tx, scope, key string, value []byte) error {
	_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO kv(scope, key, value) VALUES(?, ?, ?)`, scope, key, value)
	return err
}

// kvMap reads a kv scope into a key->RawMessage map.
func kvMap(ctx context.Context, db *sql.DB, scope string) (map[string]json.RawMessage, error) {
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM kv WHERE scope = ?`, scope)
	if err != nil {
		return nil, fmt.Errorf("kv %s: %w", scope, err)
	}
	defer rows.Close()
	m := map[string]json.RawMessage{}
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = json.RawMessage(v)
	}
	return m, rows.Err()
}

// kvList reads a kv scope into a slice of raw JSON values (customModels).
func kvList(ctx context.Context, db *sql.DB, scope string) ([][]byte, error) {
	rows, err := db.QueryContext(ctx, `SELECT value FROM kv WHERE scope = ?`, scope)
	if err != nil {
		return nil, fmt.Errorf("kv %s: %w", scope, err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var v []byte
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// scanRows reads strCols string columns followed by one data blob, applies fn
// to build each row. The caller's fn receives the strCols NullStrings and the
// data bytes.
func scanRows(rows *sql.Rows, fn func(strs []sql.NullString, data []byte) map[string]any, strCols int) ([]map[string]any, error) {
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		strs := make([]sql.NullString, strCols)
		var data []byte
		ptrs := make([]any, 0, strCols+1)
		for i := range strs {
			ptrs = append(ptrs, &strs[i])
		}
		ptrs = append(ptrs, &data)
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, fn(strs, data))
	}
	return out, rows.Err()
}

// scanStringRows reads strCols string columns (no data blob) and applies fn.
func scanStringRows(rows *sql.Rows, fn func(strs []sql.NullString) map[string]any, strCols int) ([]map[string]any, error) {
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		strs := make([]sql.NullString, strCols)
		ptrs := make([]any, 0, strCols)
		for i := range strs {
			ptrs = append(ptrs, &strs[i])
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, fn(strs))
	}
	return out, rows.Err()
}

func parseJSONMap(data []byte) map[string]any {
	m := map[string]any{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

func parseJSONArray(data []byte) []any {
	var a []any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &a)
	}
	return a
}

// mergeKnown sets keys from src into dst if the dst value is missing/empty.
func mergeKnown(dst map[string]any, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}

func stripKeys(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func strOr(m map[string]any, k, def string) string {
	if v, ok := m[k].(string); ok && v != "" {
		return v
	}
	return def
}

func nullAny(m map[string]any, k string) any {
	v, ok := m[k]
	if !ok || v == nil {
		return nil
	}
	if s, ok := v.(string); ok && s == "" {
		return nil
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return v
}

func nullStr(s sql.NullString) any {
	if !s.Valid || s.String == "" {
		return nil
	}
	return s.String
}

func nullInt(s sql.NullString) any {
	if !s.Valid || s.String == "" {
		return nil
	}
	return s.String
}

func intOr(m map[string]any, k string, def int) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case string:
		var n int
		_, _ = fmt.Sscanf(v, "%d", &n)
		if n != 0 {
			return n
		}
	}
	return def
}

func bool01(m map[string]any, k string) int {
	if v, ok := m[k]; ok {
		switch b := v.(type) {
		case bool:
			if !b {
				return 0
			}
		case float64:
			if b == 0 {
				return 0
			}
		}
	}
	return 1
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}