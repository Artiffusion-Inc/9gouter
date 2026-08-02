// Package oauth ports the static OAuth provider configurations from
// open-sse/providers/registry/*.js (the `oauth:` blocks) and
// src/lib/oauth/constants/oauth.js. It provides authorize URL generation,
// token exchange, device code request/poll, and PKCE generation.
//
// The package mirrors the JS provider flow types:
//   - authorization_code_pkce: claude, codex, xai, qwen (device), qoder
//   - authorization_code: antigravity, gemini-cli, cline, clinepass, iflow, gitlab
//   - device_code: github, qwen, kimi, kilocode, codebuddy-cn, codebuddy-intl, grok-cli
//   - browser_token: kimchi
//
// Currently implemented: authorize + exchange for PKCE and non-PKCE auth code
// providers; device-code + poll for github and qwen.
package oauth

// FlowType is the OAuth flow type a provider uses.
type FlowType string

const (
	FlowAuthCodePKCE FlowType = "authorization_code_pkce"
	FlowAuthCode     FlowType = "authorization_code"
	FlowDeviceCode   FlowType = "device_code"
	FlowBrowserToken FlowType = "browser_token"
)

// ProviderConfig is the static OAuth configuration for a provider, mirroring
// the `oauth:` block in the JS registry.
type ProviderConfig struct {
	FlowType           FlowType
	AuthorizeURL       string   // authorization endpoint
	TokenURL           string   // token endpoint
	DeviceCodeURL      string   // device code endpoint (device_code flow)
	UserInfoURL        string   // optional user info endpoint
	ClientID           string   // public client id (no secret for PKCE)
	ClientSecret       string   // confidential client secret (non-PKCE)
	Scope              string   // space-delimited scope string
	Scopes             []string // scope list (joined with space)
	CodeChallengeMethod string   // "S256" or "" for non-PKCE
	FixedPort          int      // loopback callback port (0 = any)
	CallbackPath       string   // callback path (default "/callback")
	ExtraParams        map[string]string // extra authorize params (codex)
}

// providerConfigs is the static table of OAuth provider configurations.
var providerConfigs = map[string]ProviderConfig{
	"claude": {
		FlowType:            FlowAuthCodePKCE,
		AuthorizeURL:        "https://claude.ai/oauth/authorize",
		TokenURL:            "https://api.anthropic.com/v1/oauth/token",
		ClientID:            "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		Scopes:              []string{"org:create_api_key", "user:profile", "user:inference"},
		CodeChallengeMethod: "S256",
		CallbackPath:        "/callback",
	},
	"xai": {
		FlowType:            FlowAuthCodePKCE,
		AuthorizeURL:        "https://auth.x.ai/oauth2/authorize",
		TokenURL:            "https://auth.x.ai/oauth2/token",
		ClientID:            "b1a00492-073a-47ea-816f-4c329264a828",
		Scope:               "offline_access",
		CodeChallengeMethod: "S256",
		FixedPort:           1455,
		CallbackPath:        "/callback",
	},
	"codex": {
		FlowType:            FlowAuthCodePKCE,
		AuthorizeURL:        "https://auth.openai.com/oauth/authorize",
		TokenURL:            "https://auth.openai.com/oauth/token",
		ClientID:            "app_EMoamEEZ73f0CkXaXp7hrann",
		Scope:               "openid profile email offline_access",
		CodeChallengeMethod: "S256",
		FixedPort:           1455,
		CallbackPath:        "/auth/callback",
		ExtraParams: map[string]string{
			"id_token_add_organizations": "true",
			"codex_cli_simplified_flow":  "true",
			"originator":                 "codex_cli_rs",
		},
	},
	"antigravity": {
		FlowType:     FlowAuthCode,
		AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:     "https://oauth2.googleapis.com/token",
		ClientID:     "9364752724.apps.googleusercontent.com",
		Scopes: []string{
			"https://www.googleapis.com/auth/cloud-platform",
			"https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/userinfo.profile",
			"https://www.googleapis.com/auth/cclog",
			"https://www.googleapis.com/auth/experimentsandconfigs",
		},
		CallbackPath: "/callback",
	},
	"gemini-cli": {
		FlowType:     FlowAuthCode,
		AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:     "https://oauth2.googleapis.com/token",
		ClientID:     "32555940559.apps.googleusercontent.com",
		Scopes: []string{
			"https://www.googleapis.com/auth/cloud-platform",
			"https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/userinfo.profile",
		},
		CallbackPath: "/callback",
	},
	"github": {
		FlowType:      FlowDeviceCode,
		AuthorizeURL:  "https://github.com/login/oauth/authorize",
		DeviceCodeURL: "https://github.com/login/device/code",
		TokenURL:      "https://github.com/login/oauth/access_token",
		UserInfoURL:   "https://api.github.com/user",
		ClientID:      "Iv1.b507a08c87ecfe98",
		Scope:         "read:user",
	},
	"qwen": {
		FlowType:            FlowDeviceCode,
		DeviceCodeURL:       "https://chat.qwen.ai/api/v1/oauth2/device/code",
		TokenURL:            "https://chat.qwen.ai/api/v1/oauth2/token",
		ClientID:            "f0304373b74a44d2b584a3fb70ca9e56",
		Scope:               "openid profile email model.completion",
		CodeChallengeMethod: "S256",
	},
	"cline": {
		FlowType:     FlowAuthCode,
		AuthorizeURL: "https://api.cline.bot/api/v1/auth/authorize",
		TokenURL:     "https://api.cline.bot/api/v1/auth/token",
		CallbackPath: "/callback",
	},
	"clinepass": {
		FlowType:     FlowAuthCode,
		AuthorizeURL: "https://api.cline.bot/api/v1/auth/authorize",
		TokenURL:     "https://api.cline.bot/api/v1/auth/token",
		CallbackPath: "/callback",
	},
}

// Lookup returns the OAuth config for a provider id, or false if unknown.
func Lookup(providerID string) (ProviderConfig, bool) {
	cfg, ok := providerConfigs[providerID]
	return cfg, ok
}

// ScopeString returns the scope string for the config (Scope field or
// Scopes joined with space).
func (c ProviderConfig) ScopeString() string {
	if c.Scope != "" {
		return c.Scope
	}
	if len(c.Scopes) > 0 {
		result := ""
		for i, s := range c.Scopes {
			if i > 0 {
				result += " "
			}
			result += s
		}
		return result
	}
	return ""
}

// CallbackPathOr returns the callback path or the default if empty.
func (c ProviderConfig) CallbackPathOr(def string) string {
	if c.CallbackPath != "" {
		return c.CallbackPath
	}
	return def
}
