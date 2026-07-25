package imageproxy

// providers_sync_test.go covers the synchronous image-provider adapters from
// step 5 (spec "Contract test каждому sync provider через отдельный
// httptest.Server"): method/path/auth/headers/payload, success normalization,
// upstream status error. Stability is exercised across all three model
// variants (ultra/sd3/core); HuggingFace rejects non-image raw bytes; Codex
// carries the literal `version: 0.136.0` header; SDWebUI fixes the legacy
// literal decision table (dimensions, batch_size, steps).
//
// All upstream contracts are exercised against a real httptest.Server; no mock
// HTTP client is used.

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
)

// === SDWebUI ===

func TestSDWebUI_Contract(t *testing.T) {
	var gotMethod, gotPath, gotCT, gotAuth string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"images":["iVBORw0KGgo=","iVBORw0KGgo="]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := image.Config{BaseURL: srv.URL + "/sdapi/v1/txt2img", AuthType: image.AuthTypeNone, AuthHeader: image.AuthNone, Format: image.FormatSDWebUI}
	setImageBaseURL(t, "sdwebui", cfg)

	res := h.Handle(context.Background(), Request{ProviderID: "sdwebui", Model: "v1-5", Prompt: "cat", Size: "1024x1024", N: 2, NSupplied: true, Credentials: creds("")})
	if res.Err != nil {
		t.Fatalf("Handle: %v (status=%d)", res.Err, res.StatusCode)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/sdapi/v1/txt2img") {
		t.Errorf("path = %q, want .../sdapi/v1/txt2img", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (no-auth)", gotAuth)
	}
	// Payload: width/height/steps/batch_size.
	var p map[string]any
	if err := json.Unmarshal([]byte(gotBody), &p); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if p["width"] != 1024.0 || p["height"] != 1024.0 {
		t.Errorf("dims = %v/%v, want 1024/1024", p["width"], p["height"])
	}
	if p["steps"] != 20.0 {
		t.Errorf("steps = %v, want 20", p["steps"])
	}
	if p["batch_size"] != 2.0 {
		t.Errorf("batch_size = %v, want 2", p["batch_size"])
	}
	// Normalization: two b64_json entries.
	var out map[string]any
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatal(err)
	}
	data, _ := out["data"].([]any)
	if len(data) != 2 {
		t.Errorf("data len = %d, want 2", len(data))
	}
}

func TestSDWebUI_LiteralDecisionTable(t *testing.T) {
	cases := []struct {
		name      string
		size      string
		n         int
		nsupplied bool
		wantW     int
		wantH     int
		wantBatch int
	}{
		{"default absent size absent n", "", 0, false, 1024, 1024, 1},
		{"explicit 1024x1024", "1024x1024", 0, false, 1024, 1024, 1},
		{"768x1280", "768x1280", 0, false, 768, 1280, 1},
		{"n:0 explicit → batch_size 0", "1024x1024", 0, true, 1024, 1024, 0},
		{"n:3 explicit → batch_size 3", "1024x1024", 3, true, 1024, 1024, 3},
		{"badx512 → 512/512", "badx512", 0, false, 512, 512, 1},
		{"512xbad → 512/512", "512xbad", 0, false, 512, 512, 1},
		{"0x0 → 512/512", "0x0", 0, false, 512, 512, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				_, _ = io.WriteString(w, `{"images":["x"]}`)
			}))
			defer srv.Close()
			h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
			cfg := image.Config{BaseURL: srv.URL, AuthType: image.AuthTypeNone, AuthHeader: image.AuthNone, Format: image.FormatSDWebUI}
			_, _, _, err := h.synthSDWebUI(context.Background(), cfg, Request{
				ProviderID: "sdwebui", Prompt: "p", Size: c.size, N: c.n, NSupplied: c.nsupplied,
			})
			if err != nil {
				t.Fatalf("synthSDWebUI: %v", err)
			}
			var p map[string]any
			if err := json.Unmarshal([]byte(gotBody), &p); err != nil {
				t.Fatal(err)
			}
			if int(p["width"].(float64)) != c.wantW {
				t.Errorf("width = %v, want %d", p["width"], c.wantW)
			}
			if int(p["height"].(float64)) != c.wantH {
				t.Errorf("height = %v, want %d", p["height"], c.wantH)
			}
			if int(p["batch_size"].(float64)) != c.wantBatch {
				t.Errorf("batch_size = %v, want %d", p["batch_size"], c.wantBatch)
			}
			if p["steps"] != 20.0 {
				t.Errorf("steps = %v, want 20", p["steps"])
			}
		})
	}
}

