package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
)

// NodeRepo provides persistence for provider nodes, ported from nodesRepo.js.
type NodeRepo struct{ db *sql.DB }

func NewNodeRepo(db *sql.DB) *NodeRepo { return &NodeRepo{db: db} }

type NodeFilter struct{ Type string }

func (r *NodeRepo) List(ctx context.Context, filter NodeFilter) ([]settings.ProviderNode, error) {
	where, params := []string{}, []any{}
	if filter.Type != "" {
		where, params = append(where, "type = ?"), append(params, filter.Type)
	}
	sqlWhere := ""
	if len(where) > 0 {
		sqlWhere = " WHERE " + joinAnd(where)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, type, name, data, createdAt, updatedAt FROM providerNodes`+sqlWhere, params...)
	if err != nil {
		return nil, fmt.Errorf("nodes list: %w", err)
	}
	defer rows.Close()

	var out []settings.ProviderNode
	for rows.Next() {
		n, err := r.scanNodeRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (r *NodeRepo) GetByID(ctx context.Context, id string) (*settings.ProviderNode, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, type, name, data, createdAt, updatedAt FROM providerNodes WHERE id = ?`, id)
	return r.scanNode(row)
}

func (r *NodeRepo) Create(ctx context.Context, n settings.ProviderNode) error {
	if n.ID == "" {
		return fmt.Errorf("nodes create: id is required")
	}
	now := now()
	if n.CreatedAt.IsZero() {
		n.CreatedAt = now
	}
	if n.UpdatedAt.IsZero() {
		n.UpdatedAt = now
	}
	return r.upsert(ctx, n)
}

func (r *NodeRepo) Update(ctx context.Context, n settings.ProviderNode) error {
	if n.ID == "" {
		return fmt.Errorf("nodes update: id is required")
	}
	n.UpdatedAt = now()
	return r.upsert(ctx, n)
}

func (r *NodeRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM providerNodes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("nodes delete: %w", err)
	}
	return nil
}

func (r *NodeRepo) upsert(ctx context.Context, n settings.ProviderNode) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO providerNodes(id, type, name, data, createdAt, updatedAt)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   type=excluded.type, name=excluded.name, data=excluded.data, updatedAt=excluded.updatedAt`,
		n.ID, n.Type, n.Name, jsonText(n.Data), formatTime(n.CreatedAt), formatTime(n.UpdatedAt))
	if err != nil {
		return fmt.Errorf("nodes upsert: %w", err)
	}
	return nil
}

func (r *NodeRepo) scanNode(row *sql.Row) (*settings.ProviderNode, error) {
	var n settings.ProviderNode
	var created, updated string
	var data []byte
	var typ, name sql.NullString
	err := row.Scan(&n.ID, &typ, &name, &data, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("nodes scan: %w", err)
	}
	n.Type = typ.String
	n.Name = name.String
	n.Data = json.RawMessage(data)
	n.CreatedAt, _ = parseTime(created)
	n.UpdatedAt, _ = parseTime(updated)
	return &n, nil
}

func (r *NodeRepo) scanNodeRow(rows *sql.Rows) (settings.ProviderNode, error) {
	var n settings.ProviderNode
	var created, updated string
	var data []byte
	var typ, name sql.NullString
	if err := rows.Scan(&n.ID, &typ, &name, &data, &created, &updated); err != nil {
		return n, fmt.Errorf("nodes scan: %w", err)
	}
	n.Type = typ.String
	n.Name = name.String
	n.Data = json.RawMessage(data)
	n.CreatedAt, _ = parseTime(created)
	n.UpdatedAt, _ = parseTime(updated)
	return n, nil
}
