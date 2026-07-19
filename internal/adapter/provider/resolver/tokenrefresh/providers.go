// Package tokenrefresh — per-provider refreshers. This file ports the named
// refresh handlers from open-sse/services/tokenRefresh.js REFRESH_HANDLERS
// (minus kiro, which lives in kiro.go, and vertex, which needs RS256 JWT and
// is stubbed with ErrVertexNotPorted).
//
// Each refresher implements resolver.TokenRefresher. The dispatch table at
// the bottom (Lookup) maps a provider id to its refresher, mirroring the JS
// REFRESH_HANDLERS map. Providers not in the table fall back to the generic
// OAuth2 form-encoded refresh (GenericRefresher), which mirrors the JS
// refreshAccessToken fallback.
package tokenrefresh

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/resolver"
)

// OAuth2 endpoint + client constants — copied verbatim from the JS registry
// (open-sse/providers/registry/<provider>.js) and appConstants.js. These are
// public client ids; client secrets are only present for confidential clients
// (iflow).
const (
	claudeTokenURL   = "https://api.anthropic.com/v1/oauth/token"
	claudeClientID   = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	codexTokenURL    = "https://auth.openai.com/oauth/token"
	codexClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	qwenTokenURL     = "https://chat.qwen.ai/api/v1/oauth2/token"
	qwenClientID     = "f0304373b74a44d2b584a3fb70ca9e56"
	iflowTokenURL    = "https://iflow.cn/oauth/token"
	iflowClientID    = "10009311001"
	iflowClientSecret = "4Z3YjXycVsQvyGF1etiNlIBB4RsqSDtW"
	googleTokenURL   = "https://oauth2.googleapis.com/token"
	githubTokenURL   = "https://github.com/login/oauth/access_token"
	githubClientID   = "Iv1.b507a08c87ecfe98"
	xaiTokenURL      = "https://auth.x.ai/oauth2/token"
	xaiClientID      = "b1a00492-073a-47ea-816f-4c329264a828"
	codebuddyRefreshURL = "https://copilot.tencent.com/v2/plugin/auth/token/refresh"
	codebuddyUserAgent  = "CLI/2.63.2 CodeBuddy/2.63.2"
	// GitHub Copilot token exchange (not an OAuth refresh — exchanges a github
	// access token for a short-lived copilot token).
	copilotTokenURL = "https://api.github.com/copilot_internal/v2/token"
	copilotUserAgent = "GitHubCopilotChat/0.38.0"
	copilotVSCode    = "1.110.0"
	copilotChatVer  = "0.38.0"
	copilotAPIVer   = "2025-04-01"
)

// ClaudeRefresher refreshes a Claude (Anthropic) OAuth token. Mirrors
// refreshClaudeOAuthToken: JSON body {grant_type, refresh_token, client_id}.
type ClaudeRefresher struct{ httpClient *http.Client }

func NewClaudeRefresher() *ClaudeRefresher { return &ClaudeRefresher{httpClient: newRefreshClient()} }

func (r *ClaudeRefresher) Refresh(ctx context.Context, refreshToken string, _ map[string]any, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	if refreshToken == "" {
		return nil, nil
	}
	body := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     claudeClientID,
	}
	tok, err := doJSON(ctx, r.httpClient, opts, claudeTokenURL, body, nil, log, "Claude")
	if err != nil {
		return nil, err
	}
	return fromToken(tok, refreshToken, false), nil
}

// GoogleRefresher refreshes a Google OAuth token (gemini-cli, antigravity).
// Mirrors refreshGoogleToken: form-encoded body with client_id + client_secret
// passed in from the provider's registry config (caller passes them via psd).
type GoogleRefresher struct{ httpClient *http.Client }

func NewGoogleRefresher() *GoogleRefresher { return &GoogleRefresher{httpClient: newRefreshClient()} }

func (r *GoogleRefresher) Refresh(ctx context.Context, refreshToken string, psd map[string]any, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	if refreshToken == "" {
		return nil, nil
	}
	clientID, _ := stringField(psd, "clientId")
	clientSecret, _ := stringField(psd, "clientSecret")
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	tok, err := doForm(ctx, r.httpClient, opts, googleTokenURL, form, nil, log, "Google")
	if err != nil {
		return nil, err
	}
	return fromToken(tok, refreshToken, false), nil
}

