package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RequestDetail is the JS requestDetailsRepo record shape.
type RequestDetail struct {
	ID              string          `json:"id"`
	Timestamp       string          `json:"timestamp"`
	Provider        string          `json:"provider,omitempty"`
	Model           string          `json:"model,omitempty"`
	ConnectionID    string          `json:"connectionId,omitempty"`
	Status          string          `json:"status,omitempty"`
	Latency         json.RawMessage `json:"latency,omitempty"`
	Tokens          json.RawMessage `json:"tokens,omitempty"`
	Request         json.RawMessage `json:"request,omitempty"`
	ProviderRequest json.RawMessage `json:"providerRequest,omitempty"`
	ProviderResponse json.RawMessage `json:"providerResponse,omitempty"`
	Response        json.RawMessage `json:"response,omitempty"`
	Pxpipe          json.RawMessage `json:"pxpipe,omitempty"`
}

// RequestDetailRepo persists observability details, ported from requestDetailsRepo.js.
//
// Note: the JS implementation buffers writes in memory and flushes asynchronously.
// This Go port writes directly to SQLite, matching the same SQL shape.
type RequestDetailRepo struct{ db *sql.DB }

func NewRequestDetailRepo(db *sql.DB) *RequestDetailRepo { return &RequestDetailRepo{db: db} }

// Save writes a requestDetails row directly.
func (r *RequestDetailRepo) Save(ctx context.Context, d RequestDetail) error {
	if d.ID == "" {
		return fmt.Errorf("requestDetails save: id is required")
	}
	if d.Timestamp == "" {
		d.Timestamp = formatTime(now())
	}
	data, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("requestDetails marshal: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO requestDetails(id, timestamp, provider, model, connectionId, status, data)
		 VALUES(?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   timestamp = excluded.timestamp, provider = excluded.provider,
		   model = excluded.model, connectionId = excluded.connectionId,
		   status = excluded.status, data = excluded.data`,
		d.ID, d.Timestamp, nullString(d.Provider), nullString(d.Model), nullString(d.ConnectionID), nullString(d.Status), string(data))
	if err != nil {
		return fmt.Errorf("requestDetails save: %w", err)
	}
	return nil
}

type RequestDetailFilter struct {
	Provider     string
	Model        string
	ConnectionID string
	Status       string
	StartDate    string
	EndDate      string
	Page         int
	PageSize     int
}

type RequestDetailPage struct {
	Details    []RequestDetail
	Pagination struct {
		Page       int  `json:"page"`
		PageSize   int  `json:"pageSize"`
		TotalItems int  `json:"totalItems"`
		TotalPages int  `json:"totalPages"`
		HasNext    bool `json:"hasNext"`
		HasPrev    bool `json:"hasPrev"`
	}
}

func (r *RequestDetailRepo) Query(ctx context.Context, f RequestDetailFilter) (RequestDetailPage, error) {
	var out RequestDetailPage
	conds, params := []string{}, []any{}
	if f.Provider != "" {
		conds, params = append(conds, "provider = ?"), append(params, f.Provider)
	}
	if f.Model != "" {
		conds, params = append(conds, "model = ?"), append(params, f.Model)
	}
	if f.ConnectionID != "" {
		conds, params = append(conds, "connectionId = ?"), append(params, f.ConnectionID)
	}
	if f.Status != "" {
		conds, params = append(conds, "status = ?"), append(params, f.Status)
	}
	if f.StartDate != "" {
		conds, params = append(conds, "timestamp >= ?"), append(params, f.StartDate)
	}
	if f.EndDate != "" {
		conds, params = append(conds, "timestamp <= ?"), append(params, f.EndDate)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	page := f.Page
	if page <= 0 {
		page = 1
	}
	pageSize := f.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}

	var total int
	cntRow := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requestDetails`+where, params...)
	if err := cntRow.Scan(&total); err != nil {
		return out, fmt.Errorf("requestDetails count: %w", err)
	}

	offset := (page - 1) * pageSize
	rows, err := r.db.QueryContext(ctx,
		`SELECT data FROM requestDetails`+where+` ORDER BY timestamp DESC LIMIT ? OFFSET ?`,
		append(params, pageSize, offset)...)
	if err != nil {
		return out, fmt.Errorf("requestDetails query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return out, err
		}
		var d RequestDetail
		if err := json.Unmarshal(raw, &d); err != nil {
			continue
		}
		out.Details = append(out.Details, d)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	out.Pagination.Page = page
	out.Pagination.PageSize = pageSize
	out.Pagination.TotalItems = total
	out.Pagination.TotalPages = totalPages
	out.Pagination.HasNext = page < totalPages
	out.Pagination.HasPrev = page > 1
	return out, nil
}

func (r *RequestDetailRepo) GetByID(ctx context.Context, id string) (*RequestDetail, error) {
	row := r.db.QueryRowContext(ctx, `SELECT data FROM requestDetails WHERE id = ?`, id)
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("requestDetails get: %w", err)
	}
	var d RequestDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("requestDetails unmarshal: %w", err)
	}
	return &d, nil
}

func (r *RequestDetailRepo) DistinctProviders(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT provider FROM requestDetails WHERE provider IS NOT NULL ORDER BY provider ASC`)
	if err != nil {
		return nil, fmt.Errorf("requestDetails distinct: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
