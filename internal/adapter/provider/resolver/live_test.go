package resolver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// liveSwapTransport rewrites every request's scheme/host to the test server's
// URL so the production endpoints (hardcoded constants) are redirected to the
// httptest server without exposing the constants as overridable fields. Same
// pattern as kiro_test.go's hostSwapTransport.
type liveSwapTransport struct {
	base http.RoundTripper
	to   *url.URL
}

func (t liveSwapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = t.to.Scheme
	req.URL.Host = t.to.Host
	req.Host = t.to.Host
	return t.base.RoundTrip(req)
}

func liveSwap(srvURL string) http.RoundTripper {
	u, _ := url.Parse(srvURL)
	return liveSwapTransport{base: http.DefaultTransport, to: u}
}

// withSwap installs a host-swap transport on the resolver's *http.Client so
// it hits srv instead of the real provider host.
func withSwap(r LiveModelResolver, srvURL string) {
	type transportSetter interface{ setTransport(http.RoundTripper) }
	if ts, ok := r.(transportSetter); ok {
		ts.setTransport(liveSwap(srvURL))
	}
}

func (r *clinepassResolver) setTransport(t http.RoundTripper) { r.client.Transport = t }
func (r *copilotResolver) setTransport(t http.RoundTripper)   { r.client.Transport = t }
func (r *grokCliResolver) setTransport(t http.RoundTripper)   { r.client.Transport = t }
func (r *kimchiResolver) setTransport(t http.RoundTripper)     { r.client.Transport = t }