func TestSDWebUI_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"model not loaded"}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := image.Config{BaseURL: srv.URL, AuthType: image.AuthTypeNone, AuthHeader: image.AuthNone, Format: image.FormatSDWebUI}
	_, _, status, err := h.synthSDWebUI(context.Background(), cfg, Request{ProviderID: "sdwebui", Prompt: "p"})
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
	if err == nil || !strings.Contains(err.Error(), "model not loaded") {
		t.Errorf("err = %v", err)
	}
}

func TestSDWebUI_BinaryOutput(t *testing.T) {
	rawImg := pngMagic(64)
	b64 := base64.StdEncoding.EncodeToString(rawImg)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"images":["`+b64+`"]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := image.Config{BaseURL: srv.URL, AuthType: image.AuthTypeNone, AuthHeader: image.AuthNone, Format: image.FormatSDWebUI}
	body, ct, status, err := h.synthSDWebUI(context.Background(), cfg, Request{ProviderID: "sdwebui", Prompt: "p", ResponseFormat: "binary", OutputFormat: "png"})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || ct != "image/png" {
		t.Errorf("status=%d ct=%q", status, ct)
	}
	if body[0] != 0x89 || body[1] != 'P' {
		t.Errorf("bytes = %v, want PNG magic", body[:4])
	}
}

// === ComfyUI ===

func TestComfyUI_Contract(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		// Legacy normalize is passthrough of an OpenAI-shaped body.
		_, _ = io.WriteString(w, `{"created":1700000000,"data":[{"url":"https://x/a.png"}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := image.Config{BaseURL: srv.URL, AuthType: image.AuthTypeNone, AuthHeader: image.AuthNone, Format: image.FormatComfyUI}
	setImageBaseURL(t, "comfyui", cfg)

	res := h.Handle(context.Background(), Request{ProviderID: "comfyui", Model: "default", Prompt: "cat"})
	if res.Err != nil {
		t.Fatalf("Handle: %v", res.Err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/" {
		t.Errorf("path = %q, want /", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (no-auth)", gotAuth)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(gotBody), &p); err != nil {
		t.Fatal(err)
	}
	if p["prompt"] != "cat" {
		t.Errorf("prompt = %v, want cat", p["prompt"])
	}
	// Passthrough normalization: response returned verbatim.
	if !contains(string(res.Body), `"url":"https://x/a.png"`) {
		t.Errorf("body = %q, want passthrough url", res.Body)
	}
}

func TestComfyUI_MalformedResponse502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Not OpenAI-shaped (no data array).
		_, _ = io.WriteString(w, `{"error":"workflow not found"}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := image.Config{BaseURL: srv.URL, AuthType: image.AuthTypeNone, AuthHeader: image.AuthNone, Format: image.FormatComfyUI}
	_, _, status, err := h.synthComfyUI(context.Background(), cfg, Request{ProviderID: "comfyui", Prompt: "p"})
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (malformed)", status)
	}
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Errorf("err = %v, want malformed diagnostic", err)
	}
}

func TestComfyUI_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":"bad workflow"}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := image.Config{BaseURL: srv.URL, AuthType: image.AuthTypeNone, AuthHeader: image.AuthNone, Format: image.FormatComfyUI}
	_, _, status, err := h.synthComfyUI(context.Background(), cfg, Request{ProviderID: "comfyui", Prompt: "p"})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if err == nil {
		t.Error("want error for upstream 400")
	}
}

// === HuggingFace ===

func TestHuggingFace_Contract(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT, gotUA string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotUA = r.Header.Get("User-Agent")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngMagic(64))
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := image.Config{BaseURL: srv.URL + "/models", AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatHuggingFace}
	setImageBaseURL(t, "huggingface", cfg)

	res := h.Handle(context.Background(), Request{ProviderID: "huggingface", Model: "black-forest-labs/FLUX.1-dev", Prompt: "cat", UserAgent: "test-ua", Credentials: creds("hf_tok")})
	if res.Err != nil {
		t.Fatalf("Handle: %v", res.Err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/models/black-forest-labs/FLUX.1-dev" {
		t.Errorf("path = %q, want escaped model segment", gotPath)
	}
	if gotAuth != "Bearer hf_tok" {
		t.Errorf("auth = %q, want Bearer hf_tok", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotUA != "test-ua" {
		t.Errorf("User-Agent = %q", gotUA)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(gotBody), &p); err != nil {
		t.Fatal(err)
	}
	if p["inputs"] != "cat" {
		t.Errorf("inputs = %v, want cat", p["inputs"])
	}
	// Normalization: raw PNG → b64_json.
	var out map[string]any
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatal(err)
	}
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len = %d, want 1", len(data))
	}
	b64, _ := data[0].(map[string]any)["b64_json"].(string)
	if b64 == "" {
		t.Error("b64_json empty")
	}
	dec, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	if dec[0] != 0x89 || dec[1] != 'P' {
		t.Errorf("decoded bytes = %v, want PNG magic", dec[:4])
	}
}

