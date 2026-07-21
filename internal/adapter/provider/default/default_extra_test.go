package defaultexec

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// headerExact reads the raw map slot of a header key, bypassing net/http
// canonicalization (setHeaderExact stores keys with exact casing).
func headerExact(h http.Header, key string) string {
	v, ok := h[key] //nolint:staticcheck // SA1008: intentional raw-map access reads exact-casing slot
	if !ok || len(v) == 0 {
		return ""
	}
	return v[0]
}

// creds builds a Credentials value with the given provider-specific extras.
func creds(apiKey, accessToken string, extra map[string]any) provider.Credentials {
	return provider.Credentials{
		APIKey:               apiKey,
		AccessToken:          accessToken,
		ProviderSpecificData: extra,
	}
}

// --- HeaderHook ---

func TestHeaderHookKnown(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	for _, name := range []string{"clineHeaders", "kimiHeaders", "claudeOverlay", "kilocodeOrg"} {
		if fn := e.HeaderHook(name); fn == nil {
			t.Errorf("HeaderHook(%q) = nil, want non-nil", name)
		}
	}
}

func TestHeaderHookUnknown(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	if fn := e.HeaderHook("nope"); fn != nil {
		t.Errorf("HeaderHook(unknown) = non-nil, want nil")
	}
}

// --- clineHeaders ---

func TestClineHeadersAPIKey(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	h := http.Header{}
	e.clineHeaders(h, creds("sk-abc", "", nil))
	if headerExact(h, "Authorization") != "Bearer workos:sk-abc" {
		t.Errorf("Authorization = %q, want Bearer workos:sk-abc", headerExact(h, "Authorization"))
	}
	if headerExact(h, "HTTP-Referer") != "https://cline.bot" {
		t.Errorf("HTTP-Referer = %q", headerExact(h, "HTTP-Referer"))
	}
	if headerExact(h, "X-Title") != "Cline" {
		t.Errorf("X-Title = %q", headerExact(h, "X-Title"))
	}
	if !strings.HasPrefix(headerExact(h, "User-Agent"), "9Gouter/") {
		t.Errorf("User-Agent = %q", headerExact(h, "User-Agent"))
	}
}

func TestClineHeadersStripsWorkosPrefix(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	h := http.Header{}
	e.clineHeaders(h, creds("workos:tok-123", "", nil))
	if headerExact(h, "Authorization") != "Bearer workos:tok-123" {
		t.Errorf("Authorization = %q, want Bearer workos:tok-123 (no double prefix)", headerExact(h, "Authorization"))
	}
}

func TestClineHeadersAccessTokenFallback(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	h := http.Header{}
	e.clineHeaders(h, creds("", "at-xyz", nil))
	if headerExact(h, "Authorization") != "Bearer workos:at-xyz" {
		t.Errorf("Authorization = %q, want Bearer workos:at-xyz", headerExact(h, "Authorization"))
	}
}

func TestClineHeadersNoTokenSkipsAuth(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	h := http.Header{}
	e.clineHeaders(h, creds("", "", nil))
	if _, ok := h["Authorization"]; ok {
		t.Errorf("Authorization should be absent when no token")
	}
}

// --- kimiHeaders ---

func TestKimiHeaders(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	h := http.Header{}
	e.kimiHeaders(h, provider.Credentials{})
	if headerExact(h, "X-Msh-Platform") != "9gouter" {
		t.Errorf("X-Msh-Platform = %q", headerExact(h, "X-Msh-Platform"))
	}
	if headerExact(h, "X-Msh-Version") != "2.1.2" {
		t.Errorf("X-Msh-Version = %q", headerExact(h, "X-Msh-Version"))
	}
	if id := headerExact(h, "X-Msh-Device-Id"); !strings.HasPrefix(id, "kimi-") {
		t.Errorf("X-Msh-Device-Id = %q, want kimi- prefix", id)
	}
}

// --- kilocodeOrg ---

