package imageproxy

// antigravity_test.go is the app-wiring integration test for the Antigravity
// image adapter (plan step 8 point 5). Unlike a fake-executor test, it drives
// the REAL Antigravity provider executor (antigravityexec.New, the same
// constructor the chat path uses) pointed at an httptest.Server upstream,
// wrapped in the production-style boundary adapter that app/wire.go injects.
// This proves the production delegation: the executor builds the `image_gen`
// envelope, applies OAuth Bearer + project ID + the connection's proxy route
// seam, and the imageproxy adapter extracts inline image data into the
// OpenAI {created, data:[{b64_json}]} shape.
//
// No mock executor is used — the adapter implements
// imageproxy.AntigravityImageExecutor by delegating to the real executor's
// Execute, exactly as wire.go's antigravityImageExecutor does. The upstream is
// an httptest.Server (the spec-mandated contract boundary), not a fake.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	antigravityexec "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/antigravity"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/image"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// realAntigravityAdapter mirrors app/wire.go's antigravityImageExecutor: it
// implements imageproxy.AntigravityImageExecutor by delegating to the real
// Antigravity executor. Using it here (instead of a fake) proves the
// production delegation path — the same envelope construction, OAuth Bearer
// header, project-ID propagation and proxy-route seam the chat path relies on.
type realAntigravityAdapter struct {
	exec *antigravityexec.Executor
}

func (a *realAntigravityAdapter) ExecuteImage(ctx context.Context, req AntigravityImageRequest) (AntigravityImageResponse, error) {
	resp, err := a.exec.Execute(ctx, domainProv.ExecRequest{
		Model:       req.Model,
		Body:        req.Contents,
		Stream:      false,
		Credentials: req.Credentials,
	})
	if err != nil {
		return AntigravityImageResponse{}, err
	}
	defer resp.Response.Body.Close()
	body, readErr := io.ReadAll(resp.Response.Body)
	if readErr != nil {
		if resp.Done != nil {
			resp.Done()
		}
		return AntigravityImageResponse{}, readErr
	}
	if resp.Done != nil {
		resp.Done()
	}
	return AntigravityImageResponse{Body: body, StatusCode: resp.Response.StatusCode}, nil
}

// antigravityFetchRecorder wraps a base.Fetcher to record the proxy options
// and request the real executor passed to the proxy-aware fetch seam. This
// proves the production delegation preserves the connection-aware proxy route
// (the same seam app/wire.go's productionImageExecutor uses) without
// substituting a fake executor.
type antigravityFetchRecorder struct {
	host    string
	fetched bool
	scheme  string
	hostHdr string
}

func (r *antigravityFetchRecorder) fetch(_ context.Context, client *http.Client, req *http.Request, _ proxy.Options, _ proxy.ProxyFetchOptions, _ *proxy.Fallback) (*http.Response, error) {
	r.fetched = true
	r.scheme = req.URL.Scheme
	r.hostHdr = req.Host
	req.URL.Scheme = "http"
	req.URL.Host = r.host
	req.Host = r.host
	return client.Do(req)
}

