package tokenrefresh

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/resolver"
	"github.com/go-jose/go-jose/v4"
)


// vertexTokenLeadTime is how far before expiry a cached token is considered
// stale. The Google token is valid for 1 hour; we refresh when < 5 min remain.
// Mirrors the JS vertexTokenCache check `expiresAt - Date.now() > 5 * 60 * 1000`.
const vertexTokenLeadTime = 5 * time.Minute

// serviceAccountJSON is the shape of a GCP service account key file.
type serviceAccountJSON struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	ClientID     string `json:"client_id"`
	TokenURI     string `json:"token_uri"`
}

// parseVertexSAJSON parses a GCP service account JSON key. Returns nil (not
// an error) when the input is not a valid SA JSON — the caller treats nil
// as "not a service account, skip". Mirrors parseVertexSaJson in
// open-sse/services/tokenRefresh.js.
func parseVertexSAJSON(raw string) (*serviceAccountJSON, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '{' {
		return nil, nil
	}
	var sa serviceAccountJSON
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		return nil, fmt.Errorf("vertex: invalid service account JSON: %w", err)
	}
	if sa.Type != "service_account" || sa.ClientEmail == "" || sa.PrivateKey == "" || sa.ProjectID == "" {
		return nil, nil
	}
	return &sa, nil
}

// VertexRefresher mints a Google OAuth2 access token from a GCP service
// account JSON key. Unlike OAuth2 refresh flows, Vertex does not use a
// refresh_token — it creates an RS256 JWT assertion (grant_type=jwt-bearer)
// signed with the SA private key and exchanges it at
// https://oauth2.googleapis.com/token. The token is cached per service
// account email. Mirrors refreshVertexToken in tokenRefresh.js.
type VertexRefresher struct {
	httpClient *http.Client
	mu         sync.Mutex
	tokenCache map[string]vertexCachedToken
}

type vertexCachedToken struct {
	token     string
	expiresAt time.Time
}

func NewVertexRefresher() *VertexRefresher {
	return &VertexRefresher{httpClient: newRefreshClient(), tokenCache: make(map[string]vertexCachedToken)}
}

// Refresh reads the service account JSON from psd["apiKey"] and mints an
// access token. The refreshToken argument is unused — Vertex's "credential"
// is the SA key itself, not a refresh token.
func (r *VertexRefresher) Refresh(ctx context.Context, _ string, psd map[string]any, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	saJSON, _ := psd["apiKey"].(string)
	if saJSON == "" {
		return nil, nil
	}
	sa, err := parseVertexSAJSON(saJSON)
	if err != nil {
		return nil, fmt.Errorf("vertex: parse service account: %w", err)
	}
	if sa == nil {
		return nil, nil
	}
	return r.mintVertexToken(ctx, sa, opts, log)
}

// mintVertexToken creates an RS256-signed JWT, exchanges it for an access
// token at the Google token endpoint, and caches the result.
func (r *VertexRefresher) mintVertexToken(ctx context.Context, sa *serviceAccountJSON, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	// Check cache (5 min lead).
	r.mu.Lock()
	if cached, ok := r.tokenCache[sa.ClientEmail]; ok && cached.expiresAt.Sub(time.Now()) > vertexTokenLeadTime {
		r.mu.Unlock()
		return &resolver.RefreshedCredentials{
			AccessToken: cached.token,
			ExpiresIn:   int(time.Until(cached.expiresAt) / time.Second),
		}, nil
	}
	r.mu.Unlock()

	key, err := parsePKCS8PrivateKey(sa.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("vertex: parse private key: %w", err)
	}

	now := time.Now()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, &jose.SignerOptions{
		ExtraHeaders: map[jose.HeaderKey]any{"typ": "JWT"},
	})
	if err != nil {
		return nil, fmt.Errorf("vertex: create signer: %w", err)
	}

	claims := map[string]any{
		"iss":   sa.ClientEmail,
		"scope": "https://www.googleapis.com/auth/cloud-platform",
		"aud":   googleTokenURL,
		"exp":   now.Add(time.Hour).Unix(),
		"iat":   now.Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return nil, fmt.Errorf("vertex: marshal claims: %w", err)
	}

	jws, err := signer.Sign(payload)
	if err != nil {
		return nil, fmt.Errorf("vertex: sign JWT: %w", err)
	}
	assertion, err := jws.CompactSerialize()
	if err != nil {
		return nil, fmt.Errorf("vertex: serialize JWT: %w", err)
	}

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	tok, err := doForm(ctx, r.httpClient, opts, googleTokenURL, form, nil, log, "Vertex")
	if err != nil {
		return nil, fmt.Errorf("vertex: token exchange: %w", err)
	}
	if tok == nil {
		return nil, nil
	}

	expiresIn := tok.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)

	r.mu.Lock()
	r.tokenCache[sa.ClientEmail] = vertexCachedToken{token: tok.AccessToken, expiresAt: expiresAt}
	r.mu.Unlock()

	if log != nil {
		log.Info("vertex token minted", "serviceAccount", sa.ClientEmail)
	}
	return &resolver.RefreshedCredentials{AccessToken: tok.AccessToken, ExpiresIn: expiresIn}, nil
}

// parsePKCS8PrivateKey parses a PEM-encoded PKCS8 RSA private key from a
// GCP service account JSON. The SA key's private_key field contains
// newline-escaped PEM; we unescape \n → LF before parsing.
func parsePKCS8PrivateKey(pemKey string) (*rsa.PrivateKey, error) {
	raw := strings.ReplaceAll(pemKey, "\\n", "\n")
	block, _ := pem.Decode([]byte(raw))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Try PKCS1 as fallback (some older SA keys).
		if k, e2 := x509.ParsePKCS1PrivateKey(block.Bytes); e2 == nil {
			return k, nil
		}
		return nil, fmt.Errorf("parse PKCS8: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is not RSA")
	}
	return rsaKey, nil
}
