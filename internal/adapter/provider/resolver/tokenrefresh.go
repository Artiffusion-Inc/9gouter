package resolver

import (
	"context"
	"errors"
)

// stubTokenRefresher is a TokenRefresher that always fails. It exists so the
// kiro resolver compiles and follows the fallback path (static catalog) when
// a 401 occurs, until the real tokenRefresh subsystem (T027) is ported. When
// T027 lands, replace the kiro resolver's refresher field with the real
// implementation; no call-site changes are needed.
type stubTokenRefresher struct{}

// ErrTokenRefreshNotPorted is returned by the stub refresher. Callers log it
// and fall back to the static catalog.
var ErrTokenRefreshNotPorted = errors.New("token refresh not yet ported (T027)")

// StubTokenRefresher is the placeholder TokenRefresher until T027 lands.
func StubTokenRefresher() TokenRefresher { return stubTokenRefresher{} }

func (stubTokenRefresher) Refresh(ctx context.Context, refreshToken string, psd map[string]any, log Logger) (*RefreshedCredentials, error) {
	return nil, ErrTokenRefreshNotPorted
}