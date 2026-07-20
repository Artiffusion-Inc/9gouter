package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// ProxyPoolRepo provides persistence for proxy pools, ported from proxyPoolsRepo.js.
type ProxyPoolRepo struct{ db *sql.DB }

func NewProxyPoolRepo(db *sql.DB) *ProxyPoolRepo { return &ProxyPoolRepo{db: db} }

type ProxyPoolFilter struct {
	IsActive   *bool
	TestStatus string
}

func (r *ProxyPoolRepo) List(ctx context.Context, filter ProxyPoolFilter) ([]settings.ProxyPool, error) {
	where, params := []string{}, []any{}
	if filter.IsActive != nil {
		where, params = append(where, "isActive = ?"), append(params, boolToInt(*filter.IsActive))
	}
	if filter.TestStatus != "" {
		where, params = append(where, "testStatus = ?"), append(params, filter.TestStatus)
	}
	sqlWhere := ""
	if len(where) > 0 {
		sqlWhere = " WHERE " + joinAnd(where)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, isActive, testStatus, data, createdAt, updatedAt FROM proxyPools`+sqlWhere, params...)
	if err != nil {
		return nil, fmt.Errorf("proxyPools list: %w", err)
	}
	defer rows.Close()

	var out []settings.ProxyPool
	for rows.Next() {
		p, err := r.scanPoolRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// JS sorts by updatedAt desc.
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

func (r *ProxyPoolRepo) GetByID(ctx context.Context, id string) (*settings.ProxyPool, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, isActive, testStatus, data, createdAt, updatedAt FROM proxyPools WHERE id = ?`, id)
	return r.scanPool(row)
}

func (r *ProxyPoolRepo) FindByNameAndType(ctx context.Context, name, typ string) (*settings.ProxyPool, error) {
	// JS query uses columns stored inside the JSON data blob.
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, isActive, testStatus, data, createdAt, updatedAt FROM proxyPools WHERE data LIKE ? AND data LIKE ? ORDER BY updatedAt DESC LIMIT 1`,
		`%"name":"`+name+`"%`, `%"type":"`+typ+`"%`)
	if err != nil {
		return nil, fmt.Errorf("proxyPools findByNameAndType: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	p, err := r.scanPoolRow(rows)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *ProxyPoolRepo) Create(ctx context.Context, p settings.ProxyPool) error {
	if p.ID == "" {
		return fmt.Errorf("proxyPools create: id is required")
	}
	now := now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = now
	}
	if p.TestStatus == "" {
		p.TestStatus = "unknown"
	}
	return r.upsert(ctx, p)
}

func (r *ProxyPoolRepo) Update(ctx context.Context, p settings.ProxyPool) error {
	if p.ID == "" {
		return fmt.Errorf("proxyPools update: id is required")
	}
	p.UpdatedAt = now()
	return r.upsert(ctx, p)
}

func (r *ProxyPoolRepo) Delete(ctx context.Context, id string) (*settings.ProxyPool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("proxyPools delete tx: %w", err)
	}
	defer tx.Rollback()

	p, err := r.getTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM proxyPools WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("proxyPools delete: %w", err)
	}
	return p, tx.Commit()
}

func (r *ProxyPoolRepo) upsert(ctx context.Context, p settings.ProxyPool) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO proxyPools(id, isActive, testStatus, data, createdAt, updatedAt)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   isActive=excluded.isActive, testStatus=excluded.testStatus,
		   data=excluded.data, updatedAt=excluded.updatedAt`,
		p.ID, boolToInt(p.IsActive), p.TestStatus, jsonText(p.Data), formatTime(p.CreatedAt), formatTime(p.UpdatedAt))
	if err != nil {
		return fmt.Errorf("proxyPools upsert: %w", err)
	}
	return nil
}

func (r *ProxyPoolRepo) getTx(ctx context.Context, tx *sql.Tx, id string) (*settings.ProxyPool, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, isActive, testStatus, data, createdAt, updatedAt FROM proxyPools WHERE id = ?`, id)
	return r.scanPool(row)
}

func (r *ProxyPoolRepo) scanPool(row *sql.Row) (*settings.ProxyPool, error) {
	var p settings.ProxyPool
	var created, updated string
	var data []byte
	var isActive sql.NullInt64
	var testStatus sql.NullString
	err := row.Scan(&p.ID, &isActive, &testStatus, &data, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("proxyPools scan: %w", err)
	}
	p.IsActive = scanBool(isActive)
	p.TestStatus = testStatus.String
	p.Data = json.RawMessage(data)
	p.CreatedAt, _ = parseTime(created)
	p.UpdatedAt, _ = parseTime(updated)
	return &p, nil
}

func (r *ProxyPoolRepo) scanPoolRow(rows *sql.Rows) (settings.ProxyPool, error) {
	var p settings.ProxyPool
	var created, updated string
	var data []byte
	var isActive sql.NullInt64
	var testStatus sql.NullString
	if err := rows.Scan(&p.ID, &isActive, &testStatus, &data, &created, &updated); err != nil {
		return p, fmt.Errorf("proxyPools scan: %w", err)
	}
	p.IsActive = scanBool(isActive)
	p.TestStatus = testStatus.String
	p.Data = json.RawMessage(data)
	p.CreatedAt, _ = parseTime(created)
	p.UpdatedAt, _ = parseTime(updated)
	return p, nil
}