func TestKilocodeOrgWithOrgID(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	h := http.Header{}
	e.kilocodeOrg(h, creds("", "", map[string]any{"orgId": "org-7"}))
	if headerExact(h, "X-Kilocode-OrganizationID") != "org-7" {
		t.Errorf("X-Kilocode-OrganizationID = %q, want org-7", headerExact(h, "X-Kilocode-OrganizationID"))
	}
}

func TestKilocodeOrgWithoutOrgID(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	h := http.Header{}
	e.kilocodeOrg(h, creds("", "", nil))
	if _, ok := h["X-Kilocode-OrganizationID"]; ok { //nolint:staticcheck // SA1008: intentional raw-map access (kilocode sets exact case)
		t.Errorf("org id header should be absent without orgId")
	}
}

// --- claudeOverlay ---

func TestClaudeOverlayNoOp(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	h := http.Header{}
	e.claudeOverlay(h, provider.Credentials{})
	// No static headers are set by claudeOverlay; cache is populated from
	// incoming client headers, which the test path has none of.
	if len(h) != 0 {
		t.Errorf("claudeOverlay should set no static headers, got %v", h)
	}
}

// --- BuildURL ---

func TestBuildURLRuntimeTransportWithSuffix(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	got := e.BuildURL("m", false, 0, creds("", "", map[string]any{
		"runtimeTransport": map[string]any{"baseUrl": "https://rt.example/v1", "urlSuffix": "/foo"},
	}))
	if got != "https://rt.example/v1/foo" {
		t.Errorf("got %q", got)
	}
}

func TestBuildURLRuntimeTransportWithoutSuffix(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	got := e.BuildURL("m", false, 0, creds("", "", map[string]any{
		"runtimeTransport": map[string]any{"baseUrl": "https://rt.example/v1"},
	}))
	if got != "https://rt.example/v1" {
		t.Errorf("got %q", got)
	}
}

func TestBuildURLOpenAICompatibleChat(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai-compatible-foo", base.Config{})}
	got := e.BuildURL("m", false, 0, creds("", "", map[string]any{"baseUrl": "https://gw.example/v1/"}))
	if got != "https://gw.example/v1/chat/completions" {
		t.Errorf("got %q, want https://gw.example/v1/chat/completions", got)
	}
}

func TestBuildURLOpenAICompatibleResponsesPath(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai-compatible-responses", base.Config{})}
	got := e.BuildURL("m", false, 0, creds("", "", nil))
	if !strings.HasSuffix(got, "/responses") {
		t.Errorf("got %q, want /responses suffix", got)
	}
}

func TestBuildURLOpenAICompatibleDefaultBase(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai-compatible-foo", base.Config{})}
	got := e.BuildURL("m", false, 0, creds("", "", nil))
	if got != base.OpenAICompatBase+"/chat/completions" {
		t.Errorf("got %q, want default openai base", got)
	}
}

func TestBuildURLAnthropicCompatible(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("anthropic-compatible-foo", base.Config{})}
	got := e.BuildURL("m", false, 0, creds("", "", map[string]any{"baseUrl": "https://gw.example/v1/"}))
	if got != "https://gw.example/v1/messages" {
		t.Errorf("got %q, want https://gw.example/v1/messages", got)
	}
}

func TestBuildURLAnthropicCompatibleDefaultBase(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("anthropic-compatible-foo", base.Config{})}
	got := e.BuildURL("m", false, 0, creds("", "", nil))
	if got != base.AnthropicCompatBase+"/messages" {
		t.Errorf("got %q", got)
	}
}

func TestBuildURLGeminiNonStream(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("gemini", base.Config{BaseURL: "https://gemini.example/v1beta", Format: "gemini"})}
	got := e.BuildURL("gemini-1.5", false, 0, provider.Credentials{})
	if got != "https://gemini.example/v1beta/gemini-1.5:generateContent" {
		t.Errorf("got %q", got)
	}
}

