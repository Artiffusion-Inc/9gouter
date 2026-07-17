package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	domainauth "github.com/Artiffusion-Inc/9router/internal/domain/auth"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCConfig holds the runtime OIDC settings. The dashboard JS reads these from
// settings/env; in Go they are passed via constructor so the adapter does not
// depend on config.Config directly.
type OIDCConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Scopes       []string
}

func (c *OIDCConfig) normalize() {
	c.IssuerURL = strings.TrimRight(c.IssuerURL, "/")
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.ClientSecret = strings.TrimSpace(c.ClientSecret)
	if len(c.Scopes) == 0 {
		c.Scopes = []string{"openid", "profile", "email"}
	}
}

// OIDC implements domainauth.OIDCPort using github.com/coreos/go-oidc/v3 and
// golang.org/x/oauth2. It ports the start/callback/test flows from oidc.js.
type OIDC struct {
	cfg      OIDCConfig
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
}

// NewOIDC discovers the provider and returns an OIDC adapter. The context is used
// for the discovery HTTP request.
func NewOIDC(ctx context.Context, cfg OIDCConfig) (*OIDC, error) {
	cfg.normalize()
	if cfg.IssuerURL == "" {
		return nil, errors.New("oidc: issuer URL is required")
	}
	if cfg.ClientID == "" {
		return nil, errors.New("oidc: client id is required")
	}

	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}

	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})

	return &OIDC{
		cfg:      cfg,
		provider: provider,
		verifier: verifier,
	}, nil
}

// StartURL builds the authorization URL and returns the generated PKCE/state
// values so the caller can persist them for callback validation. This satisfies
// the domainauth.OIDCPort contract.
func (o *OIDC) StartURL(ctx context.Context, redirectURI string) (authURL, state, nonce, codeVerifier string, err error) {
	if redirectURI == "" {
		redirectURI = o.cfg.RedirectURI
	}
	state = randomID()
	nonce = randomID()
	codeVerifier = oauth2.GenerateVerifier()

	authURL = o.oauth2Config(redirectURI).AuthCodeURL(
		state,
		oauth2.AccessTypeOnline,
		oauth2.S256ChallengeOption(codeVerifier),
		oidc.Nonce(nonce),
	)
	return authURL, state, nonce, codeVerifier, nil
}

// Exchange trades the authorization code for an ID token and returns the
// authenticated principal. The domainauth.OIDCPort contract does not pass the
// nonce, so nonce validation must be performed by the caller if required; the
// ID token signature, issuer and audience are verified here.
func (o *OIDC) Exchange(ctx context.Context, code, redirectURI, codeVerifier string) (domainauth.Principal, error) {
	if redirectURI == "" {
		redirectURI = o.cfg.RedirectURI
	}

	token, err := o.oauth2Config(redirectURI).Exchange(ctx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return domainauth.Principal{}, fmt.Errorf("oidc token exchange: %w", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		return domainauth.Principal{}, errors.New("oidc: id_token missing from token response")
	}

	idToken, err := o.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return domainauth.Principal{}, fmt.Errorf("oidc id token verification: %w", err)
	}

	var claims idTokenClaims
	if err := idToken.Claims(&claims); err != nil {
		return domainauth.Principal{}, fmt.Errorf("oidc parse claims: %w", err)
	}

	return domainauth.Principal{
		ID:    claims.Sub,
		Email: claims.Email,
		Name:  pickDisplayName(claims),
	}, nil
}

// Verify validates a raw ID token string and returns its claims. This mirrors
// oidc.js verifyOidcIdToken and is useful for test/logout endpoints.
func (o *OIDC) Verify(ctx context.Context, idToken string) (map[string]any, error) {
	if idToken == "" {
		return nil, errors.New("oidc: empty id token")
	}
	tok, err := o.verifier.Verify(ctx, idToken)
	if err != nil {
		return nil, fmt.Errorf("oidc verify: %w", err)
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("oidc claims: %w", err)
	}
	return claims, nil
}

// Provider exposes the underlying OIDC provider for advanced callers (e.g.
// JWK endpoint inspection).
func (o *OIDC) Provider() *oidc.Provider { return o.provider }

// oauth2Config builds an oauth2.Config for the given redirect URI.
func (o *OIDC) oauth2Config(redirectURI string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     o.cfg.ClientID,
		ClientSecret: o.cfg.ClientSecret,
		RedirectURL:  redirectURI,
		Endpoint:     o.provider.Endpoint(),
		Scopes:       o.cfg.Scopes,
	}
}

type idTokenClaims struct {
	Sub               string `json:"sub"`
	Email             string `json:"email"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	GivenName         string `json:"given_name"`
}

func pickDisplayName(c idTokenClaims) string {
	if c.PreferredUsername != "" {
		return c.PreferredUsername
	}
	if c.Email != "" {
		return c.Email
	}
	if c.Name != "" {
		return c.Name
	}
	if c.GivenName != "" {
		return c.GivenName
	}
	return c.Sub
}

// IsConfigured is a helper that reports whether the required OIDC fields are
// present, matching oidc.js isOidcConfigured.
func IsConfigured(cfg OIDCConfig) bool {
	cfg.normalize()
	return cfg.IssuerURL != "" && cfg.ClientID != "" && cfg.ClientSecret != ""
}

// ParsePublicOrigin mirrors oidc.js getPublicOrigin: prefer configured base URL,
// then X-Forwarded-Proto/Host, then request origin.
func ParsePublicOrigin(r httpRequestLike, configuredBaseURL string) string {
	if v := strings.TrimSpace(configuredBaseURL); v != "" {
		return strings.TrimRight(v, "/")
	}
	proto := "http"
	if r != nil {
		if v := strings.ToLower(strings.TrimSpace(r.Header("X-Forwarded-Proto"))); v != "" {
			proto = v
		}
		host := strings.TrimSpace(r.Header("X-Forwarded-Host"))
		if host == "" {
			host = strings.TrimSpace(r.Header("Host"))
		}
		if host != "" {
			return fmt.Sprintf("%s://%s", proto, strings.TrimRight(host, "/"))
		}
	}
	return ""
}

// httpRequestLike abstracts the few headers needed by ParsePublicOrigin so it
// can be called with either *http.Request or a test stub.
type httpRequestLike interface {
	Header(name string) string
}

// compile-time interface check.
var _ domainauth.OIDCPort = (*OIDC)(nil)

// redirectURLForCallback builds the callback redirect URI from a public origin.
func redirectURLForCallback(origin string) string {
	return strings.TrimRight(origin, "/") + "/auth/oidc/callback"
}