func TestHuggingFace_ModelPathValidation(t *testing.T) {
	cases := []struct {
		name    string
		model   string
		wantErr string
	}{
		{"empty model", "", "missing model"},
		{"query in model", "model?x=1", "query or fragment"},
		{"fragment in model", "model#frag", "query or fragment"},
		{"traversal dot", ".", "traversal segment"},
		{"traversal dotdot", "..", "traversal segment"},
		{"traversal in segment", "org/..", "traversal segment"},
		{"empty segment leading slash", "/model", "traversal segment"},
		{"empty segment trailing slash", "org/", "traversal segment"},
		{"double slash", "org//model", "traversal segment"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := huggingfaceEndpoint("https://api-inference.huggingface.co/models", c.model)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %q, want %q", err.Error(), c.wantErr)
			}
		})
	}
}

func TestHuggingFace_ModelPath_EscapedMultiSegment(t *testing.T) {
	// org/model is a valid HF path; segments are path-escaped.
	endpoint, err := huggingfaceEndpoint("https://api-inference.huggingface.co/models", "black-forest-labs/FLUX.1 dev")
	if err != nil {
		t.Fatalf("want ok, got %v", err)
	}
	// Space escaped as %20, slash preserved as separator.
	if endpoint != "https://api-inference.huggingface.co/models/black-forest-labs/FLUX.1%20dev" {
		t.Errorf("endpoint = %q", endpoint)
	}
}