func TestBuildURLGeminiStream(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("gemini", base.Config{BaseURL: "https://gemini.example/v1beta", Format: "gemini"})}
	got := e.BuildURL("gemini-1.5", true, 0, provider.Credentials{})
	if got != "https://gemini.example/v1beta/gemini-1.5:streamGenerateContent?alt=sse" {
		t.Errorf("got %q", got)
	}
}

func TestBuildURLURLSuffix(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{BaseURL: "https://x.example", URLSuffix: "/chat"})}
	got := e.BuildURL("m", false, 0, provider.Credentials{})
	if got != "https://x.example/chat" {
		t.Errorf("got %q", got)
	}
}

func TestBuildURLBaseURLsIndex(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{BaseURLs: []string{"https://a.example", "https://b.example"}})}
	if got := e.BuildURL("m", false, 1, provider.Credentials{}); got != "https://b.example" {
		t.Errorf("got %q, want https://b.example", got)
	}
}

func TestBuildURLBaseURLsOutOfBoundsFallsBack(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{BaseURLs: []string{"https://a.example", "https://b.example"}})}
	if got := e.BuildURL("m", false, 5, provider.Credentials{}); got != "https://a.example" {
		t.Errorf("got %q, want https://a.example (fallback to [0])", got)
	}
}

func TestBuildURLAccountIDPlaceholder(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{BaseURL: "https://x.example/{accountId}/v1"})}
	got := e.BuildURL("m", false, 0, creds("", "", map[string]any{"accountId": "acct-9"}))
	if got != "https://x.example/acct-9/v1" {
		t.Errorf("got %q, want accountId substituted", got)
	}
}

func TestBuildURLAccountIDMissingPanics(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{BaseURL: "https://x.example/{accountId}/v1"})}
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic for missing accountId")
		}
	}()
	_ = e.BuildURL("m", false, 0, creds("", "", nil))
}

// --- resolveRuntimeTransport / mapAuthDescriptor ---

func TestResolveRuntimeTransportFull(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	rt := e.resolveRuntimeTransport(creds("", "", map[string]any{
		"runtimeTransport": map[string]any{
			"baseUrl":   "https://rt.example",
			"urlSuffix": "/s",
			"headers":   map[string]any{"x-foo": "bar"},
			"auth": map[string]any{
				"combined":         true,
				"header":            "Authorization",
				"scheme":            "bearer",
				"anthropicVersion":  true,
			},
		},
	}))
	if rt == nil {
		t.Fatal("rt nil")
	}
	if rt.BaseURL != "https://rt.example" {
		t.Errorf("BaseURL = %q", rt.BaseURL)
	}
	if rt.URLSuffix != "/s" {
		t.Errorf("URLSuffix = %q", rt.URLSuffix)
	}
	if rt.Headers["x-foo"] != "bar" {
		t.Errorf("headers = %v", rt.Headers)
	}
	if !rt.Auth.Combined {
		t.Errorf("auth not combined")
	}
	if !rt.Auth.AnthropicVersion {
		t.Errorf("anthropicVersion not set")
	}
	if rt.Auth.Scheme != "bearer" {
		t.Errorf("scheme = %q", rt.Auth.Scheme)
	}
}

func TestResolveRuntimeTransportAbsent(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	if rt := e.resolveRuntimeTransport(provider.Credentials{}); rt != nil {
		t.Errorf("expected nil rt, got %v", rt)
	}
}

func TestResolveRuntimeTransportWrongType(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	if rt := e.resolveRuntimeTransport(creds("", "", map[string]any{"runtimeTransport": "not-a-map"})); rt != nil {
		t.Errorf("expected nil for non-map runtimeTransport, got %v", rt)
	}
}

// --- BuildHeaders ---

func TestBuildHeadersCombinedBearer(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai", base.Config{})}
	h := e.BuildHeaders(creds("sk-key", "", nil), false)
	if headerExact(h, "Authorization") != "Bearer sk-key" {
		t.Errorf("Authorization = %q, want Bearer sk-key", headerExact(h, "Authorization"))
	}
	if headerExact(h, "Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", headerExact(h, "Content-Type"))
	}
}

