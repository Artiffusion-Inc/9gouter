package resolver

// cursor_test.go pins the cursor live resolver — no mocks: real httptest
// server (HTTP/1.1 is enough; the resolver only needs the path + headers +
// body to round-trip), real protobuf body built with cursorexec helpers.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cursorexec "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/cursor"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// cursorModelsBody builds an agent.v1.GetUsableModelsResponse protobuf with
// the given model entries via cursorexec.BuildGetUsableModelsResponse.
func cursorModelsBody(t *testing.T, models ...cursorexec.CursorModel) []byte {
	t.Helper()
	return cursorexec.BuildGetUsableModelsResponse(models)
}

// TestCursorResolve parses a GetUsableModelsResponse and checks headers.
func TestCursorResolve(t *testing.T) {
	body := cursorModelsBody(t,
		cursorexec.CursorModel{ID: "gpt-5.3-codex", Name: "GPT 5.3 Codex"},
		cursorexec.CursorModel{ID: "cursor-small", Name: "Cursor Small"},
	)
	var got *http.Request
	srv := liveServer(t, 0, string(body), &got)
	r := NewCursorResolver(nil).(*cursorResolver)
	withSwap(r, srv.URL)
	res, err := r.Resolve(context.Background(), provider.Credentials{
		AccessToken:          "raw-token",
		ProviderSpecificData: map[string]any{"machineId": "machine-1"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	if len(res.Models) != 2 {
		t.Fatalf("expected 2 models, got %d: %+v", len(res.Models), res.Models)
	}
	if res.Models[0].ID != "gpt-5.3-codex" || res.Models[0].Name != "GPT 5.3 Codex" {
		t.Errorf("m0 = %+v", res.Models[0])
	}
	if res.Models[0].Kind != "chat" {
		t.Errorf("kind=%q want chat", res.Models[0].Kind)
	}
	// Verify the gateway headers were sent.
	if got == nil {
		t.Fatal("request not captured")
	}
	if ah := got.Header.Get("Authorization"); ah != "Bearer raw-token" {
		t.Errorf("Authorization=%q want Bearer raw-token", ah)
	}
	if got.Header.Get("x-cursor-client-version") != "3.12.17" {
		t.Errorf("client-version=%q want 3.12.17", got.Header.Get("x-cursor-client-version"))
	}
	if got.Header.Get("x-cursor-client-commit") == "" {
		t.Error("x-cursor-client-commit missing")
	}
	if got.Header.Get("content-type") != "application/proto" {
		t.Errorf("content-type=%q want application/proto", got.Header.Get("content-type"))
	}
	if got.Header.Get("accept") != "application/proto" {
		t.Errorf("accept=%q want application/proto", got.Header.Get("accept"))
	}
	if got.Header.Get("connect-protocol-version") != "" {
		t.Error("connect-protocol-version should be stripped for unary calls")
	}
	if got.Header.Get("x-cursor-checksum") == "" {
		t.Error("x-cursor-checksum missing")
	}
}

// TestCursorResolve_CacheHit serves a body once, then asserts a second
// Resolve does not hit the server (cache hit).
func TestCursorResolve_CacheHit(t *testing.T) {
	body := cursorModelsBody(t, cursorexec.CursorModel{ID: "m1", Name: "M1"})
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	r := NewCursorResolver(nil).(*cursorResolver)
	withSwap(r, srv.URL)
	creds := provider.Credentials{
		AccessToken:          "tok",
		ProviderSpecificData: map[string]any{"machineId": "m"},
	}
	if _, err := r.Resolve(context.Background(), creds, ResolveOpts{}); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if _, err := r.Resolve(context.Background(), creds, ResolveOpts{}); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 upstream call (cache hit on 2nd), got %d", calls)
	}
}

// TestCursorResolve_EmptyToken returns nil,nil for missing accessToken.
func TestCursorResolve_EmptyToken(t *testing.T) {
	r := NewCursorResolver(nil)
	out, err := r.Resolve(context.Background(), provider.Credentials{
		ProviderSpecificData: map[string]any{"machineId": "m"},
	}, ResolveOpts{})
	if err != nil || out != nil {
		t.Fatalf("expected nil,nil got %v %v", out, err)
	}
}

// TestCursorResolve_NoMachineId returns nil,nil for missing machineId.
func TestCursorResolve_NoMachineId(t *testing.T) {
	r := NewCursorResolver(nil)
	out, err := r.Resolve(context.Background(), provider.Credentials{
		AccessToken: "tok",
	}, ResolveOpts{})
	if err != nil || out != nil {
		t.Fatalf("expected nil,nil got %v %v", out, err)
	}
}

// TestCursorResolve_Non200 returns nil on a non-2xx (caller falls back to static
// catalog, never errors — mirrors JS cursorModels.js returning null).
func TestCursorResolve_Non200(t *testing.T) {
	srv := liveServer(t, http.StatusUnauthorized, `{"error":"bad"}`, nil)
	r := NewCursorResolver(nil).(*cursorResolver)
	withSwap(r, srv.URL)
	out, err := r.Resolve(context.Background(), provider.Credentials{
		AccessToken:          "tok",
		ProviderSpecificData: map[string]any{"machineId": "m"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("expected nil error on 401 (fallback), got %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil result on 401, got %v", out)
	}
}

// TestCursorResolve_EmptyBody returns nil when the response decodes to zero
// models (no crash).
func TestCursorResolve_EmptyBody(t *testing.T) {
	srv := liveServer(t, 0, "", nil)
	r := NewCursorResolver(nil).(*cursorResolver)
	withSwap(r, srv.URL)
	out, err := r.Resolve(context.Background(), provider.Credentials{
		AccessToken:          "tok",
		ProviderSpecificData: map[string]any{"machineId": "m"},
	}, ResolveOpts{})
	if err != nil || out != nil {
		t.Fatalf("expected nil,nil on empty body, got %v %v", out, err)
	}
}

// TestCursorResolve_GhostModeString verifies psd.ghostMode as the "false" string
// disables ghost mode (the default-true path is exercised by TestCursorResolve).
func TestCursorResolve_GhostModeString(t *testing.T) {
	body := cursorModelsBody(t, cursorexec.CursorModel{ID: "m1", Name: "M1"})
	var got *http.Request
	srv := liveServer(t, 0, string(body), &got)
	r := NewCursorResolver(nil).(*cursorResolver)
	withSwap(r, srv.URL)
	_, _ = r.Resolve(context.Background(), provider.Credentials{
		AccessToken:          "tok",
		ProviderSpecificData: map[string]any{"machineId": "m", "ghostMode": "false"},
	}, ResolveOpts{})
	if got == nil {
		t.Fatal("request not captured")
	}
	if got.Header.Get("x-ghost-mode") != "false" {
		t.Errorf("ghost-mode=%q want false", got.Header.Get("x-ghost-mode"))
	}
}

// TestCursorResolve_TokenPrefixSplit verifies a "::"-prefixed token is cleaned
// before going into the Bearer header (provider-prefixed storage shape).
func TestCursorResolve_TokenPrefixSplit(t *testing.T) {
	body := cursorModelsBody(t, cursorexec.CursorModel{ID: "m1", Name: "M1"})
	var got *http.Request
	srv := liveServer(t, 0, string(body), &got)
	r := NewCursorResolver(nil).(*cursorResolver)
	withSwap(r, srv.URL)
	_, _ = r.Resolve(context.Background(), provider.Credentials{
		AccessToken:          "cursor::actual-secret",
		ProviderSpecificData: map[string]any{"machineId": "m"},
	}, ResolveOpts{})
	if got == nil {
		t.Fatal("request not captured")
	}
	if ah := got.Header.Get("Authorization"); ah != "Bearer actual-secret" {
		t.Errorf("Authorization=%q want Bearer actual-secret (prefix stripped)", ah)
	}
}

// TestCursorCacheKey_Stable verifies the cache key is stable for the same
// credentials and differs across credentials.
func TestCursorCacheKey_Stable(t *testing.T) {
	c1 := provider.Credentials{AccessToken: "tok", ProviderSpecificData: map[string]any{"machineId": "m"}}
	c2 := provider.Credentials{AccessToken: "tok", ProviderSpecificData: map[string]any{"machineId": "m"}}
	c3 := provider.Credentials{AccessToken: "other", ProviderSpecificData: map[string]any{"machineId": "m"}}
	if a, b := cursorCacheKey(c1), cursorCacheKey(c2); a != b {
		t.Errorf("same creds → different keys %q vs %q", a, b)
	}
	if a, b := cursorCacheKey(c1), cursorCacheKey(c3); a == b {
		t.Errorf("different creds → same key %q", a)
	}
	if strings.HasPrefix(cursorCacheKey(c1), "cursor:") {
		t.Error("cache key should be a hash, not the raw seed")
	}
}

// Compile-time: cursorResolver satisfies the interface.
var _ LiveModelResolver = (*cursorResolver)(nil)