func TestHuggingFace_NonImageRawRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<html><body>Service unavailable</body></html>`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := image.Config{BaseURL: srv.URL + "/models", AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatHuggingFace}
	_, _, status, err := h.synthHuggingFace(context.Background(), cfg, Request{ProviderID: "huggingface", Model: "m", Prompt: "p", Credentials: creds("k")})
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (non-image)", status)
	}
	if err == nil {
		t.Error("want error for non-image raw bytes")
	}
}

func TestHuggingFace_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"Invalid token"}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := image.Config{BaseURL: srv.URL + "/models", AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatHuggingFace}
	_, _, status, err := h.synthHuggingFace(context.Background(), cfg, Request{ProviderID: "huggingface", Model: "m", Prompt: "p", Credentials: creds("bad")})
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
	if err == nil {
		t.Error("want error for 401")
	}
}

// === Stability AI ===

func TestStability_Contract_AllVariants(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		wantSeg    string
		wantModel  bool
		wantStyle  string
		inputStyle string
	}{
		{"core (default segment)", "stable-image-core", "core", false, "", ""},
		{"ultra variant", "stable-image-ultra", "ultra", false, "", ""},
		{"sd3 variant includes model", "sd3.5-large", "sd3", true, "", ""},
		{"sd3 with style preset", "sd3-medium", "sd3", true, "cinematic", "cinematic"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotPath string
			var gotBody string
			var gotAuth, gotCT, gotAccept string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotCT = r.Header.Get("Content-Type")
				gotAccept = r.Header.Get("Accept")
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				_, _ = io.WriteString(w, `{"image":"`+base64.StdEncoding.EncodeToString(pngMagic(16))+`"}`)
			}))
			defer srv.Close()
			h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
			cfg := image.Config{BaseURL: srv.URL + "/v2beta/stable-image/generate", AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatStability}
			_, _, _, err := h.synthStability(context.Background(), cfg, Request{
				ProviderID:   "stability-ai",
				Model:        c.model,
				Prompt:       "cat",
				Size:         "1024x1024",
				Style:        c.inputStyle,
				OutputFormat: "png",
				Credentials:  creds("st_tok"),
			})
			if err != nil {
				t.Fatalf("synthStability: %v", err)
			}
			if gotPath != "/v2beta/stable-image/generate/"+c.wantSeg {
				t.Errorf("path = %q, want segment %q", gotPath, c.wantSeg)
			}
			if gotAuth != "Bearer st_tok" {
				t.Errorf("auth = %q", gotAuth)
			}
			if gotCT != "application/json" || gotAccept != "application/json" {
				t.Errorf("CT=%q Accept=%q", gotCT, gotAccept)
			}
			var p map[string]any
			if err := json.Unmarshal([]byte(gotBody), &p); err != nil {
				t.Fatal(err)
			}
			if p["prompt"] != "cat" {
				t.Errorf("prompt = %v", p["prompt"])
			}
			if p["output_format"] != "png" {
				t.Errorf("output_format = %v, want png", p["output_format"])
			}
			if p["aspect_ratio"] != "1:1" {
				t.Errorf("aspect_ratio = %v, want 1:1", p["aspect_ratio"])
			}
			if c.wantModel {
				if p["model"] != c.model {
					t.Errorf("model = %v, want %q (sd3 includes model)", p["model"], c.model)
				}
			} else {
				if _, ok := p["model"]; ok {
					t.Errorf("model should be absent for %q, got %v", c.model, p["model"])
				}
			}
			if c.wantStyle != "" {
				if p["style_preset"] != c.wantStyle {
					t.Errorf("style_preset = %v, want %q", p["style_preset"], c.wantStyle)
				}
			} else {
				if _, ok := p["style_preset"]; ok {
					t.Errorf("style_preset should be absent when no style supplied")
				}
			}
		})
	}
}

func TestStability_AspectRatioMap(t *testing.T) {
	cases := []struct {
		size string
		want string
	}{
		{"1024x1024", "1:1"},
		{"1024x1792", "9:16"},
		{"1792x1024", "16:9"},
		{"1024x1536", "2:3"},
		{"1536x1024", "3:2"},
		{"unknown", "1:1"},
		{"", "1:1"},
	}
	for _, c := range cases {
		if got := sizeToAspectRatio(c.size); got != c.want {
			t.Errorf("sizeToAspectRatio(%q) = %q, want %q", c.size, got, c.want)
		}
	}
}

func TestStability_Normalization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"image":"`+base64.StdEncoding.EncodeToString(pngMagic(16))+`"}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := image.Config{BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatStability}
	body, ct, status, err := h.synthStability(context.Background(), cfg, Request{ProviderID: "stability-ai", Model: "core", Prompt: "p", Credentials: creds("k")})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || ct != "application/json" {
		t.Errorf("status=%d ct=%q", status, ct)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len = %d, want 1", len(data))
	}
	if _, ok := data[0].(map[string]any)["b64_json"].(string); !ok {
		t.Errorf("b64_json missing: %v", data[0])
	}
}

func TestStability_EmptyImageGivesEmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Response without `image` field → empty data array, not 502.
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := image.Config{BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatStability}
	body, _, status, err := h.synthStability(context.Background(), cfg, Request{ProviderID: "stability-ai", Model: "core", Prompt: "p", Credentials: creds("k")})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	data, _ := out["data"].([]any)
	if len(data) != 0 {
		t.Errorf("data len = %d, want 0 for missing image", len(data))
	}
}

func TestStability_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"bad request"}`)
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := image.Config{BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatStability}
	_, _, status, err := h.synthStability(context.Background(), cfg, Request{ProviderID: "stability-ai", Model: "core", Prompt: "p", Credentials: creds("k")})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if err == nil {
		t.Error("want error for upstream 400")
	}
}

// === Codex version header ===

func TestCodex_VersionHeader(t *testing.T) {
	var gotVersion, gotOriginator, gotUA, gotSession, gotReqID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("version")
		gotOriginator = r.Header.Get("originator")
		gotUA = r.Header.Get("user-agent")
		gotSession = r.Header.Get("session_id")
		gotReqID = r.Header.Get("x-client-request-id")
		// Minimal SSE with no image — synthCodex will return 502, but the
		// headers were already observed.
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	h := New(Dependencies{Executor: &fallbackExecutor{client: srv.Client()}, Logger: captureLogger{}, Config: config.Config{}})
	cfg := image.Config{BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearerAccount, Format: image.FormatCodex}
	_, _, _, _ = h.synthCodex(context.Background(), cfg, Request{ProviderID: "codex", Model: "gpt-5-image", Prompt: "p", Credentials: creds("k")})
	if gotVersion != "0.136.0" {
		t.Errorf("version = %q, want 0.136.0", gotVersion)
	}
	if gotOriginator != "codex_cli_rs" {
		t.Errorf("originator = %q, want codex_cli_rs", gotOriginator)
	}
	if gotUA != "codex_cli_rs/0.136.0" {
		t.Errorf("user-agent = %q, want codex_cli_rs/0.136.0", gotUA)
	}
	if gotSession == "" {
		t.Error("session_id empty")
	}
	if gotReqID == "" {
		t.Error("x-client-request-id empty")
	}
}
