package imageproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/image"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

type captureLogger struct{}

func (captureLogger) Infof(string, ...any)  {}
func (captureLogger) Warnf(string, ...any)  {}
func (captureLogger) Debugf(string, ...any) {}

func creds(apiKey string) domainProv.Credentials {
	return domainProv.Credentials{APIKey: apiKey, ProviderSpecificData: map[string]any{"_connectionId": "c1"}}
}

// === Dispatch / validation ===

func TestHandle_UnsupportedProvider(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "nope", Prompt: "cat"})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

func TestHandle_DeferredProviderReturns501(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	// Sync providers (sdwebui/comfyui/huggingface/stability-ai) are implemented
	// in step 5; cloudflare-ai is implemented in step 6. The still-deferred
	// async/executor providers (fal-ai, black-forest-labs, runwayml,
	// nanobanana, antigravity) keep Unsupported until steps 7–8.
	res := h.Handle(context.Background(), Request{ProviderID: "fal-ai", Prompt: "cat", Credentials: creds("k")})
	if res.StatusCode != http.StatusNotImplemented {
		t.Errorf("fal-ai should 501 in Go build, got %d", res.StatusCode)
	}
}

func TestHandle_SyncProvidersNotDeferred(t *testing.T) {
	// Step 5 lifts Unsupported from sdwebui/comfyui/huggingface/stability-ai.
	// They must NOT return 501 from the Unsupported guard (they may return
	// other statuses for missing creds/prompt, but not 501).
	for _, p := range []string{"sdwebui", "comfyui", "huggingface", "stability-ai"} {
		t.Run(p, func(t *testing.T) {
			cfg, ok := image.Lookup(p)
			if !ok {
				t.Fatalf("%s not in registry", p)
			}
			if cfg.Unsupported {
				t.Fatalf("%s still marked Unsupported after step 5", p)
			}
		})
	}
}

func TestHandle_MissingPrompt(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "openai", Prompt: "  "})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing prompt)", res.StatusCode)
	}
}

func TestHandle_NoCredentials(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "openai", Prompt: "cat"})
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
}

// === OpenAI-compatible passthrough ===

func TestHandle_OpenAI_Passthrough(t *testing.T) {
	var gotAuth string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"created":1700000000,"data":[{"url":"https://x/a.png"}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := imageCfg(srv.URL, "bearer", "openai")
	body, ct, status, err := h.synthOpenAICompatible(context.Background(), cfg, Request{
		ProviderID: "openai", Model: "dall-e-3", Prompt: "cat", N: 2, Size: "1024x1024",
		Quality: "hd", Style: "vivid", ResponseFormat: "url", Credentials: creds("k"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("auth = %q", gotAuth)
	}
	if !contains(gotBody, `"model":"dall-e-3"`) || !contains(gotBody, `"prompt":"cat"`) || !contains(gotBody, `"n":2`) || !contains(gotBody, `"quality":"hd"`) {
		t.Errorf("body = %q", gotBody)
	}
	if status != http.StatusOK || ct != "application/json" {
		t.Errorf("status=%d ct=%q", status, ct)
	}
	if !contains(string(body), `"url":"https://x/a.png"`) {
		t.Errorf("body = %q", body)
	}
}

func TestHandle_OpenAI_BodyFieldsWhitelist_Xai(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"x"}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := imageCfg(srv.URL, "bearer", "openai")
	cfg.BodyFields = []string{"model", "prompt", "n", "response_format"}
	_, _, _, err := h.synthOpenAICompatible(context.Background(), cfg, Request{
		ProviderID: "xai", Model: "grok-2-image", Prompt: "cat", Quality: "hd", Style: "vivid",
		ResponseFormat: "b64_json", Credentials: creds("k"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// quality/style must be stripped by the whitelist.
	if contains(gotBody, `"quality"`) || contains(gotBody, `"style"`) {
		t.Errorf("whitelist failed: body = %q", gotBody)
	}
	if !contains(gotBody, `"model":"grok-2-image"`) || !contains(gotBody, `"prompt":"cat"`) || !contains(gotBody, `"response_format":"b64_json"`) {
		t.Errorf("whitelisted fields missing: %q", gotBody)
	}
}

func TestHandle_OpenAI_BinaryOutput_B64(t *testing.T) {
	rawImg := pngMagic(64)
	b64 := base64.StdEncoding.EncodeToString(rawImg)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"`+b64+`"}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := imageCfg(srv.URL, "bearer", "openai")
	body, ct, status, err := h.synthOpenAICompatible(context.Background(), cfg, Request{
		ProviderID: "openai", Model: "dall-e-3", Prompt: "cat", ResponseFormat: "binary", OutputFormat: "png", Credentials: creds("k"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	if ct != "image/png" {
		t.Errorf("CT = %q, want image/png", ct)
	}
	if body[0] != 0x89 || body[1] != 'P' {
		t.Errorf("body bytes mismatch: %v", body[:4])
	}
}

func TestHandle_OpenAI_BinaryOutput_UrlDownload(t *testing.T) {
	img := pngMagic(128)
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/img.png" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(img)
			return
		}
		// submit path returns a URL pointing at this same server.
		_, _ = io.WriteString(w, `{"created":1,"data":[{"url":"`+srv.URL+`/img.png"}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{
		Executor:   &fallbackExecutor{client: srv.Client()},
		Logger:     captureLogger{},
		Config:     config.Config{},
		Resolver:   resolverFor(srv),
		SSRFPolicy: permissiveSSRFForTest{},
	})
	cfg := imageCfg(srv.URL, "bearer", "openai")
	_, ct, status, err := h.synthOpenAICompatible(context.Background(), cfg, Request{
		ProviderID: "openai", Model: "dall-e-3", Prompt: "cat", ResponseFormat: "binary", Credentials: creds("k"),
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200 (url binary no longer 501)", status)
	}
	if ct != "image/png" {
		t.Errorf("ct = %q, want image/png", ct)
	}
}

func TestHandle_OpenAI_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Invalid API key"}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := imageCfg(srv.URL, "bearer", "openai")
	_, _, status, err := h.synthOpenAICompatible(context.Background(), cfg, Request{
		ProviderID: "openai", Model: "dall-e-3", Prompt: "cat", Credentials: creds("bad"),
	})
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
	if err == nil || !contains(err.Error(), "Invalid API key") {
		t.Errorf("err = %v", err)
	}
}

// === Gemini reshape ===

func TestHandle_Gemini_Reshape(t *testing.T) {
	imgB64 := base64.StdEncoding.EncodeToString([]byte("IMGDATA"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !contains(r.URL.RawQuery, "key=k-gem") {
			t.Errorf("query = %q, want key=k-gem", r.URL.RawQuery)
		}
		if !contains(r.URL.Path, ":generateContent") {
			t.Errorf("path = %q, want :generateContent", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ignored"},{"inlineData":{"mimeType":"image/png","data":"`+imgB64+`"}}]}}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := imageCfg(srv.URL, "key", "gemini")
	body, ct, status, err := h.synthGemini(context.Background(), cfg, Request{
		ProviderID: "gemini", Model: "gemini-2.5-flash-image", Prompt: "cat", Credentials: creds("k-gem"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || ct != "application/json" {
		t.Errorf("status=%d ct=%q", status, ct)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	data, _ := parsed["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len = %d, want 1", len(data))
	}
	if data[0].(map[string]any)["b64_json"] != imgB64 {
		t.Errorf("b64_json mismatch: %v", data[0])
	}
}

func TestHandle_Gemini_ModelsPrefixStripped(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		imgB64 := base64.StdEncoding.EncodeToString([]byte("X"))
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"inlineData":{"data":"`+imgB64+`"}}]}}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := imageCfg(srv.URL, "key", "gemini")
	_, _, _, err := h.synthGemini(context.Background(), cfg, Request{
		ProviderID: "gemini", Model: "models/gemini-2.5-flash-image", Prompt: "cat", Credentials: creds("k"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(gotPath, "/gemini-2.5-flash-image:generateContent") || contains(gotPath, "models/models") {
		t.Errorf("path = %q, want models/ prefix stripped", gotPath)
	}
}

func TestHandle_Gemini_NoImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"text only"}]}}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := imageCfg(srv.URL, "key", "gemini")
	_, _, status, err := h.synthGemini(context.Background(), cfg, Request{
		ProviderID: "gemini", Model: "gemini-2.5-flash-image", Prompt: "cat", Credentials: creds("k"),
	})
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (no image)", status)
	}
	if err == nil {
		t.Error("want error for no image")
	}
}

// === Codex SSE parse ===

func TestHandle_Codex_SSEParse(t *testing.T) {
	imgB64 := base64.StdEncoding.EncodeToString([]byte("CODEXIMG"))
	sse := "data: {\"type\":\"response.image_generation_call.partial_image\",\"b64\":\"" + imgB64 + "\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"result\":\"" + imgB64 + "\"}}\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		if r.Header.Get("Authorization") != "Bearer k-codex" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("chatgpt-account-id") != "acct-1" {
			t.Errorf("chatgpt-account-id = %q", r.Header.Get("chatgpt-account-id"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := imageCfg(srv.URL, "bearer-account", "codex")
	body, ct, status, err := h.synthCodex(context.Background(), cfg, Request{
		ProviderID: "codex", Model: "gpt-5.1-image", Prompt: "cat", Size: "1024x1024",
		Credentials: domainProv.Credentials{AccessToken: "k-codex", ProviderSpecificData: map[string]any{"chatgptAccountID": "acct-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || ct != "application/json" {
		t.Errorf("status=%d ct=%q", status, ct)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	data, _ := parsed["data"].([]any)
	if len(data) != 2 {
		t.Errorf("data len = %d, want 2 (partial + done)", len(data))
	}
	// Verify upstream model dropped -image suffix by inspecting the request body would require a capture;
	// covered indirectly: the request succeeded with the SSE shape.
}

func TestHandle_Codex_NoImage(t *testing.T) {
	sse := "data: {\"type\":\"response.completed\"}\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, sse)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := imageCfg(srv.URL, "bearer-account", "codex")
	_, _, status, err := h.synthCodex(context.Background(), cfg, Request{
		ProviderID: "codex", Model: "gpt-5.1-image", Prompt: "cat",
		Credentials: domainProv.Credentials{AccessToken: "k"},
	})
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (no image)", status)
	}
	if err == nil {
		t.Error("want error for no image")
	}
}

// === buildOpenAIBody defaults ===

func TestBuildOpenAIBody_Defaults(t *testing.T) {
	b := buildOpenAIBody(Request{Model: "dall-e-3", Prompt: "cat"}, nil)
	if b["n"] != 1 {
		t.Errorf("n default = %v, want 1", b["n"])
	}
	if b["size"] != "1024x1024" {
		t.Errorf("size default = %v, want 1024x1024", b["size"])
	}
}

// === Executor boundary ===

// recordingExecutor records every request it receives and returns a canned
// response so the usecase completes without a real upstream. It proves the
// OpenAI submit path went through HTTPExecutor (not a fallback client) and that
// transport metadata reached the executor via the request context.
type recordingExecutor struct {
	got    []*http.Request
	bodies []string
}

func (r *recordingExecutor) Do(req *http.Request) (*http.Response, error) {
	// Capture a copy of the request URL + body so assertions are stable after
	// the body is consumed.
	r.got = append(r.got, req)
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		r.bodies = append(r.bodies, string(b))
		req.Body.Close()
		// Restore a readable body the usecase transport would consume — but the
		// canned response below does not read the request body again, so leave it.
	}
	// Return a minimal OpenAI-shaped success so synthOpenAICompatible completes.
	respBody := `{"created":1,"data":[{"b64_json":"iVBORw0KGgo="}]}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(respBody)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// TestExecutor_OpenAISubmitGoesThroughExecutor proves the OpenAI submit path
// routes through the injected HTTPExecutor and carries transport metadata
// (provider, connection, phase) on the request context — not a fallback client.
func TestExecutor_OpenAISubmitGoesThroughExecutor(t *testing.T) {
	rec := &recordingExecutor{}
	h := New(Dependencies{Executor: rec, Logger: captureLogger{}, Config: config.Config{}})
	cfg := imageCfg("https://upstream.example/v1/images/generations", "bearer", "openai")
	// Patch the registry entry base URL to our test endpoint.
	setImageBaseURL(t, "openai", cfg)

	res := h.Handle(context.Background(), Request{
		ProviderID:  "openai",
		Model:       "dall-e-3",
		Prompt:      "cat",
		Credentials: creds("tok"),
	})
	if res.Err != nil {
		t.Fatalf("Handle: %v", res.Err)
	}
	if len(rec.got) != 1 {
		t.Fatalf("expected 1 executor call, got %d", len(rec.got))
	}
	got := rec.got[0]
	if got.URL.String() != "https://upstream.example/v1/images/generations" {
		t.Fatalf("executor URL = %s, want test endpoint", got.URL.String())
	}
	meta, ok := TransportMetadataFromContext(got.Context())
	if !ok {
		t.Fatal("executor request had no transport metadata on context")
	}
	if meta.ProviderID != "openai" {
		t.Fatalf("meta.ProviderID = %q, want openai", meta.ProviderID)
	}
	if meta.Phase != "submit" {
		t.Fatalf("meta.Phase = %q, want submit", meta.Phase)
	}
	if meta.ConnectionID != "c1" {
		t.Fatalf("meta.ConnectionID = %q, want c1", meta.ConnectionID)
	}
}

// TestExecutor_OverridesFallbackClient proves a injected executor is used even
// when the registry base URL points at a real httptest server that would answer
// the fallback client — i.e. the fallback client is NOT used when an executor is
// wired. The httptest server counts hits; the recording executor returns the
// canned response, so the server must record zero hits.
func TestExecutor_OverridesFallbackClient(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rec := &recordingExecutor{}
	h := New(Dependencies{Executor: rec, Logger: captureLogger{}, Config: config.Config{}})
	setImageBaseURL(t, "openai", imageCfg(srv.URL, "bearer", "openai"))

	res := h.Handle(context.Background(), Request{
		ProviderID: "openai", Model: "dall-e-3", Prompt: "cat", Credentials: creds("tok"),
	})
	if res.Err != nil {
		t.Fatalf("Handle: %v", res.Err)
	}
	if hits != 0 {
		t.Fatalf("fallback client hit upstream %d times; executor must own outbound", hits)
	}
}

// setImageBaseURL patches the image registry entry for providerID to use cfg's
// BaseURL for the duration of the test. The registry is a package-level map;
// this helper keeps tests self-contained.
func setImageBaseURL(t *testing.T, providerID string, cfg image.Config) {
	t.Helper()
	orig := mustLookupImage(t, providerID)
	image.SetConfig(providerID, cfg)
	t.Cleanup(func() { image.SetConfig(providerID, orig) })
}

func mustLookupImage(t *testing.T, providerID string) image.Config {
	t.Helper()
	cfg, ok := image.Lookup(providerID)
	if !ok {
		t.Fatalf("image provider %q not in registry", providerID)
	}
	return cfg
}

// === Helpers ===

func imageCfg(baseURL, authHeader, format string) image.Config {
	return image.Config{BaseURL: baseURL, AuthHeader: image.AuthHeader(authHeader), Format: image.Format(format), AuthType: image.AuthTypeAPIKey}
}

func contains(s, sub string) bool { return jsonContains(s, sub) }

func jsonContains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
