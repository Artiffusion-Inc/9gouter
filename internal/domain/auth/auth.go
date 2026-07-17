// Package auth defines the session, principal, and OIDC ports used for
// dashboard authentication. It ports concepts from src/lib/auth/dashboardSession.js
// and src/lib/auth/oidc.js.
package auth

import (
	"context"
	"net/http"
	"time"
)

// Principal identifies an authenticated user. It is deliberately minimal
// so that adapters can extend it via claims stored in the session.
type Principal struct {
	ID    string `json:"id"`
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

// Session is a signed dashboard session. The ID is the opaque session token
// / cookie value; Principal is what the session represents.
type Session struct {
	ID        string    `json:"id"`
	Principal Principal `json:"principal"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Store is the port for session persistence. The adapter may implement this
// with signed cookies, a KV table, or both.
type Store interface {
	Set(w http.ResponseWriter, s Session) error
	Get(r *http.Request) (*Session, error)
	Clear(w http.ResponseWriter) error
}

// OIDCPort is the port for initiating and completing an OIDC login flow.
// Adapters using golang.org/x/oauth2/oidc implement this.
type OIDCPort interface {
	// StartURL returns the URL to redirect the user to, plus any cookies or
	// state that must be persisted for the callback.
	StartURL(ctx context.Context, redirectURI string) (authURL string, state string, nonce string, codeVerifier string, err error)

	// Exchange validates the returned authorization code and returns the ID token
	// claims for the authenticated principal.
	Exchange(ctx context.Context, code, redirectURI, codeVerifier string) (Principal, error)
}
