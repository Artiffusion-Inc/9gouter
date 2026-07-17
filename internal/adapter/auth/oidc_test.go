package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestOIDC_MockProviderExchange(t *testing.T) {
	issuer, jwksJSON, idToken := newMockOIDCIssuer(t, "test-client", "test-sub", "test@example.com", "Test User")

	ctx := context.Background()
	provider, err := NewOIDC(ctx, OIDCConfig{
		IssuerURL:    issuer,
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		RedirectURI:  issuer + "/auth/oidc/callback",
		Scopes:       []string{"openid", "profile", "email"},
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	if provider == nil {
		t.Fatal("expected provider")
	}

	authURL, _, _, codeVerifier, err := provider.StartURL(ctx, "")
	if err != nil {
		t.Fatalf("StartURL: %v", err)
	}
	if authURL == "" {
		t.Fatal("expected auth URL")
	}
	if !strings.Contains(authURL, "code_challenge=") {
		t.Fatal("expected PKCE code_challenge in auth URL")
	}

	principal, err := provider.Exchange(ctx, "mock-code", "", codeVerifier)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if principal.ID != "test-sub" {
		t.Errorf("id = %q, want test-sub", principal.ID)
	}
	if principal.Email != "test@example.com" {
		t.Errorf("email = %q, want test@example.com", principal.Email)
	}
	if principal.Name != "test@example.com" {
		t.Errorf("name = %q, want test@example.com (matches JS display-name priority)", principal.Name)
	}

	// Also exercise Verify against the raw ID token.
	claims, err := provider.Verify(ctx, idToken)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims["sub"] != "test-sub" {
		t.Errorf("verify sub = %v, want test-sub", claims["sub"])
	}

	_ = jwksJSON
}

func TestOIDC_IsConfigured(t *testing.T) {
	if !IsConfigured(OIDCConfig{IssuerURL: "https://idp.example.com", ClientID: "id", ClientSecret: "secret"}) {
		t.Error("expected configured")
	}
	if IsConfigured(OIDCConfig{IssuerURL: "https://idp.example.com", ClientID: "id"}) {
		t.Error("expected not configured without secret")
	}
}

func newMockOIDCIssuer(t *testing.T, clientID, sub, email, name string) (issuer, jwksJSON, idToken string) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	// Build a minimal JWK from the public key.
	n := base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes())
	jwks := map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"use": "sig",
			"kid": "mock-key",
			"n":   n,
			"e":   e,
			"alg": "RS256",
		}},
	}
	jwksBytes, err := json.Marshal(jwks)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	jwksJSON = string(jwksBytes)

	var tokenEndpoint string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			discovery := map[string]any{
				"issuer":                 issuer,
				"authorization_endpoint": issuer + "/auth",
				"token_endpoint":         tokenEndpoint,
				"jwks_uri":               issuer + "/jwks",
				"response_types_supported": []string{"code"},
				"subject_types_supported":  []string{"public"},
				"id_token_signing_alg_values_supported": []string{"RS256"},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(discovery)
		case "/jwks":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(jwksBytes)
		case "/token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if r.PostForm.Get("client_id") != clientID {
				http.Error(w, "invalid_client", http.StatusUnauthorized)
				return
			}
			if r.PostForm.Get("grant_type") != "authorization_code" {
				http.Error(w, "unsupported_grant_type", http.StatusBadRequest)
				return
			}

			now := time.Now()
			tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
				"iss":   issuer,
				"sub":   sub,
				"aud":   clientID,
				"exp":   now.Add(time.Hour).Unix(),
				"iat":   now.Unix(),
				"email": email,
				"name":  name,
			})
			tok.Header["kid"] = "mock-key"
			signed, err := tok.SignedString(priv)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			idToken = signed
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-access-token",
				"token_type":   "Bearer",
				"expires_in":   3600,
				"id_token":     signed,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() {
		if issuer == "" {
			ts.Close()
		}
	}()

	issuer = ts.URL
	tokenEndpoint = ts.URL + "/token"

	// Pre-create one ID token so the caller can also verify it independently.
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":   issuer,
		"sub":   sub,
		"aud":   clientID,
		"exp":   now.Add(time.Hour).Unix(),
		"iat":   now.Unix(),
		"email": email,
		"name":  name,
	})
	tok.Header["kid"] = "mock-key"
	idToken, err = tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign id token: %v", err)
	}

	return issuer, jwksJSON, idToken
}

func TestParsePublicOrigin(t *testing.T) {
	r := &stubRequest{
		headers: map[string]string{
			"X-Forwarded-Proto": "https",
			"X-Forwarded-Host":  "dashboard.example.com",
		},
	}
	if got, want := ParsePublicOrigin(r, ""), "https://dashboard.example.com"; got != want {
		t.Errorf("ParsePublicOrigin = %q, want %q", got, want)
	}
	if got, want := ParsePublicOrigin(nil, "https://override.example.com"), "https://override.example.com"; got != want {
		t.Errorf("ParsePublicOrigin with configured = %q, want %q", got, want)
	}
}

type stubRequest struct {
	headers map[string]string
}

func (s *stubRequest) Header(name string) string {
	return s.headers[name]
}

// Ensure imports are used.
var _ = fmt.Sprintf
var _ = url.Values{}
var _ = strings.Contains
