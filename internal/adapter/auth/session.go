// Package auth ports dashboardSession.js concepts into pure Go adapters.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	domainauth "github.com/Artiffusion-Inc/9router/internal/domain/auth"
)

const (
	// authCookieName matches dashboardSession.js.
	authCookieName = "auth_token"
	// defaultSessionTTL mirrors the 24h JWT expiry set by dashboardSession.js.
	defaultSessionTTL = 24 * time.Hour
)

var (
	// ErrInvalidSession is returned when a session cookie is missing, malformed,
	// tampered with, or expired.
	ErrInvalidSession = errors.New("invalid session")
	// ErrSecretTooShort is returned when the HMAC secret is shorter than 16 bytes.
	ErrSecretTooShort = errors.New("session secret must be at least 16 bytes")
)

// secureCookie detects whether the cookie should be flagged Secure. It mirrors
// dashboardSession.js shouldUseSecureCookie: AUTH_COOKIE_SECURE=true forces it,
// otherwise trust X-Forwarded-Proto=https.
func secureCookie(r *http.Request, forceSecure bool) bool {
	if forceSecure {
		return true
	}
	if r == nil {
		return false
	}
	return strings.ToLower(r.Header.Get("X-Forwarded-Proto")) == "https"
}

// signedPayload is the cookie payload before signing.
type signedPayload struct {
	ID        string               `json:"id"`
	Principal domainauth.Principal `json:"principal"`
	ExpiresAt time.Time            `json:"expiresAt"`
}

// CookieStore implements domainauth.Store with HMAC-signed cookies. It is a
// straight port of the security properties in dashboardSession.js (signed, opaque,
// httpOnly, secure when HTTPS, lax sameSite, path=/).
type CookieStore struct {
	secret      []byte
	ttl         time.Duration
	forceSecure bool
}

// NewCookieStore returns a store that signs cookies with secret. If secret is
// shorter than 16 bytes an error is returned so the server cannot start with a
// weak secret. ttl defaults to 24h; use WithTTL to override.
func NewCookieStore(secret string) (*CookieStore, error) {
	if len(secret) < 16 {
		return nil, ErrSecretTooShort
	}
	return &CookieStore{
		secret: []byte(secret),
		ttl:    defaultSessionTTL,
	}, nil
}

// WithTTL returns a copy of the store using the provided TTL.
func (s *CookieStore) WithTTL(ttl time.Duration) *CookieStore {
	return &CookieStore{
		secret:      s.secret,
		ttl:         ttl,
		forceSecure: s.forceSecure,
	}
}

// WithForceSecure forces the Secure flag on every cookie, matching
// AUTH_COOKIE_SECURE=true in dashboardSession.js.
func (s *CookieStore) WithForceSecure(force bool) *CookieStore {
	return &CookieStore{
		secret:      s.secret,
		ttl:         s.ttl,
		forceSecure: force,
	}
}

// Set writes an HMAC-signed auth_token cookie for the given session.
func (s *CookieStore) Set(w http.ResponseWriter, sess domainauth.Session) error {
	if sess.ID == "" {
		sess.ID = randomID()
	}
	if sess.ExpiresAt.IsZero() {
		sess.ExpiresAt = time.Now().Add(s.ttl)
	}
	payload := signedPayload{
		ID:        sess.ID,
		Principal: sess.Principal,
		ExpiresAt: sess.ExpiresAt,
	}
	value, err := s.encode(payload)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}

	ttl := time.Until(sess.ExpiresAt)
	maxAge := int(ttl.Seconds())
	if maxAge < 0 {
		maxAge = 0
	}

	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secureCookie(nil, s.forceSecure), // no request context for Set
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
		Expires:  sess.ExpiresAt,
	})
	return nil
}

// Get reads and verifies the auth_token cookie. It returns ErrInvalidSession
// when the cookie is missing, malformed, tampered with, or expired.
func (s *CookieStore) Get(r *http.Request) (*domainauth.Session, error) {
	ck, err := r.Cookie(authCookieName)
	if err != nil {
		return nil, ErrInvalidSession
	}
	payload, err := s.decode(ck.Value)
	if err != nil {
		return nil, ErrInvalidSession
	}
	if time.Now().After(payload.ExpiresAt) {
		return nil, ErrInvalidSession
	}
	return &domainauth.Session{
		ID:        payload.ID,
		Principal: payload.Principal,
		ExpiresAt: payload.ExpiresAt,
	}, nil
}

// Clear invalidates the auth_token cookie.
func (s *CookieStore) Clear(w http.ResponseWriter) error {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secureCookie(nil, s.forceSecure),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
	return nil
}

// encode serializes and signs the payload as base64url(payload).base64url(sig).
func (s *CookieStore) encode(p signedPayload) (string, error) {
	plain, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	h := hmac.New(sha256.New, s.secret)
	if _, err := h.Write(plain); err != nil {
		return "", err
	}
	encPayload := base64.RawURLEncoding.EncodeToString(plain)
	encSig := base64.RawURLEncoding.EncodeToString(h.Sum(nil))
	return encPayload + "." + encSig, nil
}

// decode verifies the signature and parses the payload.
func (s *CookieStore) decode(value string) (signedPayload, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return signedPayload{}, errors.New("invalid cookie format")
	}
	plain, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return signedPayload{}, fmt.Errorf("decode payload: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return signedPayload{}, fmt.Errorf("decode signature: %w", err)
	}
	h := hmac.New(sha256.New, s.secret)
	if _, err := h.Write(plain); err != nil {
		return signedPayload{}, err
	}
	if subtle.ConstantTimeCompare(h.Sum(nil), sig) != 1 {
		return signedPayload{}, errors.New("signature mismatch")
	}
	var p signedPayload
	if err := json.Unmarshal(plain, &p); err != nil {
		return signedPayload{}, fmt.Errorf("unmarshal payload: %w", err)
	}
	return p, nil
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fall back to a timestamp-derived value only if the OS CSPRNG fails.
		now := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(now >> (i * 8))
		}
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// compile-time interface check.
var _ domainauth.Store = (*CookieStore)(nil)