func TestBuildHeadersCombinedBearerUndefined(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai", base.Config{})}
	h := e.BuildHeaders(provider.Credentials{}, false)
	if headerExact(h, "Authorization") != "Bearer undefined" {
		t.Errorf("Authorization = %q, want Bearer undefined for empty token", headerExact(h, "Authorization"))
	}
}

func TestBuildHeadersAccessTokenFallback(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai", base.Config{})}
	h := e.BuildHeaders(creds("", "at-token", nil), false)
	if headerExact(h, "Authorization") != "Bearer at-token" {
		t.Errorf("Authorization = %q, want Bearer at-token", headerExact(h, "Authorization"))
	}
}

func TestBuildHeadersStreamAccept(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai", base.Config{})}
	h := e.BuildHeaders(creds("sk-key", "", nil), true)
	if headerExact(h, "Accept") != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", headerExact(h, "Accept"))
	}
}

func TestBuildHeadersNonStreamNoAccept(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai", base.Config{})}
	h := e.BuildHeaders(creds("sk-key", "", nil), false)
	if headerExact(h, "Accept") != "" {
		t.Errorf("Accept = %q, want empty for non-stream", headerExact(h, "Accept"))
	}
}

func TestBuildHeadersConfigHeadersApplied(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai", base.Config{
		Headers: map[string]string{"x-custom": "val"},
	})}
	h := e.BuildHeaders(creds("sk-key", "", nil), false)
	if headerExact(h, "x-custom") != "val" {
		t.Errorf("x-custom = %q, want val", headerExact(h, "x-custom"))
	}
}

func TestBuildHeadersClaudeFormat(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("anthropic", base.Config{Format: "claude"})}
	h := e.BuildHeaders(creds("sk-anth", "", nil), false)
	if headerExact(h, "x-api-key") != "sk-anth" {
		t.Errorf("x-api-key = %q, want sk-anth", headerExact(h, "x-api-key"))
	}
	if headerExact(h, "anthropic-version") != base.AnthropicAPIVersion {
		t.Errorf("anthropic-version = %q, want %s", headerExact(h, "anthropic-version"), base.AnthropicAPIVersion)
	}
}

func TestBuildHeadersAnthropicCompatibleAPIKey(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("anthropic-compatible-x", base.Config{})}
	// No baseUrl in creds → official → x-api-key set by descriptor, not Authorization.
	h := e.BuildHeaders(creds("sk-anth", "", nil), false)
	if headerExact(h, "x-api-key") != "sk-anth" {
		t.Errorf("x-api-key = %q, want sk-anth", headerExact(h, "x-api-key"))
	}
	if headerExact(h, "anthropic-version") != base.AnthropicAPIVersion {
		t.Errorf("anthropic-version = %q", headerExact(h, "anthropic-version"))
	}
}

func TestBuildHeadersAnthropicCompatibleGatewayCleanup(t *testing.T) {
	// Third-party anthropic-compatible gateway: Authorization added, browser-access
	// and x-app stripped, claude-code beta tag filtered out.
	// anthropic-beta must be stored under the canonical key so the cleanup
	// loop (which reads via h.Get, canonicalized) can see and filter it.
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("anthropic-compatible-x", base.Config{
		Headers: map[string]string{
			"Anthropic-Beta":                          "claude-code-20250219,fine-grained-tool-streaming-2025-05-14",
			"anthropic-dangerous-direct-browser-access": "true",
			"x-app":                                 "value",
		},
	})}
	h := e.BuildHeaders(creds("sk-key", "", map[string]any{"baseUrl": "https://gateway.example"}), false)
	if headerExact(h, "Authorization") != "Bearer sk-key" {
		t.Errorf("Authorization = %q, want Bearer sk-key (gateway)", headerExact(h, "Authorization"))
	}
	if beta := h.Get("anthropic-beta"); beta != "fine-grained-tool-streaming-2025-05-14" {
		t.Errorf("anthropic-beta = %q, want claude-code tag filtered out", beta)
	}
	if headerExact(h, "anthropic-dangerous-direct-browser-access") != "" {
		t.Errorf("browser-access header should be stripped")
	}
	if headerExact(h, "x-app") != "" {
		t.Errorf("x-app should be stripped")
	}
}

