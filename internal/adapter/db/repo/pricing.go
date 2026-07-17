package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// PricingRepo stores user pricing overrides in the kv table, ported from
// pricingRepo.js.
//
// Note: the JS getPricing/getPricingForModel functions merge these overrides
// with a hard-coded PROVIDER_PRICING constant defined in open-sse/providers/pricing.js.
// That constant is not part of the SQLite repository and is omitted here.
type PricingRepo struct{ db *sql.DB }

func NewPricingRepo(db *sql.DB) *PricingRepo { return &PricingRepo{db: db} }

func (r *PricingRepo) GetAll(ctx context.Context) (map[string]map[string]json.RawMessage, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT key, value FROM kv WHERE scope = 'pricing'`)
	if err != nil {
		return nil, fmt.Errorf("pricing getAll: %w", err)
	}
	defer rows.Close()

	out := map[string]map[string]json.RawMessage{}
	for rows.Next() {
		var provider string
		var raw []byte
		if err := rows.Scan(&provider, &raw); err != nil {
			return nil, err
		}
		models := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &models); err != nil {
			continue
		}
		out[provider] = models
	}
	return out, rows.Err()
}

func (r *PricingRepo) GetForModel(ctx context.Context, provider, model string) (json.RawMessage, error) {
	if model == "" {
		return nil, nil
	}
	all, err := r.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	if m, ok := all[provider]; ok {
		if v, ok := m[model]; ok {
			return v, nil
		}
	}
	return nil, nil
}

func (r *PricingRepo) Update(ctx context.Context, pricingData map[string]map[string]json.RawMessage) (map[string]map[string]json.RawMessage, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("pricing update tx: %w", err)
	}
	defer tx.Rollback()

	for provider, models := range pricingData {
		row := tx.QueryRowContext(ctx, `SELECT value FROM kv WHERE scope = 'pricing' AND key = ?`, provider)
		var raw []byte
		if err := row.Scan(&raw); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		current := map[string]json.RawMessage{}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &current)
		}
		for model, price := range models {
			current[model] = price
		}
		next, err := json.Marshal(current)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO kv(scope, key, value) VALUES('pricing', ?, ?) ON CONFLICT(scope, key) DO UPDATE SET value = excluded.value`,
			provider, string(next))
		if err != nil {
			return nil, fmt.Errorf("pricing update write: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r.GetAll(ctx)
}

func (r *PricingRepo) Reset(ctx context.Context, provider, model string) (map[string]map[string]json.RawMessage, error) {
	if provider == "" {
		return r.GetAll(ctx)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("pricing reset tx: %w", err)
	}
	defer tx.Rollback()

	if model == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM kv WHERE scope = 'pricing' AND key = ?`, provider)
		if err != nil {
			return nil, err
		}
	} else {
		row := tx.QueryRowContext(ctx, `SELECT value FROM kv WHERE scope = 'pricing' AND key = ?`, provider)
		var raw []byte
		if err := row.Scan(&raw); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		current := map[string]json.RawMessage{}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &current)
		}
		delete(current, model)
		if len(current) == 0 {
			_, err = tx.ExecContext(ctx, `DELETE FROM kv WHERE scope = 'pricing' AND key = ?`, provider)
		} else {
			next, err := json.Marshal(current)
			if err != nil {
				return nil, err
			}
			_, err = tx.ExecContext(ctx,
				`INSERT INTO kv(scope, key, value) VALUES('pricing', ?, ?) ON CONFLICT(scope, key) DO UPDATE SET value = excluded.value`,
				provider, next)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r.GetAll(ctx)
}

func (r *PricingRepo) ResetAll(ctx context.Context) (map[string]map[string]json.RawMessage, error) {
	_, err := r.db.ExecContext(ctx, `DELETE FROM kv WHERE scope = 'pricing'`)
	if err != nil {
		return nil, fmt.Errorf("pricing resetAll: %w", err)
	}
	return map[string]map[string]json.RawMessage{}, nil
}

