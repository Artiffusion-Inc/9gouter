package auth

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/adapter/auth"
	domainauth "github.com/Artiffusion-Inc/9router/internal/domain/auth"
)

type memStore struct{ got *domainauth.Session }

func (m *memStore) Set(_ http.ResponseWriter, sess domainauth.Session) error {
	m.got = &sess
	return nil
}

func (m *memStore) Get(_ *http.Request) (*domainauth.Session, error) { return nil, nil }
func (m *memStore) Clear(_ http.ResponseWriter) error                { return nil }

func TestUseCase_Login_Success(t *testing.T) {
	store := &memStore{}
	limiter := auth.NewLoginLimiter()
	uc := New(store, limiter, &PlainVerifier{InitialPassword: "secret123"}, time.Hour)

	sess, err := uc.Login("admin", "secret123", "192.0.2.1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if sess.Principal.ID != "admin" {
		t.Errorf("principal id = %q, want admin", sess.Principal.ID)
	}
	if sess.ExpiresAt.IsZero() {
		t.Error("expected expiry to be set")
	}
	if status := limiter.CheckLock("192.0.2.1"); status.Locked {
		t.Error("expected no lock after success")
	}
}

func TestUseCase_Login_InvalidPassword(t *testing.T) {
	store := &memStore{}
	limiter := auth.NewLoginLimiter()
	uc := New(store, limiter, &PlainVerifier{InitialPassword: "secret123"}, time.Hour)

	for i := 0; i < 5; i++ {
		_, err := uc.Login("admin", "wrong", "192.0.2.2")
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("attempt %d: expected ErrUnauthorized, got %v", i, err)
		}
	}
	_, err := uc.Login("admin", "wrong", "192.0.2.2")
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
}

func TestUseCase_Login_Bcrypt(t *testing.T) {
	store := &memStore{}
	limiter := auth.NewLoginLimiter()
	uc := New(store, limiter, &BcryptVerifier{
		Hash: "stored-hash",
		Comparator: func(password, hash string) error {
			if password == "correct" && hash == "stored-hash" {
				return nil
			}
			return errors.New("mismatch")
		},
	}, time.Hour)

	_, err := uc.Login("admin", "correct", "192.0.2.3")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	_, err = uc.Login("admin", "wrong", "192.0.2.3")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestUseCase_Login_NoVerifier(t *testing.T) {
	store := &memStore{}
	uc := New(store, auth.NewLoginLimiter(), nil, time.Hour)
	_, err := uc.Login("admin", "anything", "192.0.2.4")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized without verifier, got %v", err)
	}
}

func TestFormatError(t *testing.T) {
	if got := FormatError(ErrLocked); got == "" {
		t.Error("expected locked message")
	}
	if got := FormatError(ErrUnauthorized); got == "" {
		t.Error("expected unauthorized message")
	}
}