func TestBuildHeadersAnthropicCompatibleOfficialKeepsBeta(t *testing.T) {
	// Official api.anthropic.com: no Authorization injection, beta tags kept.
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("anthropic-compatible-x", base.Config{
		Headers: map[string]string{"Anthropic-Beta": "claude-code-20250219,other-tag"},
	})}
	h := e.BuildHeaders(creds("sk-key", "", map[string]any{"baseUrl": "https://api.anthropic.com"}), false)
	if headerExact(h, "Authorization") == "Bearer sk-key" {
		t.Errorf("Authorization should NOT be injected on official anthropic")
	}
	if beta := h.Get("anthropic-beta"); beta != "claude-code-20250219,other-tag" {
		t.Errorf("anthropic-beta = %q, want unchanged (official keeps claude-code)", beta)
	}
}

func TestBuildHeadersAnthropicCompatibleBetaAllFiltered(t *testing.T) {
	// When only the claude-code beta tag is present, the gateway path removes
	// the whole header (empty filtered list).
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("anthropic-compatible-x", base.Config{
		Headers: map[string]string{"Anthropic-Beta": "claude-code-20250219"},
	})}
	h := e.BuildHeaders(creds("sk-key", "", map[string]any{"baseUrl": "https://gateway.example"}), false)
	if v := h.Get("anthropic-beta"); v != "" {
		t.Errorf("anthropic-beta should be removed when all tags filtered, got %q", v)
	}
}

func TestBuildHeadersHooksApplied(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai", base.Config{
		Auth: base.AuthDescriptor{
			Combined: true,
			Header:   "Authorization",
			Scheme:   "bearer",
			Hooks:    []string{"clineHeaders"},
		},
	})}
	h := e.BuildHeaders(creds("sk-abc", "", nil), false)
	if headerExact(h, "HTTP-Referer") != "https://cline.bot" {
		t.Errorf("cline hook not applied: HTTP-Referer = %q", headerExact(h, "HTTP-Referer"))
	}
}

// --- applyAuth (direct) ---

func TestApplyAuthCombinedRawScheme(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	h := http.Header{}
	e.applyAuth(h, base.AuthDescriptor{Combined: true, Header: "x-api-key", Scheme: "raw"}, creds("sk-raw", "", nil))
	if headerExact(h, "x-api-key") != "sk-raw" {
		t.Errorf("x-api-key = %q, want sk-raw (raw scheme)", headerExact(h, "x-api-key"))
	}
}

func TestApplyAuthCombinedDefaultHeader(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	h := http.Header{}
	e.applyAuth(h, base.AuthDescriptor{Combined: true}, creds("sk-key", "", nil))
	if headerExact(h, "Authorization") != "Bearer sk-key" {
		t.Errorf("Authorization = %q, want Bearer sk-key (default bearer)", headerExact(h, "Authorization"))
	}
}

func TestApplyAuthSplitAPIKeyBearer(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	h := http.Header{}
	e.applyAuth(h, base.AuthDescriptor{
		APIKey: &base.AuthSpec{Header: "X-Custom", Scheme: "bearer"},
	}, creds("sk-key", "ignored-token", nil))
	if headerExact(h, "X-Custom") != "Bearer sk-key" {
		t.Errorf("X-Custom = %q, want Bearer sk-key", headerExact(h, "X-Custom"))
	}
}

func TestApplyAuthSplitAPIKeyRaw(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	h := http.Header{}
	e.applyAuth(h, base.AuthDescriptor{
		APIKey: &base.AuthSpec{Header: "x-key", Scheme: "raw"},
	}, creds("sk-key", "", nil))
	if headerExact(h, "x-key") != "sk-key" {
		t.Errorf("x-key = %q, want sk-key (raw)", headerExact(h, "x-key"))
	}
}

