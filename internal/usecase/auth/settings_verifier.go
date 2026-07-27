package auth

import (
	"context"
	"encoding/json"
	"errors"
)

// SettingsJSONReader is the minimal settings-repo surface the settings bcrypt
// verifier needs: the merged settings JSON (defaults applied) so it can read
// the stored "password" bcrypt hash. Implemented by a thin adapter in the
// transport/api package; the usecase stays free of db/repo imports.
type SettingsJSONReader interface {
	// SettingsJSON returns the merged settings JSON or an error if the row
	// cannot be read.
	SettingsJSON(ctx context.Context) ([]byte, error)
}

// SettingsBcryptVerifier is the primary login verifier. It mirrors the legacy
// dashboardSession.js behaviour: the password hash a user sets through the UI
// is persisted under settings.password (bcrypt) and must be honoured on login.
//
// The previous Go wiring read only the DASHBOARD_PASSWORD_HASH env var (or the
// hardcoded "123456" fallback), so any password changed via the UI was silently
// rejected with 401 — the dashboard looked logged-out for every /api/* call
// even though the static page rendered.
//
// Resolution order on each Verify (so UI password changes take effect without a
// restart):
//
//  1. settings.password (bcrypt) — if set, compared with Comparator.
//  2. Fallback verifier — env DASHBOARD_PASSWORD_HASH (bcrypt) or the
//     initial-password plain verifier, used only when no settings password is
//     stored (fresh install / password reset to default).
//
// When a settings password is set but does not match, Verify does NOT fall
// through to Fallback — that would let the initial password bypass a
// user-configured password.
type SettingsBcryptVerifier struct {
	Reader     SettingsJSONReader
	Comparator BcryptFunc
	Fallback   PasswordVerifier
}

// Verify implements PasswordVerifier.
func (v *SettingsBcryptVerifier) Verify(password string) error {
	if v.Reader != nil {
		raw, err := v.Reader.SettingsJSON(context.Background())
		if err == nil && len(raw) > 0 {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err == nil {
				if hash, _ := m["password"].(string); hash != "" {
					cmp := v.Comparator
					if cmp == nil {
						return errors.New("auth: bcrypt comparator not configured")
					}
					if cmp(password, hash) == nil {
						return nil
					}
					return ErrUnauthorized
				}
			}
		}
	}
	if v.Fallback != nil {
		return v.Fallback.Verify(password)
	}
	return ErrUnauthorized
}
