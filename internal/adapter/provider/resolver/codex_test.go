package resolver

// codex_test.go covers the v0.5.40 codex live resolver (commit d587b2a4):
// client_version=0.144.6 gate, originator: codex_cli_rs header, refresh-on-401,
// the OpenAI-shaped {data:[...]} + bare-array parse shapes, and the synthetic
// <id>-review variant expansion. Drives a real httptest server via withSwap —
// no mock repo, the resolver's own HTTP client carries the request.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// TestCodexResolve_DataWrap verifies the {data:[...]} shape parses and the
// originator + client_version gate are present on the request.
func TestCodexResolve_DataWrap(t *testing.T) {
	body := `{"data":[{"id":"gpt-5.1","display_name":"GPT 5.1"},{"id":"gpt-5.1-codex","display_name":"Codex"}]}`
	var got *http.Request
	srv := liveServer(t, 0, body, &got)
	r := NewCodexResolver(nil, nil).(*codexResolver)
	withSwap(r, srv.URL)
	res, err := r.Resolve(context.Background(), provider.Credentials{AccessToken: "tok"}, ResolveOpts{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	// 2 chat models × 2 (each + a -review variant) = 4.
	if len(res.Models) != 4 {
		t.Fatalf("expected 4 models (2 chat + 2 review), got %d: %+v", len(res.Models), res.Models)
	}
	if got.Header.Get("originator") != "codex_cli_rs" {
		t.Errorf("originator=%q want codex_cli_rs", got.Header.Get("originator"))
	}
	if !strings.Contains(got.URL.RequestURI(), "client_version=0.144.6") {
		t.Errorf("request URL=%q missing client_version=0.144.6 gate", got.URL.RequestURI())
	}
	if ah := got.Header.Get("Authorization"); ah != "Bearer tok" {
		t.Errorf("Authorization=%q want Bearer tok", ah)
	}
}

// TestCodexResolve_BareArray verifies the bare-array response shape parses.
func TestCodexResolve_BareArray(t *testing.T) {
	body := `[{"id":"o4-mini","name":"O4 Mini"}]`
	srv := liveServer(t, 0, body, nil)
	r := NewCodexResolver(nil, nil).(*codexResolver)
	withSwap(r, srv.URL)
	res, _ := r.Resolve(context.Background(), provider.Credentials{AccessToken: "tok"}, ResolveOpts{})
	if res == nil || len(res.Models) != 2 {
		t.Fatalf("expected 2 (1 chat + 1 review), got %v", res)
	}
	if res.Models[0].ID != "o4-mini" {
		t.Errorf("first id=%q", res.Models[0].ID)
	}
	if res.Models[1].ID != "o4-mini-review" || res.Models[1].UpstreamModelID != "o4-mini" {
		t.Errorf("review variant=%+v want id=o4-mini-review upstream=o4-mini", res.Models[1])
	}
}

// TestCodexResolve_SkipsImageAndEmbed verifies image-typed and embed-named
// models are not expanded into review variants.
func TestCodexResolve_SkipsImageAndEmbed(t *testing.T) {
	body := `{"data":[
		{"id":"dall-e","type":"image","name":"DALL-E"},
		{"id":"text-embed-3","name":"Embed"},
		{"id":"gpt-5.6","name":"GPT 5.6"}
	]}`
	srv := liveServer(t, 0, body, nil)
	r := NewCodexResolver(nil, nil).(*codexResolver)
	withSwap(r, srv.URL)
	res, _ := r.Resolve(context.Background(), provider.Credentials{AccessToken: "tok"}, ResolveOpts{})
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	// gpt-5.6 → 2 (chat + review); dall-e (image) → 1, no review; embed → 1, no review.
	ids := map[string]bool{}
	for _, m := range res.Models {
		ids[m.ID] = true
	}
	if !ids["gpt-5.6"] || !ids["gpt-5.6-review"] {
		t.Errorf("expected gpt-5.6 + gpt-5.6-review; got %v", ids)
	}
	if !ids["dall-e"] {
		t.Errorf("image model dall-e should be kept (without review); got %v", ids)
	}
	if ids["dall-e-review"] {
		t.Errorf("image model must NOT get a review variant")
	}
	// embed models are non-chat (id includes "embed"): kept verbatim, no review.
	if !ids["text-embed-3"] {
		t.Errorf("embed model should be kept without a review variant; got %v", ids)
	}
	if ids["text-embed-3-review"] {
		t.Errorf("embed model must NOT get a review variant")
	}
}

// TestCodexResolve_RefreshOn401 verifies a 401 triggers the CodexRefresher and
// a retry with the refreshed token (refresh-aware model sync, the core of #90).
func TestCodexResolve_RefreshOn401(t *testing.T) {
	calls := 0
	var authHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		_, _ = io.ReadAll(r.Body)
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"stale"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.6","name":"GPT 5.6"}]}`))
	}))
	t.Cleanup(srv.Close)
	r := NewCodexResolver(nil, &stubRefresher{token: "refreshed-codex"}).(*codexResolver)
	withSwap(r, srv.URL)
	persisted := false
	res, err := r.Resolve(context.Background(), provider.Credentials{
		AccessToken: "stale-at",
		ProviderSpecificData: map[string]any{"refreshToken": "rt-xyz"},
	}, ResolveOpts{
		OnCredentialsRefreshed: func(rc RefreshedCredentials) error { persisted = true; return nil },
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil || len(res.Models) != 2 {
		t.Fatalf("expected 2 models after refresh, got %v", res)
	}
	if calls != 2 {
		t.Fatalf("expected 2 upstream calls (401 then retry), got %d", calls)
	}
	if len(authHeaders) != 2 || authHeaders[0] != "Bearer stale-at" || authHeaders[1] != "Bearer refreshed-codex" {
		t.Errorf("auth headers=%v want [Bearer stale-at, Bearer refreshed-codex]", authHeaders)
	}
	if !persisted {
		t.Error("OnCredentialsRefreshed was not called")
	}
}

// TestCodexResolve_EmptyToken returns nil,nil for missing credentials.
func TestCodexResolve_EmptyToken(t *testing.T) {
	r := NewCodexResolver(nil, nil)
	out, err := r.Resolve(context.Background(), provider.Credentials{}, ResolveOpts{})
	if err != nil || out != nil {
		t.Fatalf("expected nil,nil got %v %v", out, err)
	}
}

// TestCodexResolve_401NoRefreshToken returns nil,nil on 401 when there is no
// refreshToken to refresh with (no retry, no panic).
func TestCodexResolve_401NoRefreshToken(t *testing.T) {
	srv := liveServer(t, http.StatusUnauthorized, `{"error":"stale"}`, nil)
	r := NewCodexResolver(nil, &stubRefresher{token: "ignored"}).(*codexResolver)
	withSwap(r, srv.URL)
	out, err := r.Resolve(context.Background(), provider.Credentials{AccessToken: "stale-at"}, ResolveOpts{})
	if err != nil || out != nil {
		t.Fatalf("expected nil,nil on 401 without refreshToken, got %v %v", out, err)
	}
}