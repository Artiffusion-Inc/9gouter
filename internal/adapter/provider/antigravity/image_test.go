package antigravityexec

// image_test.go pins the Antigravity image-generation contract: the
// `image_gen` envelope (requestType, clean model, imageConfig.aspectRatio,
// fixed generationConfig, text-only merged contents), the non-streaming
// generateContent action, Bearer auth (never ?key= query), and inline image
// extraction. Pure-logic transform/extract tests + an E2E Execute test that
// captures the upstream body and URL via a real httptest.Server (no mock
// executor; real BaseExecutor.Execute through the Execute override's image
// branch).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/proxy"
	domain "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// === isImageModel / parseImageConfig ===

func TestIsImageModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gemini-3.1-flash-image", true},
		{"gemini-3.1-flash-image-16x9", true},
		{"imagen-3.0", true},
		{"some-image-generation-model", true},
		{"gemini-2.5-pro", false},
		{"", false},
		{"claude-opus-4", false},
	}
	for _, c := range cases {
		if got := isImageModel(c.model); got != c.want {
			t.Errorf("isImageModel(%q)=%v want %v", c.model, got, c.want)
		}
	}
}

func TestParseImageConfig(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"gemini-3.1-flash-image", "1:1"},       // no suffix
		{"gemini-3.1-flash-image-16x9", "16:9"}, // small ratio literal
		{"gemini-3.1-flash-image-9x16", "9:16"},
		{"gemini-3.1-flash-image-1024x768", "4:3"},   // resolution GCD-reduced
		{"gemini-3.1-flash-image-1920x1080", "16:9"}, // 1920/120 : 1080/120
		{"gemini-3.1-flash-image-768x1024", "3:4"},
	}
	for _, c := range cases {
		got := parseImageConfig(c.model)["aspectRatio"]
		if got != c.want {
			t.Errorf("parseImageConfig(%q).aspectRatio=%q want %q", c.model, got, c.want)
		}
	}
}