// QwenRefresher refreshes a Qwen OAuth token. Mirrors refreshQwenToken:
// form-encoded, client_id only (no secret), carries resource_url through
// ProviderSpecificData.
type QwenRefresher struct{ httpClient *http.Client }

func NewQwenRefresher() *QwenRefresher { return &QwenRefresher{httpClient: newRefreshClient()} }

func (r *QwenRefresher) Refresh(ctx context.Context, refreshToken string, _ map[string]any, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	if refreshToken == "" {
		return nil, nil
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {qwenClientID},
	}
	tok, err := doForm(ctx, r.httpClient, opts, qwenTokenURL, form, nil, log, "Qwen")
	if err != nil {
		return nil, err
	}
	out := fromToken(tok, refreshToken, false)
	if tok.ResourceURL != "" {
		out.ProviderSpecificData = map[string]any{"resourceUrl": tok.ResourceURL}
	}
	return out, nil
}

// CodexRefresher refreshes an OpenAI (Codex CLI) OAuth token. Mirrors
// refreshCodexToken: JSON body, carries id_token, classifies permanent
// (invalid_grant) failures as Unrecoverable so the caller marks re-auth.
type CodexRefresher struct{ httpClient *http.Client }

func NewCodexRefresher() *CodexRefresher { return &CodexRefresher{httpClient: newRefreshClient()} }

func (r *CodexRefresher) Refresh(ctx context.Context, refreshToken string, _ map[string]any, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	if refreshToken == "" {
		return nil, nil
	}
	body := map[string]string{
		"client_id":     codexClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	}
	tok, err := doJSON(ctx, r.httpClient, opts, codexTokenURL, body, nil, log, "Codex")
	if err != nil {
		if cls := classifyOAuthRefreshError(err.Error(), 0); cls.Permanent {
			return &resolver.RefreshedCredentials{Unrecoverable: true}, err
		}
		return nil, err
	}
	return fromToken(tok, refreshToken, true), nil
}

// IflowRefresher refreshes an iFlow OAuth token. Mirrors refreshIflowToken:
// form-encoded body with HTTP Basic auth (clientId:clientSecret).
type IflowRefresher struct{ httpClient *http.Client }

func NewIflowRefresher() *IflowRefresher { return &IflowRefresher{httpClient: newRefreshClient()} }

func (r *IflowRefresher) Refresh(ctx context.Context, refreshToken string, _ map[string]any, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	if refreshToken == "" {
		return nil, nil
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {iflowClientID},
		"client_secret": {iflowClientSecret},
	}
	basic := base64.StdEncoding.EncodeToString([]byte(iflowClientID + ":" + iflowClientSecret))
	hdr := http.Header{"Authorization": []string{"Basic " + basic}}
	tok, err := doForm(ctx, r.httpClient, opts, iflowTokenURL, form, hdr, log, "iFlow")
	if err != nil {
		return nil, err
	}
	return fromToken(tok, refreshToken, false), nil
}

// GitHubRefresher refreshes a GitHub OAuth token. Mirrors refreshGitHubToken:
// form-encoded, client_secret included only when configured (public client by
// default).
type GitHubRefresher struct{ httpClient *http.Client }

func NewGitHubRefresher() *GitHubRefresher { return &GitHubRefresher{httpClient: newRefreshClient()} }

func (r *GitHubRefresher) Refresh(ctx context.Context, refreshToken string, psd map[string]any, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	if refreshToken == "" {
		return nil, nil
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {githubClientID},
	}
	if secret, ok := stringField(psd, "clientSecret"); ok && secret != "" {
		form.Set("client_secret", secret)
	}
	tok, err := doForm(ctx, r.httpClient, opts, githubTokenURL, form, nil, log, "GitHub")
	if err != nil {
		return nil, err
	}
	return fromToken(tok, refreshToken, false), nil
}

// CopilotRefresher exchanges a GitHub access token for a short-lived Copilot
// token. Mirrors refreshCopilotToken: GET with Authorization: token <gh>,
// editor headers. Returns {token, expires_at} (not expires_in), carried via
// RefreshedCredentials.AccessToken + ExpiresAt.
type CopilotRefresher struct{ httpClient *http.Client }

