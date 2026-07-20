// Package auth provides the dashboard authentication usecase.
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/auth"
	domainauth "github.com/Artiffusion-Inc/9gouter/internal/domain/auth"
)

var (
	// ErrUnauthorized is returned for any failed login attempt.
	ErrUnauthorized = errors.New("invalid credentials")
	// ErrLocked is returned when the IP is locked by the login limiter.
	ErrLocked = errors.New("too many failed login attempts")
)

// PasswordVerifier checks a plaintext password against the configured hash or
// initial password, mirroring dashboardSession.js verifyDashboardPassword.
type PasswordVerifier interface {
	Verify(password string) error
}

// LoginLimiter is the subset of auth.LoginLimiter used by the usecase.
type LoginLimiter interface {
	Allow(ip string) bool
	RecordFail(ip string) int
	RecordSuccess(ip string)
}

// UseCase ties together password verification, login rate-limiting and session
// creation. It does not depend on net/http so transport adapters decide how to
// write the cookie.
type UseCase struct {
	store     domainauth.Store
	limiter   LoginLimiter
	verifier  PasswordVerifier
	ttl       time.Duration
}

// New returns a dashboard auth usecase. verifier may be nil if the caller only
// uses OIDC; in that case password login always fails. sessionTTL defaults to 24h.
func New(store domainauth.Store, limiter LoginLimiter, verifier PasswordVerifier, sessionTTL time.Duration) *UseCase {
	if sessionTTL <= 0 {
		sessionTTL = 24 * time.Hour
	}
	if limiter == nil {
		// Safe no-op limiter.
		limiter = auth.NewLoginLimiter()
	}
	return &UseCase{
		store:    store,
		limiter:  limiter,
		verifier: verifier,
		ttl:      sessionTTL,
	}
}

// Login validates the password and, on success, returns a new session. The IP
// is used for login rate limiting. This extends the plan's signature with an ip
// argument because the ported loginLimiter.js is IP-based.
func (uc *UseCase) Login(user, pass, ip string) (domainauth.Session, error) {
	if !uc.limiter.Allow(ip) {
		return domainauth.Session{}, ErrLocked
	}

	if uc.verifier == nil || uc.verifier.Verify(pass) != nil {
		uc.limiter.RecordFail(ip)
		return domainauth.Session{}, ErrUnauthorized
	}

	uc.limiter.RecordSuccess(ip)

	sess := domainauth.Session{
		ID: "",
		Principal: domainauth.Principal{
			ID:    user,
			Email: "",
			Name:  user,
		},
		ExpiresAt: time.Now().Add(uc.ttl),
	}
	return sess, nil
}

// PlainVerifier matches the default-password path in dashboardSession.js:
// if no bcrypt hash is stored, compare against the configured initial password.
type PlainVerifier struct {
	InitialPassword string
}

// Verify returns nil if the password equals the initial password.
func (v *PlainVerifier) Verify(password string) error {
	if password == v.InitialPassword {
		return nil
	}
	return ErrUnauthorized
}

// BcryptFunc is the function shape used to compare a bcrypt hash. The concrete
// implementation is provided by the adapter so the usecase stays dependency-free
// of golang.org/x/crypto/bcrypt if it is not needed.
type BcryptFunc func(password, hash string) error

// BcryptVerifier compares passwords against a stored bcrypt hash.
type BcryptVerifier struct {
	Hash       string
	Comparator BcryptFunc
}

// Verify returns nil if the password matches the stored hash.
func (v *BcryptVerifier) Verify(password string) error {
	if v.Hash == "" {
		return ErrUnauthorized
	}
	if v.Comparator == nil {
		return errors.New("auth: bcrypt comparator not configured")
	}
	if err := v.Comparator(password, v.Hash); err != nil {
		return ErrUnauthorized
	}
	return nil
}

// FormatError converts an auth error into a user-facing message.
func FormatError(err error) string {
	switch {
	case errors.Is(err, ErrLocked):
		return "Too many failed login attempts. Please try again later."
	case errors.Is(err, ErrUnauthorized):
		return "Invalid password."
	default:
		return fmt.Sprintf("Login failed: %v", err)
	}
}
