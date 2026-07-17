package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// DisabledModelsRepo stores per-provider disabled model lists in the kv table,
// ported from disabledModelsRepo.js.
type DisabledModelsRepo struct{ db *sql.DB }

func NewDisabledModelsRepo(db *sql.DB) *DisabledModelsRepo { return &DisabledModelsRepo{db: db} }

const scopeDisabledModels = "disabledModels"

func (r *DisabledModelsRepo) GetAll(ctx context.Context) (map[string][]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT key, value FROM kv WHERE scope = ?`, scopeDisabledModels)
	if err != nil {
		return nil, fmt.Errorf("disabledModels getAll: %w", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var provider string
		var raw []byte
		if err := rows.Scan(&provider, &raw); err != nil {
			return nil, err
		}
		var ids []string
		if err := json.Unmarshal(raw, &ids); err != nil {
			continue
		}
		out[provider] = ids
	}
	return out, rows.Err()
}

func (r *DisabledModelsRepo) GetByProvider(ctx context.Context, providerAlias string) ([]string, error) {
	if providerAlias == "" {
		return nil, nil
	}
	row := r.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE scope = ? AND key = ?`, scopeDisabledModels, providerAlias)
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return []string{}, nil
		}
		return nil, fmt.Errorf("disabledModels getByProvider: %w", err)
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *DisabledModelsRepo) Disable(ctx context.Context, providerAlias string, ids []string) error {
	if providerAlias == "" || len(ids) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("disabledModels disable tx: %w", err)
	}
	defer tx.Rollback()

	current, err := r.getByProviderTx(ctx, tx, providerAlias)
	if err != nil {
		return err
	}
	merged := append(current, ids...)
	seen := map[string]struct{}{}
	var next []string
	for _, id := range merged {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		next = append(next, id)
	}
	value, err := json.Marshal(next)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO kv(scope, key, value) VALUES(?, ?, ?) ON CONFLICT(scope, key) DO UPDATE SET value = excluded.value`,
		scopeDisabledModels, providerAlias, string(value))
	if err != nil {
		return fmt.Errorf("disabledModels disable write: %w", err)
	}
	return tx.Commit()
}

func (r *DisabledModelsRepo) Enable(ctx context.Context, providerAlias string, ids []string) error {
	if providerAlias == "" {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("disabledModels enable tx: %w", err)
	}
	defer tx.Rollback()

	if len(ids) == 0 {
		_, err = tx.ExecContext(ctx, `DELETE FROM kv WHERE scope = ? AND key = ?`, scopeDisabledModels, providerAlias)
		if err != nil {
			return err
		}
		return tx.Commit()
	}

	current, err := r.getByProviderTx(ctx, tx, providerAlias)
	if err != nil {
		return err
	}
	remove := map[string]struct{}{}
	for _, id := range ids {
		remove[id] = struct{}{}
	}
	var next []string
	for _, id := range current {
		if _, ok := remove[id]; ok {
			continue
		}
		next = append(next, id)
	}
	if len(next) == 0 {
		_, err = tx.ExecContext(ctx, `DELETE FROM kv WHERE scope = ? AND key = ?`, scopeDisabledModels, providerAlias)
	} else {
		var value []byte
		value, err = json.Marshal(next)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO kv(scope, key, value) VALUES(?, ?, ?) ON CONFLICT(scope, key) DO UPDATE SET value = excluded.value`,
			scopeDisabledModels, providerAlias, string(value))
	}
	if err != nil {
		return fmt.Errorf("disabledModels enable write: %w", err)
	}
	return tx.Commit()
}

func (r *DisabledModelsRepo) getByProviderTx(ctx context.Context, tx *sql.Tx, providerAlias string) ([]string, error) {
	row := tx.QueryRowContext(ctx, `SELECT value FROM kv WHERE scope = ? AND key = ?`, scopeDisabledModels, providerAlias)
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return []string{}, nil
		}
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, err
	}
	return ids, nil
}