func NewCopilotRefresher() *CopilotRefresher { return &CopilotRefresher{httpClient: newRefreshClient()} }

func (r *CopilotRefresher) Refresh(ctx context.Context, githubAccessToken string, _ map[string]any, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	if githubAccessToken == "" {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, copilotTokenURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+githubAccessToken)
	req.Header.Set("User-Agent", copilotUserAgent)
	req.Header.Set("Editor-Version", "vscode/"+copilotVSCode)
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/"+copilotChatVer)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-github-api-version", copilotAPIVer)
	if log == nil {
		log = resolver.NopLogger()
	}
	resp, err := routeAwareDo(ctx, r.httpClient, req, opts)
	if err != nil {
		log.Warn("token refresh network error", "label", "Copilot", "error", err)
		return nil, err
	}
	defer resp.Body.Close()
	body := readLimit(resp.Body, 1<<16)
	if resp.StatusCode != http.StatusOK {
		log.Warn("token refresh failed", "label", "Copilot", "status", resp.StatusCode, "body", string(body))
		return nil, fmt.Errorf("Copilot refresh %d: %s", resp.StatusCode, string(body))
	}
	var parsed struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := jsonUnmarshal(body, &parsed); err != nil {
		return nil, err
	}
	log.Info("token refreshed", "label", "Copilot")
	return &resolver.RefreshedCredentials{AccessToken: parsed.Token, ExpiresAt: parsed.ExpiresAt}, nil
}

// CodebuddyRefresher refreshes a CodeBuddy (Tencent) token. Mirrors
// refreshCodebuddyToken: POST with empty JSON body, refresh token carried in
// the X-Refresh-Token header. Response {code:0, data:{accessToken,
// refreshToken, expiresIn}}.
type CodebuddyRefresher struct{ httpClient *http.Client }

func NewCodebuddyRefresher() *CodebuddyRefresher { return &CodebuddyRefresher{httpClient: newRefreshClient()} }

func (r *CodebuddyRefresher) Refresh(ctx context.Context, refreshToken string, _ map[string]any, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	if refreshToken == "" {
		return nil, nil
	}
	if log == nil {
		log = resolver.NopLogger()
	}
	hdr := http.Header{
		"User-Agent":            []string{codebuddyUserAgent},
		"X-Requested-With":      []string{"XMLHttpRequest"},
		"X-Domain":              []string{"copilot.tencent.com"},
		"X-Refresh-Token":       []string{refreshToken},
		"X-Auth-Refresh-Source": []string{"plugin"},
		"X-Product":             []string{"SaaS"},
	}
	// Empty JSON body "{}" — the refresh token is in the X-Refresh-Token header.
	raw, _ := jsonMarshal(map[string]any{})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codebuddyRefreshURL, strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := routeAwareDo(ctx, r.httpClient, req, opts)
	if err != nil {
		log.Warn("token refresh network error", "label", "CodeBuddy", "error", err)
		return nil, err
	}
	defer resp.Body.Close()
	body := readLimit(resp.Body, 1<<16)
	if resp.StatusCode != http.StatusOK {
		log.Warn("token refresh failed", "label", "CodeBuddy", "status", resp.StatusCode, "body", string(body))
		return nil, fmt.Errorf("CodeBuddy refresh %d: %s", resp.StatusCode, string(body))
	}
	// Response: {code:0, data:{accessToken, refreshToken, expiresIn}}.
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int    `json:"expiresIn"`
		} `json:"data"`
	}
	if err := jsonUnmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if parsed.Code != 0 || parsed.Data.AccessToken == "" {
		log.Warn("CodeBuddy refresh returned no token", "code", parsed.Code, "msg", parsed.Msg)
		return nil, fmt.Errorf("CodeBuddy refresh: code=%d msg=%s", parsed.Code, parsed.Msg)
	}
	log.Info("token refreshed", "label", "CodeBuddy")
	out := &resolver.RefreshedCredentials{
		AccessToken:  parsed.Data.AccessToken,
		RefreshToken: parsed.Data.RefreshToken,
		ExpiresIn:    parsed.Data.ExpiresIn,
	}
	if out.RefreshToken == "" {
		out.RefreshToken = refreshToken
	}
	return out, nil
}

