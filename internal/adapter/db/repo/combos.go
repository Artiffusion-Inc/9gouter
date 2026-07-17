package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
)

// ComboRepo provides persistence for model combos, ported from combosRepo.js.
type ComboRepo struct{ db *sql.DB }

func NewComboRepo(db *sql.DB) *ComboRepo { return &ComboRepo{db: db} }

func (r *ComboRepo) List(ctx context.Context) ([]settings.Combo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, name, kind, models, createdAt, updatedAt FROM combos ORDER BY createdAt ASC`)
	if err != nil {
		return nil, fmt.Errorf("combos list: %w", err)
	}
	defer rows.Close()

	var out []settings.Combo
	for rows.Next() {
		var c settings.Combo
		var created, updated string
		var models []byte
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &models, &created, &updated); err != nil {
			return nil, fmt.Errorf("combos scan: %w", err)
		}
		c.Models = json.RawMessage(models)
		c.CreatedAt, _ = parseTime(created)
		c.UpdatedAt, _ = parseTime(updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *ComboRepo) GetByID(ctx context.Context, id string) (*settings.Combo, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, name, kind, models, createdAt, updatedAt FROM combos WHERE id = ?`, id)
	return r.scanCombo(row)
}

func (r *ComboRepo) GetByName(ctx context.Context, name string) (*settings.Combo, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, name, kind, models, createdAt, updatedAt FROM combos WHERE name = ?`, name)
	return r.scanCombo(row)
}

func (r *ComboRepo) scanCombo(row *sql.Row) (*settings.Combo, error) {
	var c settings.Combo
	var created, updated string
	var models []byte
	err := row.Scan(&c.ID, &c.Name, &c.Kind, &models, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("combos scan: %w", err)
	}
	c.Models = json.RawMessage(models)
	c.CreatedAt, _ = parseTime(created)
	c.UpdatedAt, _ = parseTime(updated)
	return &c, nil
}

func (r *ComboRepo) Create(ctx context.Context, c settings.Combo) error {
	if c.ID == "" || c.Name == "" {
		return fmt.Errorf("combos create: id and name are required")
	}
	now := now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = now
	}
	if len(c.Models) == 0 {
		c.Models = []byte("[]")
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO combos(id, name, kind, models, createdAt, updatedAt) VALUES(?, ?, ?, ?, ?, ?)`,
		c.ID, c.Name, c.Kind, jsonText(c.Models), formatTime(c.CreatedAt), formatTime(c.UpdatedAt))
	if err != nil {
		return fmt.Errorf("combos create: %w", err)
	}
	return nil
}

func (r *ComboRepo) Update(ctx context.Context, c settings.Combo) error {
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = now()
	}
	if len(c.Models) == 0 {
		c.Models = []byte("[]")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE combos SET name = ?, kind = ?, models = ?, updatedAt = ? WHERE id = ?`,
		c.Name, c.Kind, jsonText(c.Models), formatTime(c.UpdatedAt), c.ID)
	if err != nil {
		return fmt.Errorf("combos update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("combos update: not found")
	}
	return nil
}

func (r *ComboRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM combos WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("combos delete: %w", err)
	}
	return nil
}
