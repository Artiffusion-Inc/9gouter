package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
)

// ConnectionRepo provides persistence for provider connections, ported from
// connectionsRepo.js.
type ConnectionRepo struct{ db *sql.DB }

func NewConnectionRepo(db *sql.DB) *ConnectionRepo { return &ConnectionRepo{db: db} }

type ConnectionFilter struct {
	Provider string
	IsActive *bool
}

func (r *ConnectionRepo) List(ctx context.Context, filter ConnectionFilter) ([]settings.ProviderConnection, error) {
	where, params := []string{}, []any{}
	if filter.Provider != "" {
		where, params = append(where, "provider = ?"), append(params, filter.Provider)
	}
	if filter.IsActive != nil {
		where, params = append(where, "isActive = ?"), append(params, boolToInt(*filter.IsActive))
	}
	sqlWhere := ""
	if len(where) > 0 {
		sqlWhere = " WHERE " + joinAnd(where)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, provider, authType, name, email, priority, isActive, data, createdAt, updatedAt FROM providerConnections`+sqlWhere, params...)
	if err != nil {
		return nil, fmt.Errorf("connections list: %w", err)
	}
	defer rows.Close()

	var out []settings.ProviderConnection
	for rows.Next() {
		c, err := r.scanConnectionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// JS sorts by priority ascending, with nulls last (default 999).
	sort.Slice(out, func(i, j int) bool {
		pi, pj := out[i].Priority, out[j].Priority
		if pi == pj {
			return out[i].UpdatedAt.Before(out[j].UpdatedAt)
		}
		if pi == 0 {
			return false
		}
		if pj == 0 {
			return true
		}
		return pi < pj
	})
	return out, nil
}

func (r *ConnectionRepo) GetByID(ctx context.Context, id string) (*settings.ProviderConnection, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, provider, authType, name, email, priority, isActive, data, createdAt, updatedAt FROM providerConnections WHERE id = ?`, id)
	c, err := r.scanConnection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// Create inserts a new connection. It does NOT perform the JS OAuth/API-key
// deduplication or priority auto-assignment; callers should supply a complete
// record. The reorder-on-create behavior is preserved.
func (r *ConnectionRepo) Create(ctx context.Context, c settings.ProviderConnection) error {
	if c.ID == "" || c.Provider == "" || c.AuthType == "" {
		return fmt.Errorf("connections create: id, provider and authType are required")
	}
	now := now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = now
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("connections create tx: %w", err)
	}
	defer tx.Rollback()

	if err := r.upsertTx(ctx, tx, c); err != nil {
		return err
	}
	if err := r.reorderTx(ctx, tx, c.Provider); err != nil {
		return err
	}
	return tx.Commit()
}

// Update merges data into an existing connection and reorders priorities when
// the priority field is changed.
func (r *ConnectionRepo) Update(ctx context.Context, c settings.ProviderConnection) error {
	if c.ID == "" {
		return fmt.Errorf("connections update: id is required")
	}
	c.UpdatedAt = now()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("connections update tx: %w", err)
	}
	defer tx.Rollback()

	existing, err := r.getTx(ctx, tx, c.ID)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("connections update: not found")
	}
	if err := r.upsertTx(ctx, tx, c); err != nil {
		return err
	}
	if c.Priority != existing.Priority {
		if err := r.reorderTx(ctx, tx, existing.Provider); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *ConnectionRepo) Delete(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("connections delete tx: %w", err)
	}
	defer tx.Rollback()

	var provider string
	if err := tx.QueryRowContext(ctx, `SELECT provider FROM providerConnections WHERE id = ?`, id).Scan(&provider); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return tx.Commit()
		}
		return fmt.Errorf("connections delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM providerConnections WHERE id = ?`, id); err != nil {
		return fmt.Errorf("connections delete: %w", err)
	}
	if err := r.reorderTx(ctx, tx, provider); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *ConnectionRepo) DeleteByProvider(ctx context.Context, provider string) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM providerConnections WHERE provider = ?`, provider)
	if err != nil {
		return 0, fmt.Errorf("connections deleteByProvider: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (r *ConnectionRepo) Reorder(ctx context.Context, provider string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("connections reorder tx: %w", err)
	}
	defer tx.Rollback()
	if err := r.reorderTx(ctx, tx, provider); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateUsageMeta merges lastUsedAt and consecutiveUseCount into a
// connection's JSON data blob without triggering a priority reorder. This is
// the persistence side of sticky round-robin selection (decolua/9router #2703
// Fix 4): every selection writes back the timestamp + use count so the next
// request can decide stay-vs-rotate. It is a read-modify-write over `data`
// because lastUsedAt/consecutiveUseCount live inside the JSON blob alongside
// the other optional fields, not as top-level columns.
func (r *ConnectionRepo) UpdateUsageMeta(ctx context.Context, id string, lastUsedAt time.Time, consecutiveUseCount int) error {
	row := r.db.QueryRowContext(ctx, `SELECT data FROM providerConnections WHERE id = ?`, id)
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("connections updateUsageMeta: not found")
		}
		return fmt.Errorf("connections updateUsageMeta read: %w", err)
	}
	data := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("connections updateUsageMeta parse: %w", err)
		}
	}
	data["lastUsedAt"] = lastUsedAt.UTC().Format(time.RFC3339Nano)
	data["consecutiveUseCount"] = consecutiveUseCount
	next, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("connections updateUsageMeta marshal: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`UPDATE providerConnections SET data = ?, updatedAt = ? WHERE id = ?`,
		string(next), formatTime(now()), id)
	if err != nil {
		return fmt.Errorf("connections updateUsageMeta write: %w", err)
	}
	return nil
}

// Cleanup removes null optional fields from every connection's JSON data blob.
// It matches the JS cleanupProviderConnections query behavior.
func (r *ConnectionRepo) Cleanup(ctx context.Context) (int64, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, data FROM providerConnections`)
	if err != nil {
		return 0, fmt.Errorf("connections cleanup: %w", err)
	}
	defer rows.Close()

	var changed int64
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return 0, err
		}
		data := map[string]any{}
		if err := json.Unmarshal(raw, &data); err != nil {
			continue
		}
		dirty := false
		for _, f := range connectionOptionalFields {
			if v, ok := data[f]; ok && v == nil {
				delete(data, f)
				dirty = true
			}
		}
		if psd, ok := data["providerSpecificData"].(map[string]any); ok && len(psd) == 0 {
			delete(data, "providerSpecificData")
			dirty = true
		}
		if !dirty {
			continue
		}
		next, err := json.Marshal(data)
		if err != nil {
			continue
		}
		if _, err := r.db.ExecContext(ctx, `UPDATE providerConnections SET data = ? WHERE id = ?`, string(next), id); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, rows.Err()
}

