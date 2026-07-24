package kiroexec

// profile_arn_test.go ports the profileArn-resolution half of decolua/9router
// abc0add0 (kiro #2297): api_key / idc / external_idp must never send the shared
// builder-id default profileArn (it belongs to another account → 403 "bearer
// token invalid") — only the connection-resolved ARN, or "" so CodeWhisperer
// uses the token's own default profile. OAuth/social keep the shared default.
// Verified against the real resolveKiroProfileArn / applyKiroProfileArn helpers
// (pure logic — no mock fetch) and an end-to-end Execute path that asserts the
// upstream body carries the resolved ARN, not the translator placeholder.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domain "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// profileArnCreds builds credentials for a given auth method + optional resolved
// profileArn.
func profileArnCreds(authMethod, profileArn string) domain.Credentials {
	psd := map[string]any{"authMethod": authMethod}
	if profileArn != "" {
		psd["profileArn"] = profileArn
	}
	return domain.Credentials{ProviderSpecificData: psd}
}

func TestResolveKiroProfileArn_AccountBoundSendsResolvedOrEmpty(t *testing.T) {
	// api_key / idc / external_idp: resolved ARN wins.
	for _, am := range []string{"api_key", "idc", "external_idp"} {
		resolved := "arn:aws:codewhisperer:us-east-1:111111111111:profile/MYPROFILE"
		if got := resolveKiroProfileArn(profileArnCreds(am, resolved)); got != resolved {
			t.Errorf("%q resolved ARN = %q, want %q", am, got, resolved)
		}
	}
	// account-bound with no resolved ARN → "" (NEVER the shared default).
	for _, am := range []string{"api_key", "idc", "external_idp"} {
		if got := resolveKiroProfileArn(profileArnCreds(am, "")); got != "" {
			t.Errorf("%q empty resolved ARN = %q, want empty (never the shared default)", am, got)
		}
	}
}

func TestResolveKiroProfileArn_OAuthSocialKeepsDefault(t *testing.T) {
	// builder-id OAuth with no resolved ARN → the builder-id shared default.
	if got := resolveKiroProfileArn(profileArnCreds("builder-id", "")); got != kiroDefaultProfileArnBuilderID {
		t.Errorf("builder-id default = %q, want %q", got, kiroDefaultProfileArnBuilderID)
	}
	// google / github social → the social shared default.
	for _, am := range []string{"google", "github"} {
		if got := resolveKiroProfileArn(profileArnCreds(am, "")); got != kiroDefaultProfileArnSocial {
			t.Errorf("%q social default = %q, want %q", am, got, kiroDefaultProfileArnSocial)
		}
	}
	// Unknown auth method falls back to the builder-id default (mirror
	// resolveDefaultProfileArn's non-social branch).
	if got := resolveKiroProfileArn(profileArnCreds("something-else", "")); got != kiroDefaultProfileArnBuilderID {
		t.Errorf("unknown default = %q, want builder-id default", got)
	}
	// OAuth/social with a resolved ARN still wins over the default.
	resolved := "arn:aws:codewhisperer:us-east-1:222222222222:profile/CUSTOM"
	if got := resolveKiroProfileArn(profileArnCreds("builder-id", resolved)); got != resolved {
		t.Errorf("builder-id resolved = %q, want %q", got, resolved)
	}
}

func TestResolveDefaultProfileArn(t *testing.T) {
	cases := []struct {
		authMethod string
		want       string
	}{
		{"google", kiroDefaultProfileArnSocial},
		{"github", kiroDefaultProfileArnSocial},
		{"builder-id", kiroDefaultProfileArnBuilderID},
		{"", kiroDefaultProfileArnBuilderID},
		{"anything-else", kiroDefaultProfileArnBuilderID},
	}
	for _, c := range cases {
		if got := resolveDefaultProfileArn(c.authMethod); got != c.want {
			t.Errorf("resolveDefaultProfileArn(%q) = %q, want %q", c.authMethod, got, c.want)
		}
	}
}

func TestAccountBoundAuthMethod(t *testing.T) {
	for _, am := range []string{"api_key", "idc", "external_idp"} {
		if !accountBoundAuthMethod(am) {
			t.Errorf("accountBoundAuthMethod(%q) = false, want true", am)
		}
	}
	for _, am := range []string{"builder-id", "google", "github", "", "oauth"} {
		if accountBoundAuthMethod(am) {
			t.Errorf("accountBoundAuthMethod(%q) = true, want false", am)
		}
	}
}

