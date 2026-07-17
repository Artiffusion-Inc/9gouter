package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// AliasRepo stores model aliases, custom models, and MITM alias mappings in the
// kv table, ported from aliasRepo.js.
type AliasRepo struct{ db *sql.DB }

func NewAliasRepo(db *sql.DB) *AliasRepo { return &AliasRepo{db: db} }

const (
	scopeAliases     = "modelAliases"
	scopeCustom      = "customModels"
	scopeMitm        = "mitmAlias"
)

// Model aliases.

func (r *AliasRepo) GetAliases(ctx context.Context) (map[string]string, error) {
	return r.getStringMap(ctx, scopeAliases)
}

func (r *AliasRepo) SetAlias(ctx context.Context, alias, model string) error {
	return r.kvSet(ctx, scopeAliases, alias, model)
}

func (r *AliasRepo) DeleteAlias(ctx context.Context, alias string) error {
	return r.kvRemove(ctx, scopeAliases, alias)
}

// Custom models.

type CustomModel struct {
	ProviderAlias string `json:"providerAlias"`
	ID            string `json:"id"`
	Type          string `json:"type"`
	Name          string `json:"name"`
}

func (r *AliasRepo) GetCustomModels(ctx context.Context) ([]CustomModel, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT value FROM kv WHERE scope = ?`, scopeCustom)
	if err != nil {
		return nil, fmt.Errorf("alias getCustomModels: %w", err)
	}
	defer rows.Close()

	var out []CustomModel
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var cm CustomModel
		if err := json.Unmarshal(raw, &cm); err != nil {
			continue
		}
		out = append(out, cm)
	}
	return out, rows.Err()
}

func (r *AliasRepo) AddCustomModel(ctx context.Context, cm CustomModel) (bool, error) {
	if cm.ProviderAlias == "" || cm.ID == "" {
		return false, fmt.Errorf("alias addCustomModel: providerAlias and id are required")
	}
	if cm.Type == "" {
		cm.Type = "llm"
	}
	if cm.Name == "" {
		cm.Name = cm.ID
	}
	key := customModelKey(cm.ProviderAlias, cm.ID, cm.Type)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("alias addCustomModel tx: %w", err)
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM kv WHERE scope = ? AND key = ?`, scopeCustom, key).Scan(&exists); err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if exists == 1 {
		return false, tx.Commit()
	}
	value, err := json.Marshal(cm)
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO kv(scope, key, value) VALUES(?, ?, ?)`, scopeCustom, key, string(value))
	if err != nil {
		return false, fmt.Errorf("alias addCustomModel insert: %w", err)
	}
	return true, tx.Commit()
}

func (r *AliasRepo) DeleteCustomModel(ctx context.Context, providerAlias, id, typ string) error {
	if typ == "" {
		typ = "llm"
	}
	return r.kvRemove(ctx, scopeCustom, customModelKey(providerAlias, id, typ))
}

// MITM aliases.

func (r *AliasRepo) GetMitmAliases(ctx context.Context, toolName string) (map[string]json.RawMessage, error) {
	if toolName != "" {
		row := r.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE scope = ? AND key = ?`, scopeMitm, toolName)
		var raw []byte
		if err := row.Scan(&raw); err != nil {
			if err == sql.ErrNoRows {
				return map[string]json.RawMessage{}, nil
			}
			return nil, fmt.Errorf("alias getMitmAlias: %w", err)
		}
		out := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		return out, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT key, value FROM kv WHERE scope = ?`, scopeMitm)
	if err != nil {
		return nil, fmt.Errorf("alias getMitmAliases: %w", err)
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		out[key] = raw
	}
	return out, rows.Err()
}

func (r *AliasRepo) SetMitmAliases(ctx context.Context, toolName string, mappings json.RawMessage) error {
	if toolName == "" {
		return fmt.Errorf("alias setMitmAliases: toolName is required")
	}
	return r.kvSet(ctx, scopeMitm, toolName, mappings)
}

// KV helpers.

func (r *AliasRepo) getStringMap(ctx context.Context, scope string) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT key, value FROM kv WHERE scope = ?`, scope)
	if err != nil {
		return nil, fmt.Errorf("alias getStringMap: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (r *AliasRepo) kvSet(ctx context.Context, scope, key string, value any) error {
	var bytes []byte
	switch v := value.(type) {
	case string:
		bytes = []byte(v)
	case json.RawMessage:
		bytes = v
	case []byte:
		bytes = v
	default:
		var err error
		bytes, err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO kv(scope, key, value) VALUES(?, ?, ?) ON CONFLICT(scope, key) DO UPDATE SET value = excluded.value`,
		scope, key, bytes)
	if err != nil {
		return fmt.Errorf("alias kvSet: %w", err)
	}
	return nil
}

func (r *AliasRepo) kvRemove(ctx context.Context, scope, key string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM kv WHERE scope = ? AND key = ?`, scope, key)
	if err != nil {
		return fmt.Errorf("alias kvRemove: %w", err)
	}
	return nil
}

func customModelKey(providerAlias, id, typ string) string {
	return strings.Join([]string{providerAlias, id, typ}, "|")
}