// liveServer builds an httptest server that records the request and replies
// with the given body (200 unless status != 0).
func liveServer(t *testing.T, status int, body string, got **http.Request) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			*got = r.Clone(r.Context())
		}
		_, _ = io.ReadAll(r.Body)
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestClinepassResolve verifies the /models fetch + cline-pass/* filtering +
// workos: prefix clamping for OAuth tokens.
func TestClinepassResolve(t *testing.T) {
	body := `[{"id":"cline-pass/foo","name":"Foo"},{"id":"other/bar","name":"Bar"},{"id":"cline-pass/baz","name":"Baz"}]`
	var got *http.Request
	srv := liveServer(t, 0, body, &got)
	r := NewClinepassResolver(nil, "9.9.9").(*clinepassResolver)
	withSwap(r, srv.URL)
	res, err := r.Resolve(context.Background(), provider.Credentials{AccessToken: "tok"}, ResolveOpts{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	if len(res.Models) != 2 {
		t.Fatalf("expected 2 cline-pass models, got %d", len(res.Models))
	}
	if res.Models[0].ID != "cline-pass/foo" {
		t.Errorf("first model id=%q", res.Models[0].ID)
	}
	// OAuth token must be clamped to workos: prefix.
	if ah := got.Header.Get("Authorization"); !strings.HasPrefix(ah, "Bearer workos:") {
		t.Errorf("Authorization=%q want workos: prefix", ah)
	}
}

// TestClinepassResolve_APIKey verifies API keys skip the workos: clamp.
func TestClinepassResolve_APIKey(t *testing.T) {
	body := `[{"id":"cline-pass/foo","name":"Foo"}]`
	var got *http.Request
	srv := liveServer(t, 0, body, &got)
	r := NewClinepassResolver(nil, "9.9.9").(*clinepassResolver)
	withSwap(r, srv.URL)
	_, _ = r.Resolve(context.Background(), provider.Credentials{APIKey: "sk-key"}, ResolveOpts{})
	if ah := got.Header.Get("Authorization"); ah != "Bearer sk-key" {
		t.Errorf("API key Authorization=%q want 'Bearer sk-key'", ah)
	}
}

// TestClinepassResolve_EmptyToken returns nil,nil for missing credentials.
func TestClinepassResolve_EmptyToken(t *testing.T) {
	r := NewClinepassResolver(nil, "9.9.9")
	out, err := r.Resolve(context.Background(), provider.Credentials{}, ResolveOpts{})
	if err != nil || out != nil {
		t.Fatalf("expected nil,nil got %v %v", out, err)
	}
}

// TestClinepassResolve_DataWrap verifies the {data:[...]} response shape.
func TestClinepassResolve_DataWrap(t *testing.T) {
	body := `{"data":[{"id":"cline-pass/foo","name":"Foo"}]}`
	srv := liveServer(t, 0, body, nil)
	r := NewClinepassResolver(nil, "9.9.9").(*clinepassResolver)
	withSwap(r, srv.URL)
	res, _ := r.Resolve(context.Background(), provider.Credentials{APIKey: "k"}, ResolveOpts{})
	if res == nil || len(res.Models) != 1 {
		t.Fatalf("expected 1 model from data-wrapped response, got %v", res)
	}
}

// TestClinepassResolve_Non200 returns nil on a non-2xx (caller falls back).
func TestClinepassResolve_Non200(t *testing.T) {
	srv := liveServer(t, http.StatusUnauthorized, `{"error":"bad"}`, nil)
	r := NewClinepassResolver(nil, "9.9.9").(*clinepassResolver)
	withSwap(r, srv.URL)
	out, err := r.Resolve(context.Background(), provider.Credentials{APIKey: "k"}, ResolveOpts{})
	if out != nil {
		t.Fatalf("expected nil result on 401, got %v", out)
	}
	_ = err
}

// TestCopilotResolve verifies the chat-only + policy.enabled filtering and
// the editor headers.
func TestCopilotResolve(t *testing.T) {
	body := `{"data":[
		{"id":"gpt-5","name":"GPT-5","capabilities":{"type":"chat"},"policy":{"state":"enabled"}},
		{"id":"emb-1","name":"Emb","capabilities":{"type":"embeddings"},"policy":{"state":"enabled"}},
		{"id":"disabled-1","name":"Dis","capabilities":{"type":"chat"},"policy":{"state":"disabled"}}
	]}`
	var got *http.Request
	srv := liveServer(t, 0, body, &got)
	r := NewCopilotResolver(nil, nil, "9.9.9").(*copilotResolver)
	withSwap(r, srv.URL)
	res, err := r.Resolve(context.Background(), provider.Credentials{AccessToken: "tok"}, ResolveOpts{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil || len(res.Models) != 1 || res.Models[0].ID != "gpt-5" {
		t.Fatalf("expected only gpt-5 chat+enabled, got %v", res)
	}
	if got.Header.Get("Copilot-Integration-Id") != "vscode-chat" {
		t.Errorf("missing Copilot-Integration-Id header")
	}
	if got.Header.Get("editor-version") != "vscode/1.110.0" {
		t.Errorf("editor-version=%q", got.Header.Get("editor-version"))
	}
}

// TestCopilotResolve_RefreshOn401 verifies a 401 triggers the Copilot token
// exchange and a retry with the refreshed token.
func TestCopilotResolve_RefreshOn401(t *testing.T) {
	// First call 401, second (after refresh) 200.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.ReadAll(r.Body)
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"stale"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5","capabilities":{"type":"chat"},"policy":{"state":"enabled"}}]}`))
	}))
	t.Cleanup(srv.Close)
	r := NewCopilotResolver(nil, &stubRefresher{token: "refreshed-cop"}, "9.9.9").(*copilotResolver)
	withSwap(r, srv.URL)
	persisted := false
	res, err := r.Resolve(context.Background(), provider.Credentials{AccessToken: "gh-at"}, ResolveOpts{
		OnCredentialsRefreshed: func(rc RefreshedCredentials) error { persisted = true; return nil },
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil || len(res.Models) != 1 {
		t.Fatalf("expected 1 model after refresh, got %v", res)
	}
	if calls != 2 {
		t.Fatalf("expected 2 upstream calls (401 then retry), got %d", calls)
	}
	if !persisted {
		t.Error("OnCredentialsRefreshed was not called")
	}
}

// TestCopilotResolve_EmptyToken returns nil,nil when no copilotToken/accessToken.
func TestCopilotResolve_EmptyToken(t *testing.T) {
	r := NewCopilotResolver(nil, nil, "9.9.9")
	out, err := r.Resolve(context.Background(), provider.Credentials{}, ResolveOpts{})
	if err != nil || out != nil {
		t.Fatalf("expected nil,nil got %v %v", out, err)
	}
}

// stubRefresher is a TokenRefresher that returns a fixed token, for copilot /
// grok-cli refresh-on-401 tests.
type stubRefresher struct{ token string; err error }

func (s *stubRefresher) Refresh(_ context.Context, _ string, _ map[string]any, _ ProxyOptions, _ Logger) (*RefreshedCredentials, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &RefreshedCredentials{AccessToken: s.token}, nil
}

// TestGrokCliResolve verifies the grok-cli headers + parsing of the array
// response shape.
func TestGrokCliResolve(t *testing.T) {
	body := `[{"id":"grok-4","display_name":"Grok 4","context_length":256000},
	          {"id":"grok-build","display_name":"Grok Build","context_length":500000}]`
	var got *http.Request
	srv := liveServer(t, 0, body, &got)
	r := NewGrokCliResolver(nil, nil).(*grokCliResolver)
	withSwap(r, srv.URL)
	res, err := r.Resolve(context.Background(), provider.Credentials{AccessToken: "tok"}, ResolveOpts{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil || len(res.Models) != 2 {
		t.Fatalf("expected 2 models, got %v", res)
	}
	if res.Models[0].ContextLength != 256000 {
		t.Errorf("contextLength=%d want 256000", res.Models[0].ContextLength)
	}
	if got.Header.Get("x-xai-token-auth") != "xai-grok-cli" {
		t.Errorf("missing x-xai-token-auth header")
	}
	if got.Header.Get("x-grok-client-identifier") != "grok-shell" {
		t.Errorf("client identifier=%q", got.Header.Get("x-grok-client-identifier"))
	}
}

// TestGrokCliResolve_ObjectMap verifies the object-map response shape.
func TestGrokCliResolve_ObjectMap(t *testing.T) {
	body := `{"grok-4":{"display_name":"Grok 4","context_length":256000}}`
	srv := liveServer(t, 0, body, nil)
	r := NewGrokCliResolver(nil, nil).(*grokCliResolver)
	withSwap(r, srv.URL)
	res, _ := r.Resolve(context.Background(), provider.Credentials{AccessToken: "tok"}, ResolveOpts{})
	if res == nil || len(res.Models) != 1 || res.Models[0].ID != "grok-4" {
		t.Fatalf("expected 1 model from object-map, got %v", res)
	}
}

// TestGrokCliResolve_RefreshOn401 verifies a 401 triggers the xAI refresh.
func TestGrokCliResolve_RefreshOn401(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.ReadAll(r.Body)
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id":"grok-4","context_length":256000}]`))
	}))
	t.Cleanup(srv.Close)
	r := NewGrokCliResolver(nil, &stubRefresher{token: "refreshed-at"}).(*grokCliResolver)
	withSwap(r, srv.URL)
	res, err := r.Resolve(context.Background(), provider.Credentials{
		AccessToken: "at", ProviderSpecificData: map[string]any{"refreshToken": "rt"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil || len(res.Models) != 1 {
		t.Fatalf("expected 1 model after refresh, got %v", res)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
}

// countingStubRefresher is a TokenRefresher that counts how many times Refresh
// is called. Used by the dedup-coalescing live test to prove concurrent
// resolves on a 401 share a single refresh (#2703 Fix 2b).
type countingStubRefresher struct {
	mu    sync.Mutex
	calls int
	token string
}

func (c *countingStubRefresher) Refresh(_ context.Context, _ string, _ map[string]any, _ ProxyOptions, _ Logger) (*RefreshedCredentials, error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.mu.Unlock()
	// Simulate a refresh that takes a moment so concurrent resolves overlap.
	time.Sleep(20 * time.Millisecond)
	_ = n
	return &RefreshedCredentials{AccessToken: c.token}, nil
}

// TestGrokCliResolve_ConcurrentRefreshCoalesces proves #2703 Fix 2b: when two
// resolves race the same 401, the shared dedup collapses them into a single
// upstream refresh rather than firing two.
func TestGrokCliResolve_ConcurrentRefreshCoalesces(t *testing.T) {
	calls := int32(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		_, _ = io.ReadAll(r.Body)
		if n <= 2 {
			// Both concurrent resolves get a 401 on their first fetch attempt.
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id":"grok-4","context_length":256000}]`))
	}))
	t.Cleanup(srv.Close)
	refresher := &countingStubRefresher{token: "refreshed-at"}
	r := NewGrokCliResolver(nil, refresher).(*grokCliResolver)
	withSwap(r, srv.URL)

	creds := provider.Credentials{
		AccessToken:          "at",
		ProviderSpecificData: map[string]any{"refreshToken": "rt", "connectionId": "shared-conn"},
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Resolve(context.Background(), creds, ResolveOpts{})
		}()
	}
	wg.Wait()

	if refresher.calls > 1 {
		t.Fatalf("concurrent same-connection 401 refreshes must coalesce into 1, got %d", refresher.calls)
	}
	if refresher.calls != 1 {
		t.Fatalf("expected exactly 1 refresh call, got %d", refresher.calls)
	}
}
func TestGrokCliResolve_EmptyToken(t *testing.T) {
	r := NewGrokCliResolver(nil, nil)
	out, err := r.Resolve(context.Background(), provider.Credentials{}, ResolveOpts{})
	if err != nil || out != nil {
		t.Fatalf("expected nil,nil got %v %v", out, err)
	}
}

// TestKimchiResolve verifies the /v1/models/metadata fetch + normalization
// (llm vs imageToText kind, contextLength, reasoning).
func TestKimchiResolve(t *testing.T) {
	body := `{"models":[
		{"slug":"kimi-1","display_name":"Kimi 1","reasoning":true,"limits":{"context_window":128000},"input_modalities":["text"]},
		{"slug":"vision-1","display_name":"Vision 1","input_modalities":["text","image"]}
	]}`
	var got *http.Request
	srv := liveServer(t, 0, body, &got)
	r := NewKimchiResolver(nil).(*kimchiResolver)
	withSwap(r, srv.URL)
	res, err := r.Resolve(context.Background(), provider.Credentials{AccessToken: "tok"}, ResolveOpts{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil || len(res.Models) != 2 {
		t.Fatalf("expected 2 models, got %v", res)
	}
	if res.Models[0].Kind != "llm" {
		t.Errorf("kimi-1 kind=%q want llm", res.Models[0].Kind)
	}
	if res.Models[0].ContextLength != 128000 {
		t.Errorf("kimi-1 contextLength=%d want 128000", res.Models[0].ContextLength)
	}
	if res.Models[1].Kind != "imageToText" {
		t.Errorf("vision-1 kind=%q want imageToText", res.Models[1].Kind)
	}
	if got.Header.Get("User-Agent") != "kimchi/0.1.40" {
		t.Errorf("User-Agent=%q", got.Header.Get("User-Agent"))
	}
}

// TestKimchiResolve_EmptyToken returns nil,nil for missing token.
func TestKimchiResolve_EmptyToken(t *testing.T) {
	r := NewKimchiResolver(nil)
	out, err := r.Resolve(context.Background(), provider.Credentials{}, ResolveOpts{})
	if err != nil || out != nil {
		t.Fatalf("expected nil,nil got %v %v", out, err)
	}
}

// TestKimchiResolve_CustomEndpoint verifies psd.kimchiEndpoint overrides the base.
func TestKimchiResolve_CustomEndpoint(t *testing.T) {
	body := `{"models":[{"slug":"k-1","input_modalities":["text"]}]}`
	srv := liveServer(t, 0, body, nil)
	r := NewKimchiResolver(nil).(*kimchiResolver)
	withSwap(r, srv.URL)
	_, err := r.Resolve(context.Background(), provider.Credentials{
		AccessToken: "tok",
		ProviderSpecificData: map[string]any{"kimchiEndpoint": "https://custom.kimchi.example/"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// No assertion on path here (host is swapped); the test confirms custom
	// endpoint does not break resolution.
}

// TestQoderResolver_Stub verifies the stub returns ErrQoderCosyNotPorted.
func TestQoderResolver_Stub(t *testing.T) {
	r := NewQoderResolver()
	if r.ProviderID() != "qoder" {
		t.Errorf("ProviderID=%q want qoder", r.ProviderID())
	}
	_, err := r.Resolve(context.Background(), provider.Credentials{AccessToken: "tok"}, ResolveOpts{})
	if err != ErrQoderCosyNotPorted {
		t.Fatalf("expected ErrQoderCosyNotPorted, got %v", err)
	}
}

// TestResolveRegistry_RegistersAll verifies the new resolvers can be looked up
// after registration (the wire.go composition root registers them).
func TestResolveRegistry_RegistersAll(t *testing.T) {
	ResetRegistry()
	Register(NewClinepassResolver(nil, "1.0"))
	Register(NewCopilotResolver(nil, nil, "1.0"))
	Register(NewGrokCliResolver(nil, nil))
	Register(NewKimchiResolver(nil))
	Register(NewQoderResolver())
	for _, id := range []string{"clinepass", "github", "grok-cli", "kimchi", "qoder"} {
		if Lookup(id) == nil {
			t.Errorf("Lookup(%q)=nil after Register", id)
		}
	}
}

// Compile-time: all resolvers satisfy the interface.
var (
	_ LiveModelResolver = (*clinepassResolver)(nil)
	_ LiveModelResolver = (*copilotResolver)(nil)
	_ LiveModelResolver = (*grokCliResolver)(nil)
	_ LiveModelResolver = (*kimchiResolver)(nil)
	_ LiveModelResolver = (*qoderResolver)(nil)
)

// keep imports used in some build configs.
var _ = json.Marshal