package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AuthData is the result of the authorize action: the URL to redirect the
// user to, plus the PKCE state/verifier that must be persisted for the
// exchange step. Mirrors generateAuthData in JS.
type AuthData struct {
	AuthURL      string `json:"authUrl"`
	State        string `json:"state"`
	CodeVerifier string `json:"codeVerifier"`
	RedirectURI  string `json:"redirectUri"`
	FlowType     string `json:"flowType"`
	CallbackPath string `json:"callbackPath"`
	FixedPort    int    `json:"fixedPort,omitempty"`
}

// TokenData is the normalized result of a token exchange or device poll.
type TokenData struct {
	AccessToken      string            `json:"accessToken"`
	RefreshToken     string            `json:"refreshToken,omitempty"`
	IDToken          string            `json:"idToken,omitempty"`
	ExpiresIn        int               `json:"expiresIn,omitempty"`
	Email            string            `json:"email,omitempty"`
	ProviderSpecificData map[string]any `json:"providerSpecificData,omitempty"`
}

// Authorize generates the authorization URL + PKCE state for a provider.
// For device_code providers, AuthURL is empty (the flow starts with device-code).
func Authorize(ctx context.Context, providerID, redirectURI string) (*AuthData, error) {
	cfg, ok := Lookup(providerID)
	if !ok {
		return nil, fmt.Errorf("unknown OAuth provider: %s", providerID)
	}

	state, err := GenerateState()
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}

	result := &AuthData{
		State:        state,
		RedirectURI:  redirectURI,
		FlowType:     string(cfg.FlowType),
		CallbackPath: cfg.CallbackPathOr("/callback"),
		FixedPort:    cfg.FixedPort,
	}

	if cfg.FlowType == FlowDeviceCode || cfg.FlowType == FlowBrowserToken {
		// No auth URL for device code flow — the flow starts with device-code.
		result.CodeVerifier = ""
		return result, nil
	}

	// PKCE flow: generate verifier + challenge.
	var codeChallenge string
	if cfg.FlowType == FlowAuthCodePKCE {
		verifier, challenge, err := GeneratePKCE()
		if err != nil {
			return nil, fmt.Errorf("generate PKCE: %w", err)
		}
		result.CodeVerifier = verifier
		codeChallenge = challenge
	}

	// Build authorize URL.
	params := url.Values{
		"response_type": {"code"},
		"client_id":     {cfg.ClientID},
		"redirect_uri":  {redirectURI},
		"state":         {state},
	}
	if scope := cfg.ScopeString(); scope != "" {
		params.Set("scope", scope)
	}
	if codeChallenge != "" {
		params.Set("code_challenge", codeChallenge)
		params.Set("code_challenge_method", cfg.CodeChallengeMethod)
	}
	// Extra params (codex: originator, codex_cli_simplified_flow, etc).
	for k, v := range cfg.ExtraParams {
		params.Set(k, v)
	}

	result.AuthURL = cfg.AuthorizeURL + "?" + params.Encode()
	return result, nil
}

// Exchange trades an authorization code for access/refresh tokens. For PKCE
// providers, codeVerifier is required; for non-PKCE, clientSecret is used.
func Exchange(ctx context.Context, providerID, code, redirectURI, codeVerifier string) (*TokenData, error) {
	cfg, ok := Lookup(providerID)
	if !ok {
		return nil, fmt.Errorf("unknown OAuth provider: %s", providerID)
	}

	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
		"client_id":    {cfg.ClientID},
	}
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}

	body, err := postForm(ctx, cfg.TokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}

	var resp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("oauth: %s: %s", resp.Error, resp.ErrorDesc)
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("oauth: no access_token in response")
	}

	return &TokenData{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		IDToken:      resp.IDToken,
		ExpiresIn:     resp.ExpiresIn,
	}, nil
}

// DeviceCodeData is the response from a device code request.
type DeviceCodeData struct {
	DeviceCode      string `json:"device_code"`
	UserCode         string `json:"user_code"`
	VerificationURI  string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn        int    `json:"expires_in"`
	Interval         int    `json:"interval"`
	CodeVerifier     string `json:"codeVerifier,omitempty"` // PKCE verifier (Qwen)
}

// RequestDeviceCode requests a device code from the provider's device code
// endpoint. For PKCE device-code providers (qwen), codeChallenge is included.
func RequestDeviceCode(ctx context.Context, providerID string) (*DeviceCodeData, error) {
	cfg, ok := Lookup(providerID)
	if !ok {
		return nil, fmt.Errorf("unknown OAuth provider: %s", providerID)
	}
	if cfg.DeviceCodeURL == "" {
		return nil, fmt.Errorf("provider %s does not support device code flow", providerID)
	}

	form := url.Values{
		"client_id": {cfg.ClientID},
	}
	if scope := cfg.ScopeString(); scope != "" {
		form.Set("scope", scope)
	}

	var codeVerifier string
	if cfg.CodeChallengeMethod == "S256" {
		var challenge string
		var err error
		codeVerifier, challenge, err = GeneratePKCE()
		if err != nil {
			return nil, fmt.Errorf("generate PKCE: %w", err)
		}
		form.Set("code_challenge", challenge)
		form.Set("code_challenge_method", cfg.CodeChallengeMethod)
	}

	body, err := postForm(ctx, cfg.DeviceCodeURL, form)
	if err != nil {
		return nil, fmt.Errorf("device code request: %w", err)
	}

	var resp DeviceCodeData
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse device code response: %w", err)
	}
	resp.CodeVerifier = codeVerifier
	return &resp, nil
}

// PollResult is the result of polling for a device code token.
type PollResult struct {
	Success  bool       `json:"success"`
	Tokens   *TokenData `json:"tokens,omitempty"`
	Pending  bool       `json:"pending,omitempty"`
	Error    string     `json:"error,omitempty"`
	ErrorDesc string    `json:"errorDescription,omitempty"`
}

// PollDeviceCode polls the token endpoint for a device code flow. Returns
// PollResult with Pending=true when authorization is still pending.
func PollDeviceCode(ctx context.Context, providerID, deviceCode, codeVerifier string) (*PollResult, error) {
	cfg, ok := Lookup(providerID)
	if !ok {
		return nil, fmt.Errorf("unknown OAuth provider: %s", providerID)
	}

	form := url.Values{
		"grant_type":   {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code":  {deviceCode},
		"client_id":    {cfg.ClientID},
	}
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}

	body, err := postForm(ctx, cfg.TokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("device poll: %w", err)
	}

	var resp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse poll response: %w", err)
	}

	if resp.Error != "" {
		if resp.Error == "authorization_pending" || resp.Error == "slow_down" {
			return &PollResult{
				Pending:   resp.Error == "authorization_pending",
				Error:     resp.Error,
				ErrorDesc: resp.ErrorDesc,
			}, nil
		}
		return &PollResult{
			Error:     resp.Error,
			ErrorDesc: resp.ErrorDesc,
		}, nil
	}

	if resp.AccessToken == "" {
		return &PollResult{Error: "no_access_token"}, nil
	}

	return &PollResult{
		Success: true,
		Tokens: &TokenData{
			AccessToken:  resp.AccessToken,
			RefreshToken: resp.RefreshToken,
			ExpiresIn:    resp.ExpiresIn,
		},
	}, nil
}

// postForm posts a form-encoded body and returns the response body.
func postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}