var connectionOptionalFields = []string{
	"displayName", "email", "globalPriority", "defaultModel",
	"accessToken", "refreshToken", "expiresAt", "tokenType",
	"scope", "projectId", "apiKey", "testStatus",
	"lastTested", "lastError", "lastErrorAt", "rateLimitedUntil", "expiresIn", "errorCode",
	"consecutiveUseCount", "lastUsedAt", "idToken", "lastRefreshAt",
}

func (r *ConnectionRepo) upsertTx(ctx context.Context, tx *sql.Tx, c settings.ProviderConnection) error {
	data := jsonText(c.Data)
	_, err := tx.ExecContext(ctx,
		`INSERT INTO providerConnections(id, provider, authType, name, email, priority, isActive, data, createdAt, updatedAt)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   provider=excluded.provider, authType=excluded.authType, name=excluded.name,
		   email=excluded.email, priority=excluded.priority, isActive=excluded.isActive,
		   data=excluded.data, updatedAt=excluded.updatedAt`,
		c.ID, c.Provider, c.AuthType, c.Name, c.Email, c.Priority, boolToInt(c.IsActive), data, formatTime(c.CreatedAt), formatTime(c.UpdatedAt))
	if err != nil {
		return fmt.Errorf("connections upsert: %w", err)
	}
	return nil
}

func (r *ConnectionRepo) getTx(ctx context.Context, tx *sql.Tx, id string) (*settings.ProviderConnection, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, provider, authType, name, email, priority, isActive, data, createdAt, updatedAt FROM providerConnections WHERE id = ?`, id)
	return r.scanConnection(row)
}

func (r *ConnectionRepo) reorderTx(ctx context.Context, tx *sql.Tx, provider string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, priority, updatedAt FROM providerConnections WHERE provider = ?`, provider)
	if err != nil {
		return fmt.Errorf("connections reorder: %w", err)
	}
	defer rows.Close()
	type item struct {
		id       string
		priority int
		updated  time.Time
	}
	var items []item
	for rows.Next() {
		var it item
		var updated string
		if err := rows.Scan(&it.id, &it.priority, &updated); err != nil {
			return err
		}
		it.updated, _ = parseTime(updated)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// JS reorder: priority asc, then updatedAt desc, then assign 1..N.
	sort.Slice(items, func(i, j int) bool {
		if items[i].priority != items[j].priority {
			if items[i].priority == 0 {
				return false
			}
			if items[j].priority == 0 {
				return true
			}
			return items[i].priority < items[j].priority
		}
		return items[i].updated.After(items[j].updated)
	})
	for i, it := range items {
		if _, err := tx.ExecContext(ctx, `UPDATE providerConnections SET priority = ? WHERE id = ?`, i+1, it.id); err != nil {
			return fmt.Errorf("connections reorder update: %w", err)
		}
	}
	return nil
}

func (r *ConnectionRepo) scanConnection(row *sql.Row) (*settings.ProviderConnection, error) {
	var c settings.ProviderConnection
	var created, updated string
	var data []byte
	var priority sql.NullInt64
	var name, email sql.NullString
	var isActive sql.NullInt64
	err := row.Scan(&c.ID, &c.Provider, &c.AuthType, &name, &email, &priority, &isActive, &data, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("connections scan: %w", err)
	}
	c.Name = name.String
	c.Email = email.String
	c.Priority = int(priority.Int64)
	c.IsActive = scanBool(isActive)
	c.Data = json.RawMessage(data)
	c.CreatedAt, _ = parseTime(created)
	c.UpdatedAt, _ = parseTime(updated)
	return &c, nil
}

func (r *ConnectionRepo) scanConnectionRow(rows *sql.Rows) (settings.ProviderConnection, error) {
	var c settings.ProviderConnection
	var created, updated string
	var data []byte
	var priority sql.NullInt64
	var name, email sql.NullString
	var isActive sql.NullInt64
	if err := rows.Scan(&c.ID, &c.Provider, &c.AuthType, &name, &email, &priority, &isActive, &data, &created, &updated); err != nil {
		return c, fmt.Errorf("connections scan: %w", err)
	}
	c.Name = name.String
	c.Email = email.String
	c.Priority = int(priority.Int64)
	c.IsActive = scanBool(isActive)
	c.Data = json.RawMessage(data)
	c.CreatedAt, _ = parseTime(created)
	c.UpdatedAt, _ = parseTime(updated)
	return c, nil
}
