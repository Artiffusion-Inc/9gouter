package auth

// bcrypt.go provides the concrete bcrypt comparator the dashboard login
// usecase needs. The usecase (internal/usecase/auth) keeps a BcryptFunc
// dependency-injection seam so it does not import golang.org/x/crypto/bcrypt
// directly; this adapter supplies the real implementation wired in wire.go
// and the auth HTTP handler.

import "golang.org/x/crypto/bcrypt"

// CompareBcrypt is the real bcrypt comparator: returns nil iff password
// matches the stored hash. It is the BcryptFunc the usecase expects
// (password, hash) -> error, matching bcrypt.CompareHashAndPassword order
// (hash, password) internally.
func CompareBcrypt(password, hash string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

// HashBcrypt is a convenience helper for generating a bcrypt hash from a
// plaintext password (cost 10, matching the JS dashboard default). Used by
// tests and the backup/restore seed; not on the hot login path.
func HashBcrypt(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}