func TestApplyKiroProfileArn_OverwritesPlaceholder(t *testing.T) {
	// The translator stamps the placeholder builder-id ARN; an api_key
	// connection must rewrite it to the resolved ARN (never the default).
	placeholder := "arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX"
	body := []byte(`{"profileArn":"` + placeholder + `","conversationState":{"currentMessage":{"userInputMessage":{"content":"hi"}}}}`)
	resolved := "arn:aws:codewhisperer:us-east-1:999999999999:profile/REAL"
	out := applyKiroProfileArn(body, profileArnCreds("api_key", resolved))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if got, _ := m["profileArn"].(string); got != resolved {
		t.Errorf("profileArn = %q, want %q (resolved, not placeholder)", got, resolved)
	}
	// Other fields preserved.
	if _, ok := m["conversationState"]; !ok {
		t.Error("applyKiroProfileArn dropped conversationState")
	}
}

func TestApplyKiroProfileArn_AccountBoundEmptyClearsPlaceholder(t *testing.T) {
	// idc with no resolved ARN → profileArn becomes "" so CodeWhisperer uses the
	// token's own default profile, NOT the placeholder.
	body := []byte(`{"profileArn":"arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX"}`)
	out := applyKiroProfileArn(body, profileArnCreds("idc", ""))
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if got, _ := m["profileArn"].(string); got != "" {
		t.Errorf("idc empty profileArn = %q, want empty (token default)", got)
	}
}

func TestApplyKiroProfileArn_OAuthKeepsDefault(t *testing.T) {
	// builder-id OAuth with no resolved ARN keeps the shared default placeholder.
	body := []byte(`{"profileArn":"placeholder-stamped-by-translator"}`)
	out := applyKiroProfileArn(body, profileArnCreds("builder-id", ""))
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if got, _ := m["profileArn"].(string); got != kiroDefaultProfileArnBuilderID {
		t.Errorf("builder-id profileArn = %q, want the shared default", got)
	}
}

func TestApplyKiroProfileArn_MalformedBodyPassthrough(t *testing.T) {
	// A malformed body is returned unchanged so the upstream surfaces its own
	// error rather than the chat path crashing.
	bad := []byte(`{not json`)
	if got := applyKiroProfileArn(bad, profileArnCreds("api_key", "x")); string(got) != string(bad) {
		t.Errorf("malformed body changed: %q", got)
	}
	if got := applyKiroProfileArn(nil, profileArnCreds("api_key", "x")); got != nil {
		t.Errorf("nil body changed: %q", got)
	}
}

// captureProfileArnServer is an httptest upstream that records the request
// body and returns a non-2xx so Execute's non-2xx passthrough short-circuits the
// integrity gate (we only care that the rewritten body reached upstream).
func captureProfileArnServer(t *testing.T, gotBody *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"bad"}`)
	}))
}

// TestExecute_RewritesProfileArnForApiKey is the end-to-end abc0add0 port: an
// api_key connection's request body reaching the upstream must carry the
// connection-resolved profileArn, not the translator's placeholder builder-id
// ARN. Verified against a real httptest upstream via the same mock-fetch
// redirect as execute_test.go (newKiroExecutor — no mock executor).
func TestExecute_RewritesProfileArnForApiKey(t *testing.T) {
	var gotBody string
	srv := captureProfileArnServer(t, &gotBody)
	defer srv.Close()

	e := newKiroExecutor(t, srv)
	resolved := "arn:aws:codewhisperer:us-east-1:999999999999:profile/REAL"
	creds := profileArnCreds("api_key", resolved)
	creds.APIKey = "sk-test-APIKEY"
	body := []byte(`{"profileArn":"arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX","conversationState":{}}`)

	_, err := e.Execute(context.Background(), domain.ExecRequest{
		Model:       "claude-sonnet-4.5",
		Body:        body,
		Stream:      true,
		Credentials: creds,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v\n%s", err, gotBody)
	}
	if got, _ := sent["profileArn"].(string); got != resolved {
		t.Errorf("upstream profileArn = %q, want %q (resolved, not the placeholder)", got, resolved)
	}
	if strings.Contains(gotBody, "AAAACCCCXXXX") {
		t.Errorf("upstream body still carries the placeholder builder-id ARN: %s", gotBody)
	}
}

// TestExecute_RewritesProfileArnForIdc verifies the idc auth method (the
// abc0add0 regression) clears the placeholder ARN to "" — the token-bound
// default — instead of 403-ing on the shared builder-id default.
func TestExecute_RewritesProfileArnForIdc(t *testing.T) {
	var gotBody string
	srv := captureProfileArnServer(t, &gotBody)
	defer srv.Close()

	e := newKiroExecutor(t, srv)
	creds := profileArnCreds("idc", "")
	creds.AccessToken = "idc-access-token"
	body := []byte(`{"profileArn":"arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX","conversationState":{}}`)

	_, _ = e.Execute(context.Background(), domain.ExecRequest{
		Model:       "claude-sonnet-4.5",
		Body:        body,
		Stream:      true,
		Credentials: creds,
	})

	var sent map[string]any
	_ = json.Unmarshal([]byte(gotBody), &sent)
	if got, _ := sent["profileArn"].(string); got != "" {
		t.Errorf("idc upstream profileArn = %q, want empty (token-bound default, not the placeholder)", got)
	}
}
