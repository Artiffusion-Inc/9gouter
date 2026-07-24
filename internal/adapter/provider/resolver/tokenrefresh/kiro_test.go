package tokenrefresh

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/resolver"
)

// hostSwapTransport rewrites every request's host to the test server's
// URL, so the production OIDC / social token endpoints are redirected to
// the httptest server without exposing the constants as overridable fields.
type hostSwapTransport struct {
	base http.RoundTripper
	to   *url.URL
}

func (t hostSwapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = t.to.Scheme
	req.URL.Host = t.to.Host
	req.Host = t.to.Host
	return t.base.RoundTrip(req)
}

func hostSwapTransportFn(to string) http.RoundTripper {
	u, err := url.Parse(to)
	if err != nil {
		panic(err)
	}
	return hostSwapTransport{base: http.DefaultTransport, to: u}
}

func TestKiroRefresh_EmptyToken(t *testing.T) {
	k := NewKiroRefresher()
	out, err := k.Refresh(context.Background(), "", nil, resolver.ProxyOptions{}, resolver.NopLogger())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out != nil {
		t.Errorf("expected nil for empty refreshToken, got %+v", out)
	}
}

func TestKiroRefresh_ExternalIDPConfigInvalid(t *testing.T) {
	// external_idp without clientId/scope/endpoint is a hard config error,
	// not a silent fallback to the wrong (social/AWS) branch.
	k := NewKiroRefresher()
	_, err := k.Refresh(context.Background(), "rt", map[string]any{"authMethod": "external_idp"}, resolver.ProxyOptions{}, resolver.NopLogger())
	if err == nil {
		t.Fatal("expected config error for external_idp without clientId/scope/endpoint")
	}
}

// TestKiroRefresh_ExternalIDPBranch verifies the a4f44e3e external_idp refresh:
// form-encoded refresh_token grant to a Microsoft login endpoint, returning the
// refreshed tokens + normalized providerSpecificData. The Microsoft host is
// redirected to the httptest server via a host-swap transport; the test
// validates the form body + Content-Type and that psd is carried back.
func TestKiroRefresh_ExternalIDPBranch(t *testing.T) {
	var gotCT string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "ext-at",
			"refresh_token": "ext-rt",
			"expires_in":    7200,
		})
	}))
	defer srv.Close()

	k := NewKiroRefresher()
	k.client = srv.Client()
	k.client.Transport = hostSwapTransportFn(srv.URL)

	out, err := k.Refresh(context.Background(), "rt", map[string]any{
		"authMethod":    "external_idp",
		"clientId":      "ms-cid",
		"tokenEndpoint": "https://login.microsoftonline.com/tenant/oauth2/v2.0/token",
		"scope":         "https://api.example.com/.default offline_access",
	}, resolver.ProxyOptions{}, resolver.NopLogger())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out.AccessToken != "ext-at" {
		t.Errorf("accessToken = %q, want ext-at", out.AccessToken)
	}
	if out.RefreshToken != "ext-rt" {
		t.Errorf("refreshToken = %q, want ext-rt", out.RefreshToken)
	}
	if out.ExpiresIn != 7200 {
		t.Errorf("expiresIn = %d, want 7200", out.ExpiresIn)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want form-encoded", gotCT)
	}
	// Form body carries grant_type + client_id + refresh_token + scope.
	parsed, err := url.ParseQuery(gotBody)
	if err != nil {
		t.Fatalf("body not form-encoded: %v", err)
	}
	if parsed.Get("grant_type") != "refresh_token" {
		t.Errorf("grant_type = %q", parsed.Get("grant_type"))
	}
	if parsed.Get("client_id") != "ms-cid" {
		t.Errorf("client_id = %q", parsed.Get("client_id"))
	}
	if parsed.Get("refresh_token") != "rt" {
		t.Errorf("refresh_token = %q", parsed.Get("refresh_token"))
	}
	if parsed.Get("scope") != "https://api.example.com/.default offline_access" {
		t.Errorf("scope = %q", parsed.Get("scope"))
	}
	// providerSpecificData is carried back, re-stamped + normalized.
	if out.ProviderSpecificData == nil {
		t.Fatal("ProviderSpecificData missing (should carry normalized psd back)")
	}
	if am, _ := out.ProviderSpecificData["authMethod"].(string); am != "external_idp" {
		t.Errorf("psd authMethod = %q, want external_idp", am)
	}
	if cid, _ := out.ProviderSpecificData["clientId"].(string); cid != "ms-cid" {
		t.Errorf("psd clientId = %q, want ms-cid", cid)
	}
	if te, _ := out.ProviderSpecificData["tokenEndpoint"].(string); te != "https://login.microsoftonline.com/tenant/oauth2/v2.0/token" {
		t.Errorf("psd tokenEndpoint = %q, want the validated endpoint", te)
	}
}

// TestKiroRefresh_ExternalIDPNon200 verifies a non-2xx Microsoft response is an
// error (caller falls back to the static catalog / marks re-auth).
func TestKiroRefresh_ExternalIDPNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	k := NewKiroRefresher()
	k.client = srv.Client()
	k.client.Transport = hostSwapTransportFn(srv.URL)

	_, err := k.Refresh(context.Background(), "rt", map[string]any{
		"authMethod":    "external_idp",
		"clientId":      "ms-cid",
		"tokenEndpoint": "https://login.microsoft.com/tenant/oauth2/v2.0/token",
		"scope":         "offline_access",
	}, resolver.ProxyOptions{}, resolver.NopLogger())
	if err == nil {
		t.Fatal("expected error on 401 from Microsoft endpoint")
	}
}

// TestKiroRefresh_ExternalIDPKeepOriginalRefreshToken verifies that when the
// Microsoft response omits refresh_token, the original is preserved.
func TestKiroRefresh_ExternalIDPKeepOriginalRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "ext-at",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	k := NewKiroRefresher()
	k.client = srv.Client()
	k.client.Transport = hostSwapTransportFn(srv.URL)

	out, err := k.Refresh(context.Background(), "orig-rt", map[string]any{
		"authMethod":    "external_idp",
		"clientId":      "ms-cid",
		"tokenEndpoint": "https://login.windows.net/tenant/oauth2/v2.0/token",
		"scope":         "offline_access",
	}, resolver.ProxyOptions{}, resolver.NopLogger())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out.RefreshToken != "orig-rt" {
		t.Errorf("refreshToken = %q, want original orig-rt", out.RefreshToken)
	}
}

// TestKiroRefresh_AWSBranch verifies the AWS SSO branch (clientId +
// clientSecret present) POSTs to the OIDC endpoint with the JSON body and
// returns the refreshed tokens.
func TestKiroRefresh_AWSBranch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["grantType"] != "refresh_token" {
			t.Errorf("grantType = %q", body["grantType"])
		}
		if body["refreshToken"] != "rt" {
			t.Errorf("refreshToken = %q", body["refreshToken"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken":  "new-at",
			"refreshToken": "new-rt",
			"expiresIn":    3600,
		})
	}))
	defer srv.Close()

	k := NewKiroRefresher()
	// Redirect the AWS endpoint to the test server by overriding the
	// kiroDefaultOIDC constant indirectly: we cannot, so instead test the
	// social branch which has a dedicated URL we can override... but that
	// is also a const. Use a transport that rewrites to the test server.
	k.client = srv.Client()
	// Patch the endpoint via a client transport rewrite is fragile; instead
	// set k.client to one whose CheckRedirect/transport maps the OIDC host.
	// Simplest: install a transport that swaps the host.
	k.client.Transport = hostSwapTransportFn(srv.URL)

	out, err := k.Refresh(context.Background(), "rt", map[string]any{
		"clientId":     "cid",
		"clientSecret": "csec",
	}, resolver.ProxyOptions{}, resolver.NopLogger())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out.AccessToken != "new-at" {
		t.Errorf("accessToken = %q", out.AccessToken)
	}
	if out.RefreshToken != "new-rt" {
		t.Errorf("refreshToken = %q", out.RefreshToken)
	}
}

// TestKiroRefresh_SocialBranch verifies the social branch (no
// clientId/secret) POSTs to the social tokenUrl with {refreshToken} and the
// kiro-cli User-Agent.
func TestKiroRefresh_SocialBranch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != "kiro-cli/1.0.0" {
			t.Errorf("User-Agent = %q, want kiro-cli/1.0.0", ua)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["refreshToken"] != "rt" {
			t.Errorf("refreshToken = %q", body["refreshToken"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "social-at",
			"expiresIn":   1800,
		})
	}))
	defer srv.Close()

	k := NewKiroRefresher()
	k.client = srv.Client()
	k.client.Transport = hostSwapTransportFn(srv.URL)

	out, err := k.Refresh(context.Background(), "rt", map[string]any{}, resolver.ProxyOptions{}, resolver.NopLogger())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out.AccessToken != "social-at" {
		t.Errorf("accessToken = %q, want social-at", out.AccessToken)
	}
	// No new refreshToken returned -> should keep the original.
	if out.RefreshToken != "rt" {
		t.Errorf("refreshToken = %q, want original rt", out.RefreshToken)
	}
}

// TestKiroRefresh_Non200 verifies a non-2xx response is an error (caller
// falls back to the static catalog).
func TestKiroRefresh_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	k := NewKiroRefresher()
	k.client = srv.Client()
	k.client.Transport = hostSwapTransportFn(srv.URL)

	_, err := k.Refresh(context.Background(), "rt", map[string]any{}, resolver.ProxyOptions{}, resolver.NopLogger())
	if err == nil {
		t.Fatal("expected error on 400")
	}
}
