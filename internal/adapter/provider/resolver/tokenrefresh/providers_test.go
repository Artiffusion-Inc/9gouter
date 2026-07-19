package tokenrefresh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/resolver"
)

// refreshRecorder captures the inbound request and replies with a canned
// body + status. Tests configure the response before the call.
type refreshRecorder struct {
	body   string
	status int
	got    *http.Request
	gotBody string
}

func newRefreshServer(t *testing.T, rec *refreshRecorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.got = r
		b, _ := io.ReadAll(r.Body)
		rec.gotBody = string(b)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write([]byte(rec.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// refreshClientSetter is the tiny interface the test helpers use to inject a
// per-test http.Client. Each refresher implements it via setClient.
type refreshClientSetter interface {
	setClient(*http.Client)
}

func (r *ClaudeRefresher) setClient(c *http.Client)     { r.httpClient = c }
func (r *GoogleRefresher) setClient(c *http.Client)     { r.httpClient = c }
func (r *QwenRefresher) setClient(c *http.Client)      { r.httpClient = c }
func (r *CodexRefresher) setClient(c *http.Client)     { r.httpClient = c }
func (r *IflowRefresher) setClient(c *http.Client)     { r.httpClient = c }
func (r *GitHubRefresher) setClient(c *http.Client)    { r.httpClient = c }
func (r *CopilotRefresher) setClient(c *http.Client)   { r.httpClient = c }
func (r *CodebuddyRefresher) setClient(c *http.Client) { r.httpClient = c }
func (r *XaiRefresher) setClient(c *http.Client)       { r.httpClient = c }
func (r *GenericRefresher) setClient(c *http.Client)   { r.httpClient = c }

// pointClient redirects a refresher's http client at the test server by
// wrapping the test server's transport with hostSwapTransport (defined in
// kiro_test.go) so the production endpoint host is rewritten to the test
// server without exposing the URL constants as overridable fields.
func pointClient(srv *httptest.Server, r refreshClientSetter) {
	c := srv.Client()
	c.Transport = hostSwapTransportFn(srv.URL)
	r.setClient(c)
}

// typeName returns the short "*pkg.Type" form of a refresher for Lookup tests.
func typeName(r resolver.TokenRefresher) string {
	s := fmt.Sprintf("%T", r)
	// "*tokenrefresh.X" -> "*tokenrefresh.X" already; strip module path if any.
	return s
}

// TestClaudeRefresh verifies the JSON body shape (grant_type, refresh_token,
// client_id) and parsing of access_token / refresh_token rotation / expires_in.
func TestClaudeRefresh(t *testing.T) {
	rec := &refreshRecorder{body: `{"access_token":"at","refresh_token":"rt2","expires_in":3600}`}
	srv := newRefreshServer(t, rec)
	r := NewClaudeRefresher()
	pointClient(srv, r)

	out, err := r.Refresh(context.Background(), "rt", nil, resolver.NopLogger())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.AccessToken != "at" {
		t.Errorf("AccessToken=%q want at", out.AccessToken)
	}
	if out.RefreshToken != "rt2" {
		t.Errorf("RefreshToken=%q want rt2", out.RefreshToken)
	}
	if out.ExpiresIn != 3600 {
		t.Errorf("ExpiresIn=%d want 3600", out.ExpiresIn)
	}
	// Body must be JSON with the three fields.
	var body map[string]string
	_ = json.Unmarshal([]byte(rec.gotBody), &body)
	if body["grant_type"] != "refresh_token" || body["refresh_token"] != "rt" || body["client_id"] != claudeClientID {
		t.Errorf("claude body mismatch: %s", rec.gotBody)
	}
	if rec.got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type=%q", rec.got.Header.Get("Content-Type"))
	}
}

// TestClaudeRefresh_EmptyToken returns nil,nil (no-op) for an empty refresh.
func TestClaudeRefresh_EmptyToken(t *testing.T) {
	r := NewClaudeRefresher()
	out, err := r.Refresh(context.Background(), "", nil, resolver.NopLogger())
	if err != nil || out != nil {
		t.Fatalf("expected nil,nil got %v %v", out, err)
	}
}

// TestClaudeRefresh_PreservesRefreshTokenWhenNotRotated verifies the
// refresh_token fallback to the original when upstream omits it.
func TestClaudeRefresh_PreservesRefreshTokenWhenNotRotated(t *testing.T) {
	rec := &refreshRecorder{body: `{"access_token":"at","expires_in":3600}`}
	srv := newRefreshServer(t, rec)
	r := NewClaudeRefresher()
	pointClient(srv, r)
	out, _ := r.Refresh(context.Background(), "orig-rt", nil, resolver.NopLogger())
	if out.RefreshToken != "orig-rt" {
		t.Errorf("RefreshToken=%q want orig-rt", out.RefreshToken)
	}
}

// TestGoogleRefresh verifies clientId/clientSecret come from psd and the body
// is form-encoded.
func TestGoogleRefresh(t *testing.T) {
	rec := &refreshRecorder{body: `{"access_token":"at","expires_in":3600}`}
	srv := newRefreshServer(t, rec)
	r := NewGoogleRefresher()
	pointClient(srv, r)
	psd := map[string]any{"clientId": "gcid", "clientSecret": "gsecret"}
	out, err := r.Refresh(context.Background(), "rt", psd, resolver.NopLogger())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.AccessToken != "at" {
		t.Errorf("AccessToken=%q", out.AccessToken)
	}
	form, _ := url.ParseQuery(rec.gotBody)
	if form.Get("client_id") != "gcid" || form.Get("client_secret") != "gsecret" {
		t.Errorf("google form mismatch: %s", rec.gotBody)
	}
	if form.Get("grant_type") != "refresh_token" {
		t.Errorf("grant_type=%q", form.Get("grant_type"))
	}
}

// TestQwenRefresh_ResourceURL verifies resource_url is carried through psd.
func TestQwenRefresh_ResourceURL(t *testing.T) {
	rec := &refreshRecorder{body: `{"access_token":"at","refresh_token":"r2","resource_url":"https://dashscope.example/","expires_in":3600}`}
	srv := newRefreshServer(t, rec)
	r := NewQwenRefresher()
	pointClient(srv, r)
	out, _ := r.Refresh(context.Background(), "rt", nil, resolver.NopLogger())
	if out.ProviderSpecificData["resourceUrl"] != "https://dashscope.example/" {
		t.Errorf("resourceUrl not carried: %v", out.ProviderSpecificData)
	}
	form, _ := url.ParseQuery(rec.gotBody)
	if form.Get("client_id") != qwenClientID {
		t.Errorf("qwen client_id=%q", form.Get("client_id"))
	}
}

// TestCodexRefresh_IDToken verifies id_token is carried through.
func TestCodexRefresh_IDToken(t *testing.T) {
	rec := &refreshRecorder{body: `{"access_token":"at","refresh_token":"r2","id_token":"idt","expires_in":3600}`}
	srv := newRefreshServer(t, rec)
	r := NewCodexRefresher()
	pointClient(srv, r)
	out, _ := r.Refresh(context.Background(), "rt", nil, resolver.NopLogger())
	if out.IDToken != "idt" {
		t.Errorf("IDToken=%q want idt", out.IDToken)
	}
}

// TestCodexRefresh_PermanentFailure verifies invalid_grant is classified as
// Unrecoverable so the caller marks re-auth.
func TestCodexRefresh_PermanentFailure(t *testing.T) {
	rec := &refreshRecorder{status: http.StatusBadRequest, body: `{"error":"invalid_grant","error_description":"refresh_token_expired"}`}
	srv := newRefreshServer(t, rec)
	r := NewCodexRefresher()
	pointClient(srv, r)
	out, err := r.Refresh(context.Background(), "rt", nil, resolver.NopLogger())
	if err == nil {
		t.Fatal("expected error for invalid_grant")
	}
	if out == nil || !out.Unrecoverable {
		t.Fatal("expected Unrecoverable=true for invalid_grant")
	}
}

// TestIflowRefresh_BasicAuth verifies HTTP Basic auth header (clientId:secret).
func TestIflowRefresh_BasicAuth(t *testing.T) {
	rec := &refreshRecorder{body: `{"access_token":"at","expires_in":3600}`}
	srv := newRefreshServer(t, rec)
	r := NewIflowRefresher()
	pointClient(srv, r)
	_, err := r.Refresh(context.Background(), "rt", nil, resolver.NopLogger())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.got.Header.Get("Authorization") == "" || !strings.HasPrefix(rec.got.Header.Get("Authorization"), "Basic ") {
		t.Errorf("iFlow missing Basic auth: %q", rec.got.Header.Get("Authorization"))
	}
}

// TestGitHubRefresh_OptionalSecret verifies client_secret is included only when
// configured.
func TestGitHubRefresh_OptionalSecret(t *testing.T) {
	rec := &refreshRecorder{body: `{"access_token":"at","expires_in":3600}`}
	srv := newRefreshServer(t, rec)
	r := NewGitHubRefresher()
	pointClient(srv, r)
	_, _ = r.Refresh(context.Background(), "rt", map[string]any{"clientSecret": "ghs"}, resolver.NopLogger())
	form, _ := url.ParseQuery(rec.gotBody)
	if form.Get("client_secret") != "ghs" {
		t.Errorf("expected client_secret=ghs, body=%s", rec.gotBody)
	}
	if form.Get("client_id") != githubClientID {
		t.Errorf("github client_id=%q", form.Get("client_id"))
	}
}

// TestGitHubRefresh_NoSecret verifies a public client omits client_secret.
func TestGitHubRefresh_NoSecret(t *testing.T) {
	rec := &refreshRecorder{body: `{"access_token":"at","expires_in":3600}`}
	srv := newRefreshServer(t, rec)
	r := NewGitHubRefresher()
	pointClient(srv, r)
	_, _ = r.Refresh(context.Background(), "rt", nil, resolver.NopLogger())
	form, _ := url.ParseQuery(rec.gotBody)
	if _, ok := form["client_secret"]; ok {
		t.Errorf("public client must not send client_secret: %s", rec.gotBody)
	}
}

// TestCopilotRefresh verifies the GET + editor headers + {token, expires_at}
// response shape.
func TestCopilotRefresh(t *testing.T) {
	rec := &refreshRecorder{body: `{"token":"cop-tok","expires_at":"2026-12-31T23:59:59Z"}`}
	srv := newRefreshServer(t, rec)
	r := NewCopilotRefresher()
	pointClient(srv, r)
	out, err := r.Refresh(context.Background(), "gh-at", nil, resolver.NopLogger())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.AccessToken != "cop-tok" {
		t.Errorf("AccessToken=%q", out.AccessToken)
	}
	if out.ExpiresAt != "2026-12-31T23:59:59Z" {
		t.Errorf("ExpiresAt=%q", out.ExpiresAt)
	}
	if rec.got.Method != http.MethodGet {
		t.Errorf("method=%q want GET", rec.got.Method)
	}
	if rec.got.Header.Get("Authorization") != "token gh-at" {
		t.Errorf("Authorization=%q", rec.got.Header.Get("Authorization"))
	}
	if rec.got.Header.Get("Editor-Version") != "vscode/1.110.0" {
		t.Errorf("Editor-Version=%q", rec.got.Header.Get("Editor-Version"))
	}
	if rec.got.Header.Get("x-github-api-version") != "2025-04-01" {
		t.Errorf("api version=%q", rec.got.Header.Get("x-github-api-version"))
	}
}

// TestCodebuddyRefresh verifies the X-Refresh-Token header + nested {code,data}
// response shape.
func TestCodebuddyRefresh(t *testing.T) {
	rec := &refreshRecorder{body: `{"code":0,"data":{"accessToken":"cb-at","refreshToken":"r2","expiresIn":3600}}`}
	srv := newRefreshServer(t, rec)
	r := NewCodebuddyRefresher()
	pointClient(srv, r)
	out, err := r.Refresh(context.Background(), "rt", nil, resolver.NopLogger())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.AccessToken != "cb-at" {
		t.Errorf("AccessToken=%q want cb-at", out.AccessToken)
	}
	if rec.got.Header.Get("X-Refresh-Token") != "rt" {
		t.Errorf("X-Refresh-Token=%q want rt", rec.got.Header.Get("X-Refresh-Token"))
	}
	if rec.got.Header.Get("X-Domain") != "copilot.tencent.com" {
		t.Errorf("X-Domain=%q", rec.got.Header.Get("X-Domain"))
	}
}

// TestCodebuddyRefresh_NonZeroCode verifies a non-zero code response is an
// error (no token).
func TestCodebuddyRefresh_NonZeroCode(t *testing.T) {
	rec := &refreshRecorder{body: `{"code":1,"msg":"expired"}`}
	srv := newRefreshServer(t, rec)
	r := NewCodebuddyRefresher()
	pointClient(srv, r)
	out, err := r.Refresh(context.Background(), "rt", nil, resolver.NopLogger())
	if err == nil {
		t.Fatal("expected error for code!=0")
	}
	if out != nil {
		t.Fatalf("expected nil out, got %v", out)
	}
}

// TestXaiRefresh verifies the form body (client_id only, no secret) + id_token.
func TestXaiRefresh(t *testing.T) {
	rec := &refreshRecorder{body: `{"access_token":"at","refresh_token":"r2","id_token":"idt","expires_in":3600}`}
	srv := newRefreshServer(t, rec)
	r := NewXaiRefresher()
	pointClient(srv, r)
	out, err := r.Refresh(context.Background(), "rt", nil, resolver.NopLogger())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.IDToken != "idt" {
		t.Errorf("IDToken=%q want idt", out.IDToken)
	}
	form, _ := url.ParseQuery(rec.gotBody)
	if form.Get("client_id") != xaiClientID {
		t.Errorf("xai client_id=%q", form.Get("client_id"))
	}
	if _, ok := form["client_secret"]; ok {
		t.Errorf("xai must not send client_secret: %s", rec.gotBody)
	}
}

// TestGenericRefresh reads refreshUrl/clientId/clientSecret from psd.
func TestGenericRefresh(t *testing.T) {
	rec := &refreshRecorder{body: `{"access_token":"at","expires_in":3600}`}
	srv := newRefreshServer(t, rec)
	r := NewGenericRefresher("cline")
	pointClient(srv, r)
	psd := map[string]any{
		"refreshUrl":   srv.URL,
		"clientId":     "cid",
		"clientSecret": "csec",
	}
	out, err := r.Refresh(context.Background(), "rt", psd, resolver.NopLogger())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.AccessToken != "at" {
		t.Errorf("AccessToken=%q", out.AccessToken)
	}
	form, _ := url.ParseQuery(rec.gotBody)
	if form.Get("client_id") != "cid" || form.Get("client_secret") != "csec" {
		t.Errorf("generic form mismatch: %s", rec.gotBody)
	}
}

// TestGenericRefresh_NoRefreshURL verifies the error when psd lacks refreshUrl.
func TestGenericRefresh_NoRefreshURL(t *testing.T) {
	r := NewGenericRefresher("cline")
	_, err := r.Refresh(context.Background(), "rt", map[string]any{}, resolver.NopLogger())
	if err == nil || !strings.Contains(err.Error(), "no refresh URL") {
		t.Fatalf("expected no-refresh-url error, got %v", err)
	}
}

// TestVertexRefresh_NotPorted verifies the stub returns ErrVertexNotPorted.
func TestVertexRefresh_NotPorted(t *testing.T) {
	r := NewVertexRefresher()
	_, err := r.Refresh(context.Background(), "rt", nil, resolver.NopLogger())
	if err != ErrVertexNotPorted {
		t.Fatalf("expected ErrVertexNotPorted, got %v", err)
	}
}

// TestNon200IsError verifies a non-2xx upstream response is an error (caller
// falls back), not a silent success.
func TestNon200IsError(t *testing.T) {
	rec := &refreshRecorder{status: http.StatusInternalServerError, body: `server boom`}
	srv := newRefreshServer(t, rec)
	r := NewClaudeRefresher()
	pointClient(srv, r)
	out, err := r.Refresh(context.Background(), "rt", nil, resolver.NopLogger())
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if out != nil {
		t.Fatalf("expected nil out on 500, got %v", out)
	}
}

// TestLookup returns the expected refresher type per provider id.
func TestLookup(t *testing.T) {
	cases := map[string]string{
		"claude":         "*tokenrefresh.ClaudeRefresher",
		"gemini-cli":     "*tokenrefresh.GoogleRefresher",
		"antigravity":    "*tokenrefresh.GoogleRefresher",
		"qwen":           "*tokenrefresh.QwenRefresher",
		"codex":          "*tokenrefresh.CodexRefresher",
		"iflow":          "*tokenrefresh.IflowRefresher",
		"github":         "*tokenrefresh.GitHubRefresher",
		"copilot":        "*tokenrefresh.CopilotRefresher",
		"codebuddy-cn":   "*tokenrefresh.CodebuddyRefresher",
		"xai":            "*tokenrefresh.XaiRefresher",
		"grok-cli":       "*tokenrefresh.XaiRefresher",
		"gcli":           "*tokenrefresh.XaiRefresher",
		"vertex":         "*tokenrefresh.VertexRefresher",
		"vertex-partner": "*tokenrefresh.VertexRefresher",
	}
	for id, want := range cases {
		r := Lookup(id)
		if r == nil {
			t.Errorf("Lookup(%q)=nil", id)
			continue
		}
		got := typeName(r)
		if got != want {
			t.Errorf("Lookup(%q)=%s want %s", id, got, want)
		}
	}
	if Lookup("not-a-provider") != nil {
		t.Error("Lookup(unknown) should be nil")
	}
}

// TestClassifyOAuthRefreshError verifies the permanent-failure markers.
func TestClassifyOAuthRefreshError(t *testing.T) {
	cases := []struct {
		body      string
		status    int
		permanent bool
	}{
		{`{"error":{"code":"invalid_grant"}}`, 400, true},
		{`{"error":"refresh_token_expired"}`, 400, true},
		{`{"error":"refresh_token_reused"}`, 400, true},
		{`{"error_description":"refresh_token_invalidated"}`, 400, true},
		{`{"error":"invalid_request"}`, 400, false},
		{`{"error":"server_error"}`, 500, false},
		{``, 500, false},
	}
	for _, c := range cases {
		cls := classifyOAuthRefreshError(c.body, c.status)
		if cls.Permanent != c.permanent {
			t.Errorf("classify(%q) permanent=%v want %v", c.body, cls.Permanent, c.permanent)
		}
		if cls.Status != c.status {
			t.Errorf("classify(%q) status=%d want %d", c.body, cls.Status, c.status)
		}
	}
}

// Compile-time: all refreshers satisfy the interface.
var (
	_ resolver.TokenRefresher = (*ClaudeRefresher)(nil)
	_ resolver.TokenRefresher = (*GoogleRefresher)(nil)
	_ resolver.TokenRefresher = (*QwenRefresher)(nil)
	_ resolver.TokenRefresher = (*CodexRefresher)(nil)
	_ resolver.TokenRefresher = (*IflowRefresher)(nil)
	_ resolver.TokenRefresher = (*GitHubRefresher)(nil)
	_ resolver.TokenRefresher = (*CopilotRefresher)(nil)
	_ resolver.TokenRefresher = (*CodebuddyRefresher)(nil)
	_ resolver.TokenRefresher = (*XaiRefresher)(nil)
	_ resolver.TokenRefresher = (*GenericRefresher)(nil)
	_ resolver.TokenRefresher = (*VertexRefresher)(nil)
)