// XaiRefresher refreshes an xAI (x.ai / grok-cli) OAuth token. Mirrors
// refreshXaiToken / XaiService.refreshAccessToken: form-encoded, client_id
// only (public PKCE client, no secret), carries id_token.
type XaiRefresher struct{ httpClient *http.Client }

func NewXaiRefresher() *XaiRefresher { return &XaiRefresher{httpClient: newRefreshClient()} }

func (r *XaiRefresher) Refresh(ctx context.Context, refreshToken string, _ map[string]any, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	if refreshToken == "" {
		return nil, nil
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {xaiClientID},
		"refresh_token": {refreshToken},
	}
	tok, err := doForm(ctx, r.httpClient, opts, xaiTokenURL, form, nil, log, "xAI")
	if err != nil {
		// JS returns {error:"invalid_grant"} for invalid_grant/invalid_request.
		if cls := classifyOAuthRefreshError(err.Error(), 0); cls.Permanent {
			return &resolver.RefreshedCredentials{Unrecoverable: true}, err
		}
		return nil, err
	}
	return fromToken(tok, refreshToken, true), nil
}

// GenericRefresher is the fallback for providers with a standard OAuth2
// form-encoded refresh endpoint (cline, clinepass, kimi-coding, qoder, ...).
// Mirrors the JS refreshAccessToken fallback: reads refreshUrl/clientId/
// clientSecret from the connection's ProviderSpecificData and POSTs a
// grant_type=refresh_token form body.
type GenericRefresher struct {
	httpClient *http.Client
	providerID string
}

func NewGenericRefresher(providerID string) *GenericRefresher {
	return &GenericRefresher{httpClient: newRefreshClient(), providerID: providerID}
}

func (r *GenericRefresher) Refresh(ctx context.Context, refreshToken string, psd map[string]any, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	if refreshToken == "" {
		return nil, nil
	}
	refreshURL, ok := stringField(psd, "refreshUrl")
	if !ok || refreshURL == "" {
		return nil, fmt.Errorf("no refresh URL configured for provider %q", r.providerID)
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	if cid, ok := stringField(psd, "clientId"); ok && cid != "" {
		form.Set("client_id", cid)
	}
	if secret, ok := stringField(psd, "clientSecret"); ok && secret != "" {
		form.Set("client_secret", secret)
	}
	tok, err := doForm(ctx, r.httpClient, opts, refreshURL, form, nil, log, r.providerID)
	if err != nil {
		return nil, err
	}
	return fromToken(tok, refreshToken, false), nil
}

// VertexRefresher is stubbed: vertex needs an RS256-signed service-account JWT
// (go-jose). It returns ErrVertexNotPorted so callers fall back to the static
// catalog. Real implementation is a T027 follow-up.
type VertexRefresher struct{}

func NewVertexRefresher() *VertexRefresher { return &VertexRefresher{} }

func (*VertexRefresher) Refresh(_ context.Context, _ string, _ map[string]any, _ resolver.ProxyOptions, _ resolver.Logger) (*resolver.RefreshedCredentials, error) {
	return nil, ErrVertexNotPorted
}

// Lookup returns the TokenRefresher for a provider id, mirroring the JS
// REFRESH_HANDLERS map. Returns nil for providers with no refresh handler
// (the caller may then try GenericRefresher via LookupGeneric).
func Lookup(providerID string) resolver.TokenRefresher {
	switch providerID {
	case "claude":
		return NewClaudeRefresher()
	case "gemini-cli", "antigravity":
		return NewGoogleRefresher()
	case "qwen":
		return NewQwenRefresher()
	case "codex":
		return NewCodexRefresher()
	case "iflow":
		return NewIflowRefresher()
	case "github":
		return NewGitHubRefresher()
	case "copilot":
		return NewCopilotRefresher()
	case "codebuddy-cn":
		return NewCodebuddyRefresher()
	case "xai", "grok-cli", "gcli":
		return NewXaiRefresher()
	case "kiro":
		return NewKiroRefresher()
	case "vertex", "vertex-partner":
		return NewVertexRefresher()
	}
	return nil
}

// stringField reads a string field from a provider-specific-data map, tolerating
// both string and numeric JSON values.
func stringField(m map[string]any, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[key]
	if !ok {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case fmt.Stringer:
		return t.String(), true
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v)), true
	}
}