func TestApplyAuthSplitOAuth(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	h := http.Header{}
	e.applyAuth(h, base.AuthDescriptor{
		OAuth: &base.AuthSpec{Header: "X-OAuth", Scheme: "bearer"},
	}, creds("", "at-token", nil))
	if headerExact(h, "X-OAuth") != "Bearer at-token" {
		t.Errorf("X-OAuth = %q, want Bearer at-token", headerExact(h, "X-OAuth"))
	}
}

func TestApplyAuthNoTokenNoSplitBranch(t *testing.T) {
	// No APIKey + no AccessToken → no split header set.
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	h := http.Header{}
	e.applyAuth(h, base.AuthDescriptor{
		APIKey: &base.AuthSpec{Header: "x-key", Scheme: "raw"},
	}, provider.Credentials{})
	if headerExact(h, "x-key") != "" {
		t.Errorf("x-key = %q, want empty (no token)", headerExact(h, "x-key"))
	}
}

// --- TransformRequest ---

func TestTransformRequestEmptyBody(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai", base.Config{})}
	out, err := e.TransformRequest("m", nil, false, provider.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Errorf("expected empty map, got %v", m)
	}
}

func TestTransformRequestInvalidJSON(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai", base.Config{})}
	in := json.RawMessage("{not json")
	out, err := e.TransformRequest("m", in, false, provider.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(in) {
		t.Errorf("invalid JSON should pass through unchanged")
	}
}

func TestTransformRequestNonMapBody(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai", base.Config{})}
	in := json.RawMessage(`[1,2,3]`)
	out, err := e.TransformRequest("m", in, false, provider.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(in) {
		t.Errorf("non-map body should pass through unchanged")
	}
}

func TestTransformRequestDropClientMetadata(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai", base.Config{Quirks: base.Quirks{DropClientMetadata: true}})}
	in := json.RawMessage(`{"model":"m","client_metadata":{"x":1},"messages":[]}`)
	out, err := e.TransformRequest("m", in, false, provider.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if _, ok := m["client_metadata"]; ok {
		t.Errorf("client_metadata should be dropped")
	}
}

func TestTransformRequestKimiProviderInjectsReasoning(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("kimi", base.Config{})}
	in := json.RawMessage(`{"model":"k2.5","messages":[{"role":"assistant","content":"ok","tool_calls":[{"id":"1"}]}]}`)
	out, err := e.TransformRequest("k2.5", in, false, provider.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := assistantReasoning(parseBody(t, out))
	if rc != " " {
		t.Errorf("reasoning_content = %q, want \" \" (kimi provider)", rc)
	}
}

func TestTransformRequestStaticReasoningInjectUsed(t *testing.T) {
	// When Config.ReasoningInject is set, applyModelReasoningInject is skipped.
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("kimi", base.Config{ReasoningInject: &base.ReasoningInject{Scope: "toolCalls"}})}
	in := json.RawMessage(`{"model":"k2.5","messages":[{"role":"assistant","content":"ok","tool_calls":[{"id":"1"}]}]}`)
	out, err := e.TransformRequest("k2.5", in, false, provider.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := assistantReasoning(parseBody(t, out))
	if rc != " " {
		t.Errorf("reasoning_content = %q, want \" \" (static inject)", rc)
	}
}

// --- applyJSONSchemaFallback ---

func TestApplyJSONSchemaFallbackWithStringSystem(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai-compatible-x", base.Config{})}
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "be helpful"},
			map[string]any{"role": "user", "content": "hi"},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"schema": map[string]any{"type": "object"},
			},
		},
	}
	out := e.applyJSONSchemaFallback(body)
	rf, _ := out["response_format"].(map[string]any)
	if rf["type"] != "json_object" {
		t.Errorf("response_format.type = %v, want json_object", rf["type"])
	}
	msgs := out["messages"].([]any)
	sys := msgs[0].(map[string]any)
	c, _ := sys["content"].(string)
	if !strings.Contains(c, "You must respond with valid JSON") {
		t.Errorf("system prompt not augmented: %q", c)
	}
}

