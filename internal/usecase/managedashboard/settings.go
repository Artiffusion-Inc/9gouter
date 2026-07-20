package managedashboard

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// SettingsService exposes settings operations.
type SettingsService struct {
	Repo interface {
		Get(ctx context.Context) (*settings.Settings, error)
		Update(ctx context.Context, updates json.RawMessage) (*settings.Settings, error)
	}
}

// Get returns the merged settings object.
func (s *SettingsService) Get(ctx context.Context) (*settings.Settings, error) {
	return s.Repo.Get(ctx)
}

// Merge updates settings after stripping protected keys and password data.
func (s *SettingsService) Merge(ctx context.Context, body []byte) (*settings.Settings, error) {
	patch := map[string]any{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &patch); err != nil {
			return nil, err
		}
	}
	for _, k := range []string{"password", "mitmSudoEncrypted", "oidcClientSecret"} {
		delete(patch, k)
	}
	if v, ok := patch["newPassword"].(string); ok && v != "" {
		patch["password"] = v
		delete(patch, "newPassword")
		delete(patch, "currentPassword")
	}
	if v, ok := patch["oidcClientSecret"].(string); ok && stringsTrim(v) == "" {
		delete(patch, "oidcClientSecret")
	}
	if len(patch) == 0 {
		return s.Repo.Get(ctx)
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}
	return s.Repo.Update(ctx, raw)
}

// OidcConfigured reports whether OIDC is fully configured.
func (s *SettingsService) OidcConfigured(data json.RawMessage) bool {
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	issuer, _ := m["oidcIssuerUrl"].(string)
	clientID, _ := m["oidcClientId"].(string)
	secret, _ := m["oidcClientSecret"].(string)
	return issuer != "" && clientID != "" && secret != ""
}

// HasPassword reports whether a password hash is stored.
func (s *SettingsService) HasPassword(data json.RawMessage) bool {
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	v, _ := m["password"].(string)
	return v != ""
}

func stringsTrim(v string) string {
	return strings.TrimSpace(v)
}