func TestCleanImageModel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"gemini-3.1-flash-image-1024x768", "gemini-3.1-flash-image"},
		{"gemini-3.1-flash-image", "gemini-3.1-flash-image"},
		{"imagen-3.0-16x9", "imagen-3.0"},
	}
	for _, c := range cases {
		if got := cleanImageModel(c.in); got != c.want {
			t.Errorf("cleanImageModel(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// === transformImageRequest (pure logic) ===

func imageTransform(t *testing.T, model, body string, creds domain.Credentials) map[string]any {
	t.Helper()
	e := New(base.Config{ID: "antigravity"})
	out, err := e.transformImageRequest(model, json.RawMessage(body), creds)
	if err != nil {
		t.Fatalf("transformImageRequest: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, string(out))
	}
	return m
}

// TestTransformImage_Envelope verifies the image_gen envelope shape: clean
// model, requestType, userAgent, fixed generationConfig, imageConfig.aspectRatio.
func TestTransformImage_Envelope(t *testing.T) {
	env := imageTransform(t, "gemini-3.1-flash-image-16x9", `{"contents":[{"role":"user","parts":[{"text":"a cat"}]}]}`, domain.Credentials{})
	if env["model"] != "gemini-3.1-flash-image" {
		t.Errorf("model=%v want gemini-3.1-flash-image (suffix stripped)", env["model"])
	}
	if env["userAgent"] != "antigravity" {
		t.Errorf("userAgent=%v want antigravity", env["userAgent"])
	}
	if env["requestType"] != "image_gen" {
		t.Errorf("requestType=%v want image_gen", env["requestType"])
	}
	if env["project"] == "" {
		t.Error("missing envelope.project")
	}
	rid, _ := env["requestId"].(string)
	if !strings.HasPrefix(rid, "agent/") {
		t.Errorf("requestId=%q want agent/... prefix", rid)
	}
	req, ok := env["request"].(map[string]any)
	if !ok {
		t.Fatal("envelope.request is not an object")
	}
	gc, ok := req["generationConfig"].(map[string]any)
	if !ok {
		t.Fatal("missing generationConfig")
	}
	if gc["temperature"] != 1.0 {
		t.Errorf("temperature=%v want 1.0", gc["temperature"])
	}
	if gc["topP"] != 0.95 {
		t.Errorf("topP=%v want 0.95", gc["topP"])
	}
	if gc["topK"] != float64(40) {
		t.Errorf("topK=%v want 40", gc["topK"])
	}
	if gc["maxOutputTokens"] != float64(8192) {
		t.Errorf("maxOutputTokens=%v want 8192", gc["maxOutputTokens"])
	}
	ic, ok := gc["imageConfig"].(map[string]any)
	if !ok {
		t.Fatal("missing imageConfig")
	}
	if ic["aspectRatio"] != "16:9" {
		t.Errorf("imageConfig.aspectRatio=%v want 16:9", ic["aspectRatio"])
	}
	// No tools / systemInstruction / safetySettings for image gen.
	if _, has := req["tools"]; has {
		t.Error("image request must not carry tools")
	}
	if _, has := req["systemInstruction"]; has {
		t.Error("image request must not carry systemInstruction")
	}
	if _, has := req["safetySettings"]; has {
		t.Error("image request must not carry safetySettings")
	}
	if _, has := req["sessionId"]; !has {
		t.Error("missing sessionId")
	}
}

// TestTransformImage_TextOnlyMerge verifies non-image non-text parts
// (functionCall, thought) are dropped and text + inlineData parts across
// messages are kept (image-edit inputs preserved per spec).
func TestTransformImage_TextOnlyMerge(t *testing.T) {
	body := `{"contents":[
		{"role":"user","parts":[{"text":"hello"},{"inlineData":{"mimeType":"image/png","data":"abc"}},{"thought":true}]},
		{"role":"model","parts":[{"text":"ack"}]},
		{"role":"user","parts":[{"functionCall":{"name":"f"}}]}
	]}`
	env := imageTransform(t, "gemini-3.1-flash-image", body, domain.Credentials{})
	req, _ := env["request"].(map[string]any)
	contents, _ := req["contents"].([]any)
	// Two messages carry text/inlineData parts (user+hello+inlineData,
	// model+ack); the third has only a functionCall and is dropped.
	if len(contents) != 2 {
		t.Fatalf("contents len=%d want 2 (text/inlineData filter)", len(contents))
	}
	first, _ := contents[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("first role=%v want user", first["role"])
	}
	parts, _ := first["parts"].([]any)
	// text + inlineData kept; thought dropped.
	if len(parts) != 2 {
		t.Errorf("first parts len=%d want 2 (text + inlineData kept, thought dropped)", len(parts))
	}
	pm0, _ := parts[0].(map[string]any)
	if pm0["text"] != "hello" {
		t.Errorf("first part text=%v want hello", pm0["text"])
	}
	pm1, _ := parts[1].(map[string]any)
	if _, has := pm1["inlineData"]; !has {
		t.Error("inlineData part dropped (image-edit input must be preserved)")
	}
}

// TestTransformImage_ProjectFromCredentials verifies a credential projectId
// flows into envelope.project.
func TestTransformImage_ProjectFromCredentials(t *testing.T) {
	env := imageTransform(t, "gemini-3.1-flash-image", `{"contents":[]}`, domain.Credentials{ProviderSpecificData: map[string]any{"projectId": "img-proj"}})
	if env["project"] != "img-proj" {
		t.Errorf("project=%v want img-proj", env["project"])
	}
}

// TestTransformImage_AcceptsBodyRequestContents verifies the body.request.contents
// path the legacy transformRequest reads first.
func TestTransformImage_AcceptsBodyRequestContents(t *testing.T) {
	env := imageTransform(t, "gemini-3.1-flash-image", `{"request":{"contents":[{"role":"user","parts":[{"text":"via request"}]}]}}`, domain.Credentials{})
	req, _ := env["request"].(map[string]any)
	contents, _ := req["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents len=%d want 1", len(contents))
	}
}

// === extractImageFromResponse ===

func TestExtractImageFromResponse(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"text":"a"},{"inlineData":{"data":"img1","mimeType":"image/png"}},{"inlineData":{"data":"img2"}}]}}]}`
	got := extractImageFromResponse([]byte(body))
	if len(got) != 2 || got[0] != "img1" || got[1] != "img2" {
		t.Errorf("extract=%v want [img1 img2]", got)
	}
}

func TestExtractImageFromResponse_NestedResponse(t *testing.T) {
	// Legacy normalize reads responseBody.response.candidates as a fallback.
	body := `{"response":{"candidates":[{"content":{"parts":[{"inlineData":{"data":"nested"}}]}}]}}`
	got := extractImageFromResponse([]byte(body))
	if len(got) != 1 || got[0] != "nested" {
		t.Errorf("extract=%v want [nested]", got)
	}
}

func TestExtractImageFromResponse_None(t *testing.T) {
	got := extractImageFromResponse([]byte(`{"candidates":[{"content":{"parts":[{"text":"no image"}]}}]}`))
	if len(got) != 0 {
		t.Errorf("extract=%v want empty", got)
	}
}

// === E2E Execute (image path) ===

// TestExecute_ImageEnvelopeSentUpstream verifies the E2E image Execute path:
// the upstream receives the `image_gen` envelope (clean model, imageConfig,
// fixed generationConfig, non-streaming action), Bearer auth header, and NO
// `?key=` query. Captures via a real httptest.Server — no mock executor.
func TestExecute_ImageEnvelopeSentUpstream(t *testing.T) {
	var capturedBody, capturedPath, capturedAuth, capturedAccept string
	var capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		capturedPath = r.URL.Path
		capturedURL = r.URL.String()
		capturedAuth = r.Header.Get("Authorization")
		capturedAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"data":"iVBORw0KGgo=","mimeType":"image/png"}}]}}]}`))
	}))
	defer srv.Close()

	e := New(base.Config{
		ID:      "antigravity",
		BaseURL: srv.URL,
		Format:  "antigravity",
	})
	e.Fetch = mockFetchTo(srv)

	resp, err := e.Execute(context.Background(), domain.ExecRequest{
		Model: "gemini-3.1-flash-image-16x9",
		Body:  json.RawMessage(`{"contents":[{"role":"user","parts":[{"text":"a futuristic city"}]}]}`),
		// Stream=true is intentionally passed to prove the image path forces
		// non-streaming regardless.
		Stream: true,
		Credentials: domain.Credentials{
			AccessToken:          "ag-oauth-token",
			ProviderSpecificData: map[string]any{"_connectionId": "conn-img", "projectId": "proj-img"},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer resp.Response.Body.Close()
	resp.Done()

	// Non-streaming action generateContent, NOT streamGenerateContent.
	if !strings.HasSuffix(capturedPath, "/v1internal:generateContent") {
		t.Errorf("path=%q want .../v1internal:generateContent (non-streaming)", capturedPath)
	}
	if strings.Contains(capturedURL, "alt=sse") {
		t.Errorf("url=%q must not carry alt=sse for image gen", capturedURL)
	}
	// Bearer auth, never ?key= query.
	if capturedAuth != "Bearer ag-oauth-token" {
		t.Errorf("Authorization=%q want Bearer ag-oauth-token", capturedAuth)
	}
	if strings.Contains(capturedURL, "key=") {
		t.Errorf("url=%q must not put credential in query (Bearer only)", capturedURL)
	}
	// Non-streaming → no text/event-stream Accept.
	if capturedAccept == "text/event-stream" {
		t.Errorf("Accept=%q; image gen must not request SSE", capturedAccept)
	}

	var up map[string]any
	if err := json.Unmarshal([]byte(capturedBody), &up); err != nil {
		t.Fatalf("upstream body not JSON: %v (body=%s)", err, capturedBody)
	}
	if up["requestType"] != "image_gen" {
		t.Errorf("requestType=%v want image_gen", up["requestType"])
	}
	if up["model"] != "gemini-3.1-flash-image" {
		t.Errorf("model=%v want gemini-3.1-flash-image (suffix stripped)", up["model"])
	}
	if up["project"] != "proj-img" {
		t.Errorf("project=%v want proj-img", up["project"])
	}
	req, _ := up["request"].(map[string]any)
	gc, _ := req["generationConfig"].(map[string]any)
	ic, _ := gc["imageConfig"].(map[string]any)
	if ic["aspectRatio"] != "16:9" {
		t.Errorf("imageConfig.aspectRatio=%v want 16:9", ic["aspectRatio"])
	}
	if gc["maxOutputTokens"] != float64(8192) {
		t.Errorf("maxOutputTokens=%v want 8192", gc["maxOutputTokens"])
	}
}

// TestExecute_ImageExtractsInline verifies the image Execute response is the
// raw Gemini candidates body (the caller extracts inlineData).
func TestExecute_ImageExtractsInline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"data":"ZHVtbXk=","mimeType":"image/png"}}]}}]}`))
	}))
	defer srv.Close()

	e := New(base.Config{ID: "antigravity", BaseURL: srv.URL, Format: "antigravity"})
	e.Fetch = mockFetchTo(srv)

	resp, err := e.Execute(context.Background(), domain.ExecRequest{
		Model:  "imagen-3.0",
		Body:   json.RawMessage(`{"contents":[{"role":"user","parts":[{"text":"x"}]}]}`),
		Stream: false,
		Credentials: domain.Credentials{
			AccessToken:          "tok",
			ProviderSpecificData: map[string]any{"projectId": "p"},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer resp.Response.Body.Close()
	body, _ := io.ReadAll(resp.Response.Body)
	resp.Done()
	imgs := extractImageFromResponse(body)
	if len(imgs) != 1 || imgs[0] != "ZHVtbXk=" {
		t.Errorf("extract=%v want [ZHVtbXk=]", imgs)
	}
}

// keep proxy import referenced (mockFetchTo uses it).
var _ = proxy.Options{}