func TestApplyJSONSchemaFallbackWithArraySystem(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai-compatible-x", base.Config{})}
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "be helpful"}}},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"schema": map[string]any{"type": "object"},
			},
		},
	}
	out := e.applyJSONSchemaFallback(body)
	msgs := out["messages"].([]any)
	sys := msgs[0].(map[string]any)
	c, _ := sys["content"].([]any)
	if len(c) != 2 {
		t.Errorf("expected appended text block, got %v", c)
	}
}

func TestApplyJSONSchemaFallbackNoSystemInserts(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai-compatible-x", base.Config{})}
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"schema": map[string]any{"type": "object"},
			},
		},
	}
	out := e.applyJSONSchemaFallback(body)
	msgs := out["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("expected system message inserted first, got %v", msgs[0])
	}
}

func TestApplyJSONSchemaFallbackNoResponseFormat(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai-compatible-x", base.Config{})}
	body := map[string]any{"messages": []any{}}
	out := e.applyJSONSchemaFallback(body)
	if _, ok := out["response_format"]; ok {
		t.Errorf("no response_format → body unchanged")
	}
}

func TestApplyJSONSchemaFallbackNonSchemaType(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai-compatible-x", base.Config{})}
	body := map[string]any{
		"response_format": map[string]any{"type": "text"},
	}
	out := e.applyJSONSchemaFallback(body)
	rf, _ := out["response_format"].(map[string]any)
	if rf["type"] != "text" {
		t.Errorf("response_format.type = %v, want text (unchanged)", rf["type"])
	}
}

func TestApplyJSONSchemaFallbackNoSchemaField(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai-compatible-x", base.Config{})}
	body := map[string]any{
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{},
		},
	}
	out := e.applyJSONSchemaFallback(body)
	rf, _ := out["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Errorf("expected unchanged when no schema field, got %v", rf["type"])
	}
}

// --- injectReasoning / shouldInjectReasoning ---

func TestInjectReasoningToolCallsScopeNoToolCalls(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "content": "ok"},
		},
	}
	out := e.injectReasoning(body, "toolCalls")
	rc, _ := assistantReasoning(out)
	if rc != "" {
		t.Errorf("reasoning_content = %q, want empty (no tool_calls)", rc)
	}
}

func TestInjectReasoningNonAssistantUnchanged(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	out := e.injectReasoning(body, "all")
	rc, _ := assistantReasoning(out)
	if rc != "" {
		t.Errorf("non-assistant should not get reasoning, got %q", rc)
	}
}

func TestInjectReasoningNoMessages(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	body := map[string]any{"model": "m"}
	out := e.injectReasoning(body, "all")
	if _, ok := out["messages"]; ok {
		t.Errorf("messages should not be added when absent")
	}
}

func TestInjectReasoningNonMapMessage(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("p", base.Config{})}
	body := map[string]any{
		"messages": []any{"raw-string-msg"},
	}
	out := e.injectReasoning(body, "all")
	msgs := out["messages"].([]any)
	if msgs[0] != "raw-string-msg" {
		t.Errorf("non-map message should pass through, got %v", msgs[0])
	}
}

// --- New ---

func TestNewWrapsBase(t *testing.T) {
	e := New("openai", base.Config{BaseURL: "https://x.example"})
	if e.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", e.Provider)
	}
	if e.BaseExecutor == nil {
		t.Errorf("BaseExecutor nil")
	}
	if e.Config.BaseURL != "https://x.example" {
		t.Errorf("Config.BaseURL = %q", e.Config.BaseURL)
	}
}

// helper: parse a transformed body into a map.
func parseBody(t *testing.T, b json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parseBody: %v (%s)", err, string(b))
	}
	return m
}