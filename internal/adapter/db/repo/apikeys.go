package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// APIKeyRepo provides persistence for API keys, ported from apiKeysRepo.js.
type APIKeyRepo struct{ db *sql.DB }

func NewAPIKeyRepo(db *sql.DB) *APIKeyRepo { return &APIKeyRepo{db: db} }

func (r *APIKeyRepo) List(ctx context.Context) ([]settings.APIKey, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, key, name, machineId, isActive, createdAt FROM apiKeys ORDER BY createdAt ASC`)
	if err != nil {
		return nil, fmt.Errorf("apiKeys list: %w", err)
	}
	defer rows.Close()

	var out []settings.APIKey
	for rows.Next() {
		var k settings.APIKey
		var created string
		var isActive sql.NullInt64
		if err := rows.Scan(&k.ID, &k.Key, &k.Name, &k.MachineID, &isActive, &created); err != nil {
			return nil, fmt.Errorf("apiKeys scan: %w", err)
		}
		k.IsActive = scanBool(isActive)
		k.CreatedAt, _ = parseTime(created)
		out = append(out, k)
	}
	return out, rows.Err()
}

func (r *APIKeyRepo) GetByID(ctx context.Context, id string) (*settings.APIKey, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, key, name, machineId, isActive, createdAt FROM apiKeys WHERE id = ?`, id)
	return r.scanKey(row)
}

func (r *APIKeyRepo) GetByKey(ctx context.Context, key string) (*settings.APIKey, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, key, name, machineId, isActive, createdAt FROM apiKeys WHERE key = ?`, key)
	return r.scanKey(row)
}

func (r *APIKeyRepo) scanKey(row *sql.Row) (*settings.APIKey, error) {
	var k settings.APIKey
	var created string
	var isActive sql.NullInt64
	err := row.Scan(&k.ID, &k.Key, &k.Name, &k.MachineID, &isActive, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("apiKeys scan: %w", err)
	}
	k.IsActive = scanBool(isActive)
	k.CreatedAt, _ = parseTime(created)
	return &k, nil
}

func (r *APIKeyRepo) Create(ctx context.Context, k settings.APIKey) error {
	if k.ID == "" || k.Key == "" {
		return fmt.Errorf("apiKeys create: id and key are required")
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = now()
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO apiKeys(id, key, name, machineId, isActive, createdAt) VALUES(?, ?, ?, ?, ?, ?)`,
		k.ID, k.Key, k.Name, k.MachineID, boolToInt(k.IsActive), formatTime(k.CreatedAt))
	if err != nil {
		return fmt.Errorf("apiKeys create: %w", err)
	}
	return nil
}

func (r *APIKeyRepo) Update(ctx context.Context, k settings.APIKey) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE apiKeys SET key = ?, name = ?, machineId = ?, isActive = ? WHERE id = ?`,
		k.Key, k.Name, k.MachineID, boolToInt(k.IsActive), k.ID)
	if err != nil {
		return fmt.Errorf("apiKeys update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("apiKeys update: not found")
	}
	return nil
}

func (r *APIKeyRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM apiKeys WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("apiKeys delete: %w", err)
	}
	return nil
}

func (r *APIKeyRepo) SetActive(ctx context.Context, id string, active bool) error {
	_, err := r.db.ExecContext(ctx, `UPDATE apiKeys SET isActive = ? WHERE id = ?`, boolToInt(active), id)
	if err != nil {
		return fmt.Errorf("apiKeys setActive: %w", err)
	}
	return nil
}

func (r *APIKeyRepo) Validate(ctx context.Context, key string) (bool, error) {
	row := r.db.QueryRowContext(ctx, `SELECT isActive FROM apiKeys WHERE key = ?`, key)
	var v sql.NullInt64
	if err := row.Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("apiKeys validate: %w", err)
	}
	return scanBool(v), nil
}
