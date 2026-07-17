package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Artiffusion-Inc/9router/internal/domain/settings"
)

// SettingsRepo persists the single-row settings object, ported from settingsRepo.js.
type SettingsRepo struct{ db *sql.DB }

func NewSettingsRepo(db *sql.DB) *SettingsRepo { return &SettingsRepo{db: db} }

var defaultSettings = map[string]any{
	"cloudEnabled":                    false,
	"tunnelEnabled":                   false,
	"tunnelUrl":                       "",
	"tunnelProvider":                  "cloudflare",
	"tailscaleEnabled":                false,
	"tailscaleUrl":                    "",
	"stickyRoundRobinLimit":           3,
	"providerStrategies":              map[string]any{},
	"quotaVisibility":                 map[string]any{},
	"comboStrategy":                   "fallback",
	"comboStickyRoundRobinLimit":      1,
	"comboStrategies":                 map[string]any{},
	"requireLogin":                    true,
	"tunnelDashboardAccess":           true,
	"authMode":                        "password",
	"oidcIssuerUrl":                   "",
	"oidcClientId":                    "",
	"oidcClientSecret":                "",
	"oidcScopes":                      "openid profile email",
	"oidcLoginLabel":                  "Sign in with OIDC",
	"enableObservability":             true,
	"observabilityMaxRecords":         1000,
	"observabilityBatchSize":          20,
	"observabilityFlushIntervalMs":    5000,
	"observabilityMaxJsonSize":        5,
	"outboundProxyEnabled":            false,
	"outboundProxyUrl":                "",
	"outboundNoProxy":                 "",
	"mitmRouterBaseUrl":               "http://localhost:20128",
	"dnsToolEnabled":                  map[string]any{},
	"rtkEnabled":                      true,
	"headroomEnabled":                 false,
	"headroomUrl":                     "http://localhost:8787",
	"headroomCompressUserMessages":    false,
	"cavemanEnabled":                  false,
	"cavemanLevel":                    "full",
	"ponytailEnabled":                 false,
	"ponytailLevel":                   "full",
	"pxpipeEnabled":                   false,
	"pxpipeAutoInstall":               true,
	"pxpipeMinChars":                  25000,
	"pxpipeTimeoutMs":                 15000,
}

func (r *SettingsRepo) Get(ctx context.Context) (*settings.Settings, error) {
	raw, err := r.readRaw(ctx)
	if err != nil {
		return nil, err
	}
	merged := r.mergeWithDefaults(raw)
	data, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("settings marshal: %w", err)
	}
	return &settings.Settings{ID: 1, Data: json.RawMessage(data)}, nil
}

func (r *SettingsRepo) Export(ctx context.Context) (json.RawMessage, error) {
	raw, err := r.readRaw(ctx)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return []byte("{}"), nil
	}
	return raw, nil
}

// Update atomically merges updates into the stored settings row and returns
// the merged result with defaults applied.
func (r *SettingsRepo) Update(ctx context.Context, updates json.RawMessage) (*settings.Settings, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("settings update tx: %w", err)
	}
	defer tx.Rollback()

	current := map[string]any{}
	row := tx.QueryRowContext(ctx, `SELECT data FROM settings WHERE id = 1`)
	var raw []byte
	if err := row.Scan(&raw); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("settings update read: %w", err)
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &current)
	}

	patch := map[string]any{}
	if len(updates) > 0 {
		if err := json.Unmarshal(updates, &patch); err != nil {
			return nil, fmt.Errorf("settings update parse: %w", err)
		}
	}
	for k, v := range patch {
		current[k] = v
	}

	next, err := json.Marshal(current)
	if err != nil {
		return nil, fmt.Errorf("settings update marshal: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO settings(id, data) VALUES(1, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data`,
		string(next))
	if err != nil {
		return nil, fmt.Errorf("settings update write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	merged := r.mergeWithDefaults(json.RawMessage(next))
	mergedBytes, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return &settings.Settings{ID: 1, Data: json.RawMessage(mergedBytes)}, nil
}

func (r *SettingsRepo) IsCloudEnabled(ctx context.Context) (bool, error) {
	s, err := r.Get(ctx)
	if err != nil {
		return false, err
	}
	var m map[string]any
	if err := json.Unmarshal(s.Data, &m); err != nil {
		return false, err
	}
	v, _ := m["cloudEnabled"].(bool)
	return v, nil
}

func (r *SettingsRepo) GetCloudURL(ctx context.Context) string {
	s, err := r.Get(ctx)
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(s.Data, &m); err != nil {
		return ""
	}
	for _, k := range []string{"cloudUrl", "CLOUD_URL", "NEXT_PUBLIC_CLOUD_URL"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func (r *SettingsRepo) readRaw(ctx context.Context) (json.RawMessage, error) {
	row := r.db.QueryRowContext(ctx, `SELECT data FROM settings WHERE id = 1`)
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("settings readRaw: %w", err)
	}
	return json.RawMessage(raw), nil
}

func (r *SettingsRepo) mergeWithDefaults(raw json.RawMessage) map[string]any {
	parsed := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &parsed)
	}
	merged := map[string]any{}
	for k, v := range defaultSettings {
		merged[k] = v
	}
	for k, v := range parsed {
		merged[k] = v
	}
	// JS special case: outboundProxyEnabled inferred from outboundProxyUrl.
	if merged["outboundProxyEnabled"] == false {
		if s, ok := merged["outboundProxyUrl"].(string); ok && s != "" {
			merged["outboundProxyEnabled"] = true
		}
	}
	return merged
}
