// Package tokenrefresh ports the per-provider OAuth/token-refresh functions
// from open-sse/services/tokenRefresh/providers.js. Each provider has its own
// refresh protocol; this package implements them one at a time. The first
// ported provider is kiro (the T030 live resolver depends on it to retry
// ListAvailableModels after a 401).
//
// NOT YET PORTED (follow-up): the other 10 providers (claude, google, qwen,
// codex, iflow, github, copilot, codebuddy, xai, vertex). Each is a small
// HTTP POST with provider-specific headers/body; they slot in as additional
// TokenRefresher implementations.
package tokenrefresh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/resolver"
)

// kiroRefreshEndpoint constants — from open-sse/providers/registry/kiro.js
// and services/tokenRefresh/providers.js (PROVIDERS.kiro.tokenUrl).
const (
	kiroSocialTokenURL = "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
	kiroAWSOIDCBase    = "https://oidc.%s.amazonaws.com/token"
	kiroDefaultOIDC    = "https://oidc.us-east-1.amazonaws.com/token"
	kiroRefreshTimeout = 20 * time.Second
)

// KiroRefresher refreshes an expired Kiro access token. It implements the
// two main Kiro auth branches from the JS refreshKiroToken:
//
//  1. AWS SSO (clientId + clientSecret present): POST to the AWS OIDC token
//     endpoint with a JSON body {clientId, clientSecret, refreshToken,
//     grantType:"refresh_token"}. IDC connections use a region-scoped
//     endpoint; builder-id uses the default us-east-1.
//  2. Social (no clientId/secret): POST to the Kiro social tokenUrl with a
//     JSON body {refreshToken} and User-Agent "kiro-cli/1.0.0".
//
// NOT YET PORTED: the external_idp branch (buildExternalIdpRefreshParams),
// which depends on the unported src/lib/oauth/kiroExternalIdp.js helper.
// Connections with authMethod=="external_idp" return
// ErrExternalIDPNotPorted so the caller falls back to the static catalog
// rather than silently doing the wrong thing.
type KiroRefresher struct {
	client *http.Client
}

// NewKiroRefresher builds a KiroRefresher with a bounded-timeout HTTP client.
func NewKiroRefresher() *KiroRefresher {
	return &KiroRefresher{client: &http.Client{Timeout: kiroRefreshTimeout}}
}

// ErrExternalIDPNotPorted signals an unported refresh branch.
var ErrExternalIDPNotPorted = fmt.Errorf("kiro external_idp refresh not yet ported (T027 follow-up)")

// Refresh implements resolver.TokenRefresher. It never panics on a nil psd.
// opts routes the refresh HTTP call through the proxy stack when set (Fix 2a) —
// a kiro connection behind a strict proxy refreshes through the same path as
// its ListAvailableModels call instead of dialing the AWS OIDC / social token
// endpoint directly.
func (k *KiroRefresher) Refresh(ctx context.Context, refreshToken string, psd map[string]any, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	if log == nil {
		log = resolver.NopLogger()
	}
	if refreshToken == "" {
		return nil, nil
	}
	if psd == nil {
		psd = map[string]any{}
	}
	authMethod, _ := psd["authMethod"].(string)
	clientID, _ := psd["clientId"].(string)
	clientSecret, _ := psd["clientSecret"].(string)
	region, _ := psd["region"].(string)

	if authMethod == "external_idp" {
		log.Warn("kiro external_idp refresh branch not yet ported")
		return nil, ErrExternalIDPNotPorted
	}

	if clientID != "" && clientSecret != "" {
		return k.refreshAWS(ctx, refreshToken, clientID, clientSecret, authMethod, region, opts, log)
	}
	return k.refreshSocial(ctx, refreshToken, opts, log)
}

func (k *KiroRefresher) refreshAWS(ctx context.Context, refreshToken, clientID, clientSecret, authMethod, region string, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	endpoint := kiroDefaultOIDC
	if authMethod == "idc" && region != "" {
		endpoint = fmt.Sprintf(kiroAWSOIDCBase, region)
	}
	body, _ := json.Marshal(map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	var resp struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}
	if err := k.do(req, opts, &resp, log, "Kiro AWS"); err != nil {
		return nil, err
	}
	out := &resolver.RefreshedCredentials{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ExpiresIn:    resp.ExpiresIn,
	}
	if resp.RefreshToken == "" {
		out.RefreshToken = refreshToken
	}
	// Carry profileArn through ProviderSpecificData so the resolver can
	// resolve the region on the next ListAvailableModels call.
	if resp.ProfileArn != "" {
		// Merge into psd via the OnCredentialsRefreshed hook at the call site;
		// here we only return token fields. profileArn patching (the JS
		// resolveKiroProfileArnPatch path that fetches a missing profileArn)
		// is a follow-up.
	}
	return out, nil
}

func (k *KiroRefresher) refreshSocial(ctx context.Context, refreshToken string, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	body, _ := json.Marshal(map[string]string{"refreshToken": refreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kiroSocialTokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kiro-cli/1.0.0")

	var resp struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
	}
	if err := k.do(req, opts, &resp, log, "Kiro social"); err != nil {
		return nil, err
	}
	out := &resolver.RefreshedCredentials{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ExpiresIn:    resp.ExpiresIn,
	}
	if out.RefreshToken == "" {
		out.RefreshToken = refreshToken
	}
	return out, nil
}

// do executes the refresh request, classifies failures, and decodes the JSON
// response into dst. A non-2xx response is an error (caller falls back). opts
// routes the request through the proxy stack when set (Fix 2a).
func (k *KiroRefresher) do(req *http.Request, opts resolver.ProxyOptions, dst any, log resolver.Logger, label string) error {
	resp, err := routeAwareDo(req.Context(), k.client, req, opts)
	if err != nil {
		log.Warn("token refresh network error", "label", label, "error", err)
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		log.Warn("token refresh failed", "label", label, "status", resp.StatusCode, "body", string(respBody))
		return fmt.Errorf("%s refresh %d: %s", label, resp.StatusCode, string(respBody))
	}
	if err := json.Unmarshal(respBody, dst); err != nil {
		log.Warn("token refresh decode error", "label", label, "error", err)
		return err
	}
	log.Info("token refreshed", "label", label)
	return nil
}