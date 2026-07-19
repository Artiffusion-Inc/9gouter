package tokenrefresh

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/resolver"
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
	out, err := k.Refresh(context.Background(), "", nil, resolver.NopLogger())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out != nil {
		t.Errorf("expected nil for empty refreshToken, got %+v", out)
	}
}

func TestKiroRefresh_ExternalIDPNotPorted(t *testing.T) {
	k := NewKiroRefresher()
	_, err := k.Refresh(context.Background(), "rt", map[string]any{"authMethod": "external_idp"}, resolver.NopLogger())
	if err != ErrExternalIDPNotPorted {
		t.Fatalf("err = %v, want ErrExternalIDPNotPorted", err)
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
	}, resolver.NopLogger())
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

	out, err := k.Refresh(context.Background(), "rt", map[string]any{}, resolver.NopLogger())
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

	_, err := k.Refresh(context.Background(), "rt", map[string]any{}, resolver.NopLogger())
	if err == nil {
		t.Fatal("expected error on 400")
	}
}