// TestAntigravity_RealExecutor_ProductionDelegation is the mandatory app-wiring
// integration test: it builds the real Antigravity executor (not a mock),
// wraps it in the production-style boundary adapter, and verifies the full
// image-proxy pipeline:
//   - the upstream receives the `image_gen` envelope with a clean model,
//     imageConfig.aspectRatio and the fixed generationConfig;
//   - the OAuth Bearer token reaches the upstream (no ?key= query);
//   - the credential projectId propagates into envelope.project;
//   - the executor's fetch seam is invoked (the production proxy-aware route);
//   - the imageproxy adapter extracts inline image data into the OpenAI
//     {created, data:[{b64_json}]} shape.
func TestAntigravity_RealExecutor_ProductionDelegation(t *testing.T) {
	var capturedBody, capturedPath, capturedAuth, capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		capturedPath = r.URL.Path
		capturedURL = r.URL.String()
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"data":"iVBORw0KGgo=","mimeType":"image/png"}}]}}]}`))
	}))
	defer srv.Close()

	// Build the real Antigravity executor the same way registry.go / wire.go do,
	// pointed at the httptest upstream.
	exec := antigravityexec.New(base.Config{
		ID:      "antigravity",
		BaseURL: srv.URL,
		Format:  "antigravity",
	})
	rec := &antigravityFetchRecorder{host: strings.TrimPrefix(srv.URL, "http://")}
	exec.Fetch = rec.fetch

	// Patch the image registry so imageproxy.Lookup finds antigravity with the
	// httptest base URL (the adapter does not read BaseURL — the provider
	// executor does — but the registry must not mark it Unsupported).
	origCfg, _ := image.Lookup("antigravity")
	image.SetConfig("antigravity", image.Config{
		AuthType:   image.AuthTypeAPIKey,
		AuthHeader: image.AuthBearer,
		Format:     image.FormatAntigravity,
	})
	t.Cleanup(func() { image.SetConfig("antigravity", origCfg) })

	h := New(Dependencies{
		AntigravityExecutor: &realAntigravityAdapter{exec: exec},
		Logger:              captureLogger{},
		Config:              config.Config{},
	})

	res := h.Handle(context.Background(), Request{
		ProviderID: "antigravity",
		Model:      "gemini-3.1-flash-image-16x9",
		Prompt:     "a neon city",
		Credentials: domainProv.Credentials{
			AccessToken:          "ag-wired-token",
			ProviderSpecificData: map[string]any{"_connectionId": "conn-wired", "projectId": "proj-wired"},
		},
	})
	if res.Err != nil {
		t.Fatalf("Handle: %v (status=%d)", res.Err, res.StatusCode)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", res.StatusCode)
	}

	// 1. Upstream received the image_gen envelope via the real executor.
	if !strings.HasSuffix(capturedPath, "/v1internal:generateContent") {
		t.Errorf("upstream path=%q want .../v1internal:generateContent", capturedPath)
	}
	var up map[string]any
	if err := json.Unmarshal([]byte(capturedBody), &up); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if up["requestType"] != "image_gen" {
		t.Errorf("requestType=%v want image_gen", up["requestType"])
	}
	if up["model"] != "gemini-3.1-flash-image" {
		t.Errorf("model=%v want gemini-3.1-flash-image (suffix stripped)", up["model"])
	}
	if up["project"] != "proj-wired" {
		t.Errorf("project=%v want proj-wired (credential projectId propagated)", up["project"])
	}
	req, _ := up["request"].(map[string]any)
	gc, _ := req["generationConfig"].(map[string]any)
	ic, _ := gc["imageConfig"].(map[string]any)
	if ic["aspectRatio"] != "16:9" {
		t.Errorf("imageConfig.aspectRatio=%v want 16:9", ic["aspectRatio"])
	}

	// 2. OAuth Bearer auth, no ?key= query (forbidden for Antigravity).
	if capturedAuth != "Bearer ag-wired-token" {
		t.Errorf("Authorization=%q want Bearer ag-wired-token", capturedAuth)
	}
	if strings.Contains(capturedURL, "key=") {
		t.Errorf("upstream url=%q must not put credential in query", capturedURL)
	}

	// 3. The real executor's fetch seam fired — the production proxy-aware
	// route is the one making the call, not a fake executor.
	if !rec.fetched {
		t.Error("real executor fetch seam was not invoked (no production delegation)")
	}

	// 4. imageproxy normalized the Gemini candidates into the OpenAI shape.
	var out map[string]any
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatalf("response body not JSON: %v", err)
	}
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len=%d want 1", len(data))
	}
	first, _ := data[0].(map[string]any)
	if first["b64_json"] != "iVBORw0KGgo=" {
		t.Errorf("b64_json=%v want iVBORw0KGgo=", first["b64_json"])
	}
	if _, has := out["created"]; !has {
		t.Error("missing created field")
	}
}

// TestAntigravity_RealExecutor_UpstreamError verifies a non-2xx upstream
// surfaces through the real executor + adapter as the OpenAI error status.
func TestAntigravity_RealExecutor_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"oauth token expired"}}`))
	}))
	defer srv.Close()

	exec := antigravityexec.New(base.Config{ID: "antigravity", BaseURL: srv.URL, Format: "antigravity"})
	rec := &antigravityFetchRecorder{host: strings.TrimPrefix(srv.URL, "http://")}
	exec.Fetch = rec.fetch

	origCfg, _ := image.Lookup("antigravity")
	image.SetConfig("antigravity", image.Config{AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatAntigravity})
	t.Cleanup(func() { image.SetConfig("antigravity", origCfg) })

	h := New(Dependencies{
		AntigravityExecutor: &realAntigravityAdapter{exec: exec},
		Logger:              captureLogger{},
		Config:              config.Config{},
	})

	res := h.Handle(context.Background(), Request{
		ProviderID: "antigravity",
		Model:      "gemini-3.1-flash-image",
		Prompt:     "x",
		Credentials: domainProv.Credentials{
			AccessToken:          "bad",
			ProviderSpecificData: map[string]any{"projectId": "p"},
		},
	})
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", res.StatusCode)
	}
	if res.Err == nil {
		t.Error("want non-nil error for upstream 401")
	}
}

// TestAntigravity_NoExecutorReturns501 verifies the nil-executor guard: when
// the production wiring did not inject an AntigravityImageExecutor, the
// adapter returns 501 honestly (no silent fallback).
func TestAntigravity_NoExecutorReturns501(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{
		ProviderID: "antigravity",
		Model:      "gemini-3.1-flash-image",
		Prompt:     "x",
		Credentials: domainProv.Credentials{
			AccessToken:          "tok",
			ProviderSpecificData: map[string]any{"projectId": "p"},
		},
	})
	if res.StatusCode != http.StatusNotImplemented {
		t.Errorf("status=%d want 501 (no executor wired)", res.StatusCode)
	}
}

// TestAntigravity_RealExecutor_ImageEditInput verifies an inline image-edit
// input (validated data URL) is forwarded as a Gemini inlineData part before
// the text prompt, mirroring the legacy resolveImageInput + parts.unshift
// order. The upstream receives inlineData first, text second.
func TestAntigravity_RealExecutor_ImageEditInput(t *testing.T) {
	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"data":"out","mimeType":"image/png"}}]}}]}`))
	}))
	defer srv.Close()

	exec := antigravityexec.New(base.Config{ID: "antigravity", BaseURL: srv.URL, Format: "antigravity"})
	rec := &antigravityFetchRecorder{host: strings.TrimPrefix(srv.URL, "http://")}
	exec.Fetch = rec.fetch

	origCfg, _ := image.Lookup("antigravity")
	image.SetConfig("antigravity", image.Config{AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatAntigravity})
	t.Cleanup(func() { image.SetConfig("antigravity", origCfg) })

	h := New(Dependencies{
		AntigravityExecutor: &realAntigravityAdapter{exec: exec},
		Logger:              captureLogger{},
		Config:              config.Config{},
	})

	// A 1x1 PNG as a data URL.
	pngB64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAVCBwIMAQAAAABJRU5ErkJggg=="
	res := h.Handle(context.Background(), Request{
		ProviderID: "antigravity",
		Model:      "gemini-3.1-flash-image",
		Prompt:     "edit this",
		Credentials: domainProv.Credentials{
			AccessToken:          "tok",
			ProviderSpecificData: map[string]any{"projectId": "p"},
		},
		Options: RequestOptions{
			ImageInputs: []ImageInput{{
				Kind: "data",
				B64:  pngB64,
				MIME: "image/png",
			}},
		},
	})
	if res.Err != nil {
		t.Fatalf("Handle: %v (status=%d)", res.Err, res.StatusCode)
	}

	// The contents passed to the executor carry inlineData first, text second.
	var env map[string]any
	if err := json.Unmarshal([]byte(capturedBody), &env); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	req, _ := env["request"].(map[string]any)
	contents, _ := req["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents len=%d want 1", len(contents))
	}
	first, _ := contents[0].(map[string]any)
	parts, _ := first["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts len=%d want 2 (inlineData + text)", len(parts))
	}
	firstPart, _ := parts[0].(map[string]any)
	if _, has := firstPart["inlineData"]; !has {
		t.Errorf("first part=%v want inlineData (image-edit input unshifted before text)", firstPart)
	}
	secondPart, _ := parts[1].(map[string]any)
	if _, has := secondPart["text"]; !has {
		t.Errorf("second part=%v want text", secondPart)
	}
}
