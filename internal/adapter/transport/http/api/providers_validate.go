package api

// providers_validate.go ports the key-validation half of upstream 9102c4c6
// (xiaomi-tokenplan: region selector, key validation, multi-connection #2251):
// POST /api/providers/validate probes the upstream /models endpoint with the
// caller's Bearer apiKey (8s timeout) and reports whether the key is valid.
//
// The legacy JS validate route (src/app/api/providers/validate/route.js) is a
// ~400-line switch over dozens of providers, most of which have no Go-side
// registry entry yet. The Go rewrite shipped a stub that returned {valid:true}
// unconditionally — every key looked valid. This port replaces the stub with a
// real probe for the providers the Go build actually serves today (xiaomi-tokenplan
// + a generic OpenAI-compat fallback that hits /models). For xiaomi-tokenplan
// the /models endpoint returns 403 for a valid key that lacks list-permission
// (only 401 means invalid), mirroring the JS fix. Unknown providers fall back
// to a /models probe with res.ok (2xx), preserving the previous "no specific
// branch" behaviour without the unconditional-true lie.

import (
	"context"
	"net/http"
	"strings"
	"time"

	adapterprovider "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider"
)

// validateRequest is the POST /api/providers/validate body the dashboard sends.
type validateRequest struct {
	Provider             string         `json:"provider"`
	APIKey               string         `json:"apiKey"`
	ProviderSpecificData map[string]any `json:"providerSpecificData"`
}

// validateResponse is the {valid, ...} shape the dashboard reads.
type validateResponse struct {
	Valid    bool   `json:"valid"`
	Provider string `json:"provider,omitempty"`
	Status   int    `json:"status,omitempty"`
	Error    string `json:"error,omitempty"`
}

// validateTimeout is the upstream probe timeout. Mirrors the JS
// AbortSignal.timeout(8000) from #2251 — a dead key/host must not hang the
// dashboard's validate button.
const validateTimeout = 8 * time.Second

// resolveXiaomiTokenplanBaseURL picks the region-specific base URL from
// providerSpecificData["region"], mirroring the Xiaomi Tokenplan executor's
// BuildURL region resolution (internal/adapter/provider/xiaomi-tokenplan). A
// providerSpecificData["baseUrl"] override wins (custom-region hosts / test
// stubs); otherwise the known regional hosts are matched by substring and the
// SGP region is the fallback.
func resolveXiaomiTokenplanBaseURL(psd map[string]any) string {
	if override, ok := psd["baseUrl"].(string); ok && override != "" {
		return strings.TrimSuffix(override, "/")
	}
	region, _ := psd["region"].(string)
	for _, u := range []string{
		"https://token-plan-sgp.xiaomimimo.com/v1",
		"https://token-plan-cn.xiaomimimo.com/v1",
		"https://token-plan-ams.xiaomimimo.com/v1",
	} {
		if strings.Contains(u, region) || region == "" {
			return strings.TrimSuffix(u, "/")
		}
	}
	return "https://token-plan-sgp.xiaomimimo.com/v1"
}

// validateHTTPClient is the upstream probe client. Overridable in tests so a
// probe can be pointed at an httptest.Server instead of a real host. Defaults
// to http.DefaultClient in production.
var validateHTTPClient = http.DefaultClient

// validateProbe performs the upstream /models GET with Bearer auth and returns
// the HTTP status (or -1 + error on transport failure). The 8s timeout is
// enforced via context.
func validateProbe(ctx context.Context, url, apiKey string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return -1, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := validateHTTPClient.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// evaluateValidateStatus maps the upstream /models status to a valid/invalid
// verdict per the provider's validation rule. xiaomi-tokenplan treats 403 as
// a valid key (no list-permission); only 401 is invalid. Generic providers use
// res.ok (2xx).
func evaluateValidateStatus(provider string, status int) bool {
	switch provider {
	case "xiaomi-tokenplan":
		// /models returns 403 for valid keys lacking list permission; only 401
		// means invalid.
		return status != http.StatusUnauthorized
	case "xai":
		// xai returns 400 for a bad key, 403 for valid-but-no-credit.
		return status == http.StatusOK || status == http.StatusForbidden
	default:
		return status >= 200 && status < 300
	}
}

// validateURLForProvider builds the upstream /models probe URL for a provider.
// xiaomi-tokenplan resolves a region-specific base; other providers fall back
// to the registered provider BaseURL (stripped of a /chat/completions or
// /messages suffix and appended /models). Returns "" when no URL can be
// resolved, in which case the handler degrades to unconditional-true (the
// previous stub behaviour) rather than blocking the UI.
func validateURLForProvider(provider string, psd map[string]any) string {
	if provider == "xiaomi-tokenplan" {
		return resolveXiaomiTokenplanBaseURL(psd) + "/models"
	}
	// Use the provider's registered chat BaseURL; strip a known chat suffix and
	// probe /models. This covers generic OpenAI-compat providers that expose
	// a /models list endpoint alongside /chat/completions.
	base := adapterprovider.ChatBaseURL(provider)
	if base == "" {
		return ""
	}
	base = strings.TrimSuffix(base, "/")
	for _, suffix := range []string{"/chat/completions", "/messages", "/completions"} {
		base = strings.TrimSuffix(base, suffix)
	}
	return strings.TrimSuffix(base, "/") + "/models"
}
