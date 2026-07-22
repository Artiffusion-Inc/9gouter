// kimi.go ports refreshKimiToken from open-sse/services/tokenRefresh/providers.js
// (upstream v0.5.40, commit 68566f53): a form-encoded grant_type=refresh_token
// POST with NO client_secret, carrying the X-Msh-* headers required by the
// CLIProxyAPI Kimi auth parity. The device id must stay stable per connection
// for the whole OAuth session, so it is read from the connection's
// ProviderSpecificData["deviceId"] (set at device-code acquisition) and forwarded
// to buildKimiHeaders; when absent (api-key auth, or a token imported without a
// stored device id), the headers carry a generated id — the refresh itself does
// not require a specific device id, only the header presence.
package tokenrefresh

import (
	"context"
	"net/http"
	"net/url"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/default"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/resolver"
)

// Kimi OAuth endpoint + client constants — copied verbatim from the upstream
// registry kimi.js oauth block (commit 68566f53). Public client id; no secret.
const (
	kimiTokenURL   = "https://auth.kimi.com/api/oauth/token"
	kimiRefreshURL = "https://auth.kimi.com/api/oauth/token"
	kimiClientID   = "17e5f671-d194-4dfb-9706-5516cb48c098"
)

// KimiRefresher refreshes a Kimi Code OAuth token. Mirrors refreshKimiToken:
// form-encoded body {grant_type, refresh_token, client_id} + X-Msh-* headers.
type KimiRefresher struct{ httpClient *http.Client }

func NewKimiRefresher() *KimiRefresher { return &KimiRefresher{httpClient: newRefreshClient()} }

func (r *KimiRefresher) Refresh(ctx context.Context, refreshToken string, psd map[string]any, opts resolver.ProxyOptions, log resolver.Logger) (*resolver.RefreshedCredentials, error) {
	if refreshToken == "" {
		return nil, nil
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {kimiClientID},
	}
	hdr := http.Header{}
	defaultexec.BuildKimiHeaders(hdr, kimiDeviceIDFromPSD(psd))
	tok, err := doForm(ctx, r.httpClient, opts, kimiRefreshURL, form, hdr, log, "Kimi")
	if err != nil {
		return nil, err
	}
	out := fromToken(tok, refreshToken, false)
	// Preserve the stable device id across the refresh so subsequent refreshes
	// and upstream chat requests keep the same X-Msh-Device-Id.
	if id := kimiDeviceIDFromPSD(psd); id != "" {
		if out.ProviderSpecificData == nil {
			out.ProviderSpecificData = map[string]any{}
		}
		out.ProviderSpecificData["deviceId"] = id
	}
	return out, nil
}

// kimiDeviceIDFromPSD reads the stable Kimi device id from the connection's
// provider-specific data. Returns "" when none is stored (the header builder
// then generates a timestamp id).
func kimiDeviceIDFromPSD(psd map[string]any) string {
	id, ok := stringField(psd, "deviceId")
	if !ok {
		return ""
	}
	return id
}
