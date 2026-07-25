package imageproxy

// cloudflare_test.go covers the Cloudflare AI image contract (step 6). All
// scenarios use a real httptest.Server; no mock HTTP client. The tests assert:
//
//   - account ID validation (32-hex / UUID / invalid / absent → 400 pre-executor)
//   - model segment validation (query/fragment/traversal → 400)
//   - multipart boundary/body for each of the 3 canonical FLUX.2 IDs (prompt,
//     dims as strings, optional fields as strings; NO image/mask keys)
//   - JSON img2img keys (image → image_b64 + image)
//   - JSON inpainting keys (mask → mask_b64 + mask + mask_image)
//   - size/width/height precedence (size regex → width/height, then finite override)
//   - seed:0 retention (numeric 0 retained for supported optional fields)
//   - mask alias canonicalization (mask_image/maskImage/mask all produce canonical
//     mask_b64+mask+mask_image)
//   - adjacent JSON model with optional fields
//   - image/mask rejection on multipart models → 400 pre-executor
//   - safe data URL input (resolveInputImage OK)
//   - URL input SSRF rejection (private/loopback URL → 400 pre-executor)
//   - raw image response (image/* → b64_json via sniff) and raw non-image (HTML
//     → 502, not base64-encoded)
//   - absence of image/mask keys in multipart body

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/image"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// cfCreds builds credentials carrying a Cloudflare account id + API key.
func cfCreds(accountID string) domainProv.Credentials {
	return domainProv.Credentials{
		APIKey: "cf-tok",
		ProviderSpecificData: map[string]any{
			"accountId":     accountID,
			"_connectionId": "c1",
		},
	}
}

// cfHexID is a valid 32-hex Cloudflare account identifier.
const cfHexID = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"

// cfUUID is a valid UUID-shape Cloudflare account identifier.
const cfUUID = "12345678-1234-1234-1234-123456789012"

// cfHandler builds an imageproxy Handler pointed at srv with a permissive SSRF
// policy (so httptest loopback endpoints pass) and a resolver for srv's host.
func cfHandler(t *testing.T, srv *httptest.Server, resolver HostResolver) *Handler {
	t.Helper()
	deps := Dependencies{
		Logger:     captureLogger{},
		SSRFPolicy: permissiveSSRFForTest{},
	}
	if srv != nil {
		deps.Executor = &fallbackExecutor{client: srv.Client()}
	}
	if resolver != nil {
		deps.Resolver = resolver
	}
	return New(deps)
}

// cfCfg returns a Cloudflare Format config pointed at base.
func cfCfg(base string) image.Config {
	return image.Config{
		BaseURL:    base,
		AuthType:   image.AuthTypeAPIKey,
		AuthHeader: image.AuthBearer,
		Format:     image.FormatCloudflareAI,
	}
}

// rawJSON marshals v to a json.RawMessage (for RequestOptions fields).
func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// parseMultipart parses a multipart/form-data body and returns the fields map
// plus the Content-Type (so the test can assert the boundary shape).
func parseMultipart(t *testing.T, ct string, body []byte) (map[string]string, string) {
	t.Helper()
	// Extract the boundary from Content-Type: multipart/form-data; boundary=...
	boundary := ""
	for _, part := range strings.Split(ct, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "boundary=") {
			boundary = strings.TrimPrefix(part, "boundary=")
			boundary = strings.Trim(boundary, "\"")
		}
	}
	if boundary == "" {
		t.Fatalf("no boundary in Content-Type %q", ct)
	}
	r := multipart.NewReader(bytes.NewReader(body), boundary)
	form, err := r.ReadForm(32 << 20)
	if err != nil {
		t.Fatalf("ReadForm: %v", err)
	}
	out := map[string]string{}
	for k, vs := range form.Value {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out, boundary
}

// === Account ID + model validation ===

func TestCloudflare_InvalidAccountID_PreExecutor(t *testing.T) {
	cases := []struct {
		name    string
		creds   domainProv.Credentials
		model   string
		wantSub string
	}{
		{"missing accountId", domainProv.Credentials{APIKey: "k", ProviderSpecificData: map[string]any{}}, "@cf/m/m", "missing accountId"},
		{"empty accountId", cfCreds(""), "@cf/m/m", "missing accountId"},
		{"non-hex short", cfCreds("abc123"), "@cf/m/m", "32-hex"},
		{"uuid malformed", cfCreds("not-a-uuid"), "@cf/m/m", "32-hex"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("upstream should not be called for invalid account id")
				_, _ = io.WriteString(w, "{}")
			}))
			defer srv.Close()
			h := cfHandler(t, srv, nil)
			_, _, status, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), Request{
				ProviderID: "cloudflare-ai", Model: c.model, Prompt: "p", Credentials: c.creds,
			})
			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", status)
			}
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("err = %v, want substring %q", err, c.wantSub)
			}
		})
	}
}

func TestCloudflare_ValidAccountID_HexAndUUID(t *testing.T) {
	for _, id := range []string{cfHexID, cfUUID} {
		t.Run(id, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				_, _ = io.WriteString(w, `{"result":{"image":"data:image/png;base64,iVBORw0KGgo="}}`)
			}))
			defer srv.Close()
			h := cfHandler(t, srv, nil)
			_, _, status, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), Request{
				ProviderID: "cloudflare-ai", Model: "@cf/some/text-model", Prompt: "p", Credentials: cfCreds(id),
			})
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if status != http.StatusOK {
				t.Errorf("status = %d", status)
			}
			if !strings.Contains(gotPath, "/"+id+"/ai/run/") {
				t.Errorf("path = %q, want account id embedded", gotPath)
			}
		})
	}
}

func TestCloudflare_InvalidModel_PreExecutor(t *testing.T) {
	cases := []struct {
		name    string
		model   string
		wantSub string
	}{
		{"empty", "", "missing model"},
		{"query", "@cf/m?x=1", "query"},
		{"fragment", "@cf/m#frag", "fragment"},
		{"traversal", "@cf/../m", "traversal"},
		{"dot segment", "@cf/./m", "traversal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("upstream should not be called for invalid model")
				_, _ = io.WriteString(w, "{}")
			}))
			defer srv.Close()
			h := cfHandler(t, srv, nil)
			_, _, status, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), Request{
				ProviderID: "cloudflare-ai", Model: c.model, Prompt: "p", Credentials: cfCreds(cfHexID),
			})
			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", status)
			}
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("err = %v, want substring %q", err, c.wantSub)
			}
		})
	}
}

// === Multipart contract (3 canonical IDs) ===

func TestCloudflare_Multipart_BodyForThreeCanonicalIDs(t *testing.T) {
	for _, model := range CloudflareMultipartModels {
		t.Run(model, func(t *testing.T) {
			var gotCT string
			var gotBody []byte
			var gotPath string
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotCT = r.Header.Get("Content-Type")
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotBody, _ = io.ReadAll(r.Body)
				_, _ = io.WriteString(w, `{"result":{"image":"data:image/png;base64,iVBORw0KGgo="}}`)
			}))
			defer srv.Close()
			h := cfHandler(t, srv, nil)
			req := Request{
				ProviderID: "cloudflare-ai", Model: model, Prompt: "a cat", Size: "512x768",
				Credentials: cfCreds(cfHexID),
				Options: RequestOptions{
					Width:          rawJSON(0), // finite 0 — NOT retained for dims (it's a dimension, present)
					Height:         rawJSON(512),
					NegativePrompt: rawJSON("ugly"),
					Guidance:       rawJSON(3.5),
					Seed:           rawJSON(0), // numeric 0 retained
					NumSteps:       rawJSON(20),
					Strength:       rawJSON(0.8),
					Steps:          nil, // absent — skipped
				},
			}
			_, _, status, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if status != http.StatusOK {
				t.Errorf("status = %d", status)
			}
			// Path embeds account id + escaped model.
			if !strings.HasSuffix(gotPath, "/ai/run/"+model) {
				t.Errorf("path = %q, want suffix /ai/run/%s", gotPath, model)
			}
			// Authorization: Bearer.
			if gotAuth != "Bearer cf-tok" {
				t.Errorf("auth = %q", gotAuth)
			}
			// Content-Type is multipart/form-data with a boundary.
			if !strings.HasPrefix(gotCT, "multipart/form-data; boundary=") {
				t.Errorf("Content-Type = %q, want multipart/form-data", gotCT)
			}
			fields, _ := parseMultipart(t, gotCT, gotBody)
			// prompt + dims as strings + optional fields as strings.
			if fields["prompt"] != "a cat" {
				t.Errorf("prompt = %q", fields["prompt"])
			}
			// Width was finite 0 — present, so width="0". Height="512".
			if fields["width"] != "0" {
				t.Errorf("width = %q, want 0 (finite retained)", fields["width"])
			}
			if fields["height"] != "512" {
				t.Errorf("height = %q, want 512", fields["height"])
			}
			// Optional fields as strings.
			if fields["negative_prompt"] != "ugly" {
				t.Errorf("negative_prompt = %q", fields["negative_prompt"])
			}
			if fields["guidance"] != "3.5" {
				t.Errorf("guidance = %q, want 3.5", fields["guidance"])
			}
			if fields["seed"] != "0" {
				t.Errorf("seed = %q, want 0 (numeric zero retained)", fields["seed"])
			}
			if fields["num_steps"] != "20" {
				t.Errorf("num_steps = %q", fields["num_steps"])
			}
			if fields["strength"] != "0.8" {
				t.Errorf("strength = %q", fields["strength"])
			}
			// steps absent — must not be present.
			if _, ok := fields["steps"]; ok {
				t.Errorf("steps = %q, want absent", fields["steps"])
			}
			// NO image/mask keys in multipart body.
			for _, key := range []string{"image", "image_b64", "mask", "mask_b64", "mask_image"} {
				if _, ok := fields[key]; ok {
					t.Errorf("multipart body must not contain %q key", key)
				}
			}
		})
	}
}

func TestCloudflare_Multipart_RejectsImageAndMask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be called when image/mask supplied on multipart model")
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()
	h := cfHandler(t, srv, nil)
	img := dataURL("image/png", pngMagic(32))
	req := Request{
		ProviderID: "cloudflare-ai", Model: CloudflareMultipartModels[0], Prompt: "p",
		Credentials: cfCreds(cfHexID),
		Options:     RequestOptions{RawImageInputs: []json.RawMessage{rawJSON(img)}},
	}
	_, _, status, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if err == nil || !strings.Contains(err.Error(), "multipart") {
		t.Errorf("err = %v", err)
	}

	// Mask rejection.
	req2 := Request{
		ProviderID: "cloudflare-ai", Model: CloudflareMultipartModels[0], Prompt: "p",
		Credentials: cfCreds(cfHexID),
		Options:     RequestOptions{RawMask: rawJSON(img)},
	}
	_, _, status2, err2 := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req2)
	if status2 != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status2)
	}
	if err2 == nil || !strings.Contains(err2.Error(), "multipart") {
		t.Errorf("err = %v", err2)
	}
}

// === JSON img2img / inpainting keys ===

func TestCloudflare_JSON_Img2Img_ExactKeys(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"result":{"image":"data:image/png;base64,iVBORw0KGgo="}}`)
	}))
	defer srv.Close()
	h := cfHandler(t, srv, nil)
	img := dataURL("image/png", pngMagic(32))
	req := Request{
		ProviderID: "cloudflare-ai", Model: CloudflareImg2ImgModel, Prompt: "cat on a roof", Size: "1024x1024",
		Credentials: cfCreds(cfHexID),
		Options: RequestOptions{
			RawImageInputs: []json.RawMessage{rawJSON(img)},
			Width:          rawJSON(768),
		},
	}
	_, _, status, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	var p map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatal(err)
	}
	// prompt present.
	if _, ok := p["prompt"]; !ok {
		t.Error("missing prompt")
	}
	// width from finite override (768), height from size regex (1024).
	var w int
	if err := json.Unmarshal(p["width"], &w); err != nil || w != 768 {
		t.Errorf("width = %s, want 768", p["width"])
	}
	var heightVal int
	if err := json.Unmarshal(p["height"], &heightVal); err != nil || heightVal != 1024 {
		t.Errorf("height = %s, want 1024", p["height"])
	}
	// image_b64 (string) + image (base64 []byte → JSON string).
	if _, ok := p["image_b64"]; !ok {
		t.Error("missing image_b64")
	}
	if _, ok := p["image"]; !ok {
		t.Error("missing image ([]byte base64)")
	}
	// image_b64 and image decode to the same bytes.
	var b64 string
	_ = json.Unmarshal(p["image_b64"], &b64)
	var imgStr string
	_ = json.Unmarshal(p["image"], &imgStr)
	if b64 != imgStr {
		t.Errorf("image_b64 (%q) != image (%q)", b64, imgStr)
	}
	dec, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || dec[0] != 0x89 || dec[1] != 'P' {
		t.Errorf("decoded image bytes mismatch: %v err=%v", dec[:2], err)
	}
	// No mask keys (img2img).
	for _, key := range []string{"mask", "mask_b64", "mask_image"} {
		if _, ok := p[key]; ok {
			t.Errorf("img2img body must not contain %q", key)
		}
	}
}

func TestCloudflare_JSON_Inpainting_ExactKeys(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"result":{"image":"https://cdn/x.png"}}`)
	}))
	defer srv.Close()
	h := cfHandler(t, srv, nil)
	img := dataURL("image/png", pngMagic(32))
	mask := dataURL("image/jpeg", jpegMagic(32))
	req := Request{
		ProviderID: "cloudflare-ai", Model: CloudflareInpaintingModel, Prompt: "fill", Size: "1024x1024",
		Credentials: cfCreds(cfHexID),
		Options: RequestOptions{
			RawImageInputs: []json.RawMessage{rawJSON(img)},
			RawMask:        rawJSON(mask),
		},
	}
	_, _, status, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	var p map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatal(err)
	}
	// image → image_b64 + image.
	if _, ok := p["image_b64"]; !ok {
		t.Error("missing image_b64")
	}
	if _, ok := p["image"]; !ok {
		t.Error("missing image")
	}
	// mask → mask_b64 + mask + mask_image (all three).
	if _, ok := p["mask_b64"]; !ok {
		t.Error("missing mask_b64")
	}
	if _, ok := p["mask"]; !ok {
		t.Error("missing mask")
	}
	if _, ok := p["mask_image"]; !ok {
		t.Error("missing mask_image")
	}
	// mask and mask_image are the same base64.
	var mStr, miStr string
	_ = json.Unmarshal(p["mask"], &mStr)
	_ = json.Unmarshal(p["mask_image"], &miStr)
	if mStr != miStr {
		t.Errorf("mask (%q) != mask_image (%q)", mStr, miStr)
	}
	// mask_b64 == mask (same bytes).
	var mb64 string
	_ = json.Unmarshal(p["mask_b64"], &mb64)
	if mb64 != mStr {
		t.Errorf("mask_b64 (%q) != mask (%q)", mb64, mStr)
	}
}

// === Mask alias canonicalization ===

func TestCloudflare_JSON_MaskAliasCanonicalization(t *testing.T) {
	// The handler canonicalises mask_image/maskImage/mask into RawMask before
	// calling the usecase; here we verify the usecase produces the same
	// mask_b64+mask+mask_image regardless of which alias was supplied, by
	// passing the same raw value through RawMask directly.
	mask := dataURL("image/png", pngMagic(32))
	for _, alias := range []string{"mask_image", "maskImage", "mask"} {
		t.Run(alias, func(t *testing.T) {
			var gotBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				_, _ = io.WriteString(w, `{"result":{"image":"https://x/a.png"}}`)
			}))
			defer srv.Close()
			h := cfHandler(t, srv, nil)
			req := Request{
				ProviderID: "cloudflare-ai", Model: CloudflareInpaintingModel, Prompt: "p",
				Credentials: cfCreds(cfHexID),
				Options:     RequestOptions{RawMask: rawJSON(mask)},
			}
			_, _, _, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			var p map[string]json.RawMessage
			_ = json.Unmarshal(gotBody, &p)
			for _, key := range []string{"mask_b64", "mask", "mask_image"} {
				if _, ok := p[key]; !ok {
					t.Errorf("alias %q: missing %s in body", alias, key)
				}
			}
		})
	}
}

// === Dimensions precedence ===

func TestCloudflare_DimensionsPrecedence(t *testing.T) {
	cases := []struct {
		name         string
		size         string
		width        json.RawMessage
		height       json.RawMessage
		wantW        int
		wantH        int
		wantPresentW bool
		wantPresentH bool
	}{
		{"size only", "1024x1024", nil, nil, 1024, 1024, true, true},
		{"width overrides size", "1024x1024", rawJSON(512), nil, 512, 1024, true, true},
		{"height overrides size", "1024x1024", nil, rawJSON(768), 1024, 768, true, true},
		{"both override size", "1024x1024", rawJSON(640), rawJSON(480), 640, 480, true, true},
		{"invalid size + finite dims", "bad", rawJSON(800), rawJSON(600), 800, 600, true, true},
		{"invalid size + no dims", "bad", nil, nil, 0, 0, false, false},
		{"no size + no dims", "", nil, nil, 0, 0, false, false},
		{"finite width 0", "1024x1024", rawJSON(0), nil, 0, 1024, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				_, _ = io.WriteString(w, `{"result":{"image":"https://x/a.png"}}`)
			}))
			defer srv.Close()
			h := cfHandler(t, srv, nil)
			req := Request{
				ProviderID: "cloudflare-ai", Model: "@cf/some/text-model", Prompt: "p", Size: c.size,
				Credentials: cfCreds(cfHexID),
				Options:     RequestOptions{Width: c.width, Height: c.height},
			}
			_, _, _, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			var p map[string]json.RawMessage
			_ = json.Unmarshal(gotBody, &p)
			w, wok := p["width"]
			hv, hok := p["height"]
			if c.wantPresentW {
				if !wok {
					t.Fatalf("width should be present")
				}
				var wi int
				_ = json.Unmarshal(w, &wi)
				if wi != c.wantW {
					t.Errorf("width = %d, want %d", wi, c.wantW)
				}
			} else if wok {
				t.Errorf("width should be absent, got %s", w)
			}
			if c.wantPresentH {
				if !hok {
					t.Fatalf("height should be present")
				}
				var hi int
				_ = json.Unmarshal(hv, &hi)
				if hi != c.wantH {
					t.Errorf("height = %d, want %d", hi, c.wantH)
				}
			} else if hok {
				t.Errorf("height should be absent, got %s", hv)
			}
		})
	}
}

// === seed:0 retention ===

func TestCloudflare_SeedZeroRetained(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"result":{"image":"https://x/a.png"}}`)
	}))
	defer srv.Close()
	h := cfHandler(t, srv, nil)
	req := Request{
		ProviderID: "cloudflare-ai", Model: "@cf/some/text-model", Prompt: "p",
		Credentials: cfCreds(cfHexID),
		Options: RequestOptions{
			Seed:     rawJSON(0),
			Guidance: rawJSON(0),
		},
	}
	_, _, _, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var p map[string]json.RawMessage
	_ = json.Unmarshal(gotBody, &p)
	// seed:0 retained.
	if s, ok := p["seed"]; !ok {
		t.Error("seed absent; numeric 0 must be retained")
	} else {
		var n int
		_ = json.Unmarshal(s, &n)
		if n != 0 {
			t.Errorf("seed = %d, want 0", n)
		}
	}
	// guidance:0 retained.
	if s, ok := p["guidance"]; !ok {
		t.Error("guidance absent; numeric 0 must be retained")
	} else {
		var n int
		_ = json.Unmarshal(s, &n)
		if n != 0 {
			t.Errorf("guidance = %d, want 0", n)
		}
	}
}

// null/empty optional fields skipped
func TestCloudflare_NullEmptyOptionalSkipped(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"result":{"image":"https://x/a.png"}}`)
	}))
	defer srv.Close()
	h := cfHandler(t, srv, nil)
	req := Request{
		ProviderID: "cloudflare-ai", Model: "@cf/some/text-model", Prompt: "p",
		Credentials: cfCreds(cfHexID),
		Options: RequestOptions{
			NegativePrompt: rawJSON(nil), // null — skipped
			Guidance:       rawJSON(""),  // empty string — skipped
			Seed:           rawJSON(7),   // retained
		},
	}
	_, _, _, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var p map[string]json.RawMessage
	_ = json.Unmarshal(gotBody, &p)
	if _, ok := p["negative_prompt"]; ok {
		t.Error("null negative_prompt must be skipped")
	}
	if _, ok := p["guidance"]; ok {
		t.Error("empty-string guidance must be skipped")
	}
	if _, ok := p["seed"]; !ok {
		t.Error("seed=7 must be retained")
	}
}

// === Adjacent JSON model with optional fields ===

func TestCloudflare_JSON_AdjacentModelWithOptionalFields(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"created":1700000000,"data":[{"b64_json":"iVBORw0KGgo="}]}`)
	}))
	defer srv.Close()
	h := cfHandler(t, srv, nil)
	req := Request{
		ProviderID: "cloudflare-ai", Model: "@cf/un/u/v", Prompt: "p", Size: "512x512",
		Credentials: cfCreds(cfHexID),
		Options: RequestOptions{
			NegativePrompt: rawJSON("blurry"),
			NumSteps:       rawJSON(30),
			Steps:          rawJSON(50),
			Strength:       rawJSON(0.9),
		},
	}
	body, _, status, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	var p map[string]json.RawMessage
	_ = json.Unmarshal(gotBody, &p)
	for _, k := range []string{"prompt", "width", "height", "negative_prompt", "num_steps", "steps", "strength"} {
		if _, ok := p[k]; !ok {
			t.Errorf("missing %q in JSON body", k)
		}
	}
	// No image/mask keys (text model).
	for _, k := range []string{"image", "image_b64", "mask", "mask_b64", "mask_image"} {
		if _, ok := p[k]; ok {
			t.Errorf("text model body must not contain %q", k)
		}
	}
	// Response passthrough (already OpenAI shape).
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Errorf("data len = %d, want 1", len(data))
	}
}

// === Safe data URL input ===

func TestCloudflare_JSON_SafeDataURLInput(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"result":{"image":"https://x/a.png"}}`)
	}))
	defer srv.Close()
	h := cfHandler(t, srv, nil)
	img := dataURL("image/png", pngMagic(32))
	req := Request{
		ProviderID: "cloudflare-ai", Model: CloudflareImg2ImgModel, Prompt: "p",
		Credentials: cfCreds(cfHexID),
		Options:     RequestOptions{RawImageInputs: []json.RawMessage{rawJSON(img)}},
	}
	_, _, status, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	var p map[string]json.RawMessage
	_ = json.Unmarshal(gotBody, &p)
	if _, ok := p["image_b64"]; !ok {
		t.Error("missing image_b64 for safe data URL")
	}
}

// === URL input SSRF rejection (pre-executor) ===

func TestCloudflare_JSON_URLInputSSRFRejection(t *testing.T) {
	// Production SSRF policy: a private/loopback URL is rejected before any
	// upstream call (neither the image fetch nor the submit reaches the wire).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called when image URL is SSRF-rejected")
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()
	h := New(Dependencies{
		Logger:     captureLogger{},
		Executor:   &fallbackExecutor{client: srv.Client()},
		SSRFPolicy: defaultSSRFPolicy{},
		Resolver: ResolverFunc(func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("10.0.0.1")}, nil
		}),
	})
	req := Request{
		ProviderID: "cloudflare-ai", Model: CloudflareImg2ImgModel, Prompt: "p",
		Credentials: cfCreds(cfHexID),
		Options:     RequestOptions{RawImageInputs: []json.RawMessage{rawJSON("https://internal.example/a.png")}},
	}
	_, _, status, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("err = %v, want forbidden substring", err)
	}
}

// === Raw image response (image/* → b64_json via sniff) ===

func TestCloudflare_RawImageResponse_SniffedB64(t *testing.T) {
	img := pngMagic(64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(img)
	}))
	defer srv.Close()
	h := cfHandler(t, srv, nil)
	req := Request{
		ProviderID: "cloudflare-ai", Model: "@cf/some/text-model", Prompt: "p",
		Credentials: cfCreds(cfHexID),
	}
	body, ct, status, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK || ct != "application/json" {
		t.Errorf("status=%d ct=%q", status, ct)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len = %d", len(data))
	}
	item, _ := data[0].(map[string]any)
	b64, _ := item["b64_json"].(string)
	if b64 == "" {
		t.Fatal("missing b64_json in normalised raw-image response")
	}
	dec, _ := base64.StdEncoding.DecodeString(b64)
	if dec[0] != 0x89 || dec[1] != 'P' {
		t.Errorf("decoded bytes mismatch: %v", dec[:4])
	}
}

// === Raw non-image response (HTML → 502, not base64-encoded) ===

func TestCloudflare_RawNonImageResponse_502NotBase64(t *testing.T) {
	html := []byte("<html><body>error page</body></html>")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(html)
	}))
	defer srv.Close()
	h := cfHandler(t, srv, nil)
	req := Request{
		ProviderID: "cloudflare-ai", Model: "@cf/some/text-model", Prompt: "p",
		Credentials: cfCreds(cfHexID),
	}
	body, _, status, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
	if err == nil {
		t.Fatal("want error for non-image raw response")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
	if body != nil {
		// The raw HTML must NOT be returned as the body (no base64-encoding).
		if bytes.Contains(body, html) || bytes.Contains(body, []byte("PGh0bWw")) {
			t.Errorf("non-image raw response was leaked/base64-encoded into body: %q", body)
		}
	}
}

// === Upstream error pass-through ===

func TestCloudflare_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errors":[{"message":"bad token"}]}`)
	}))
	defer srv.Close()
	h := cfHandler(t, srv, nil)
	req := Request{
		ProviderID: "cloudflare-ai", Model: "@cf/some/text-model", Prompt: "p",
		Credentials: cfCreds(cfHexID),
	}
	_, _, status, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
	if err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Errorf("err = %v", err)
	}
}

// === Queued responses shape ===

func TestCloudflare_QueuedResponsesShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"result":{"responses":[{"success":false,"result":{}},{"success":true,"result":{"image":"data:image/png;base64,iVBORw0KGgo="}}]}}`)
	}))
	defer srv.Close()
	h := cfHandler(t, srv, nil)
	req := Request{
		ProviderID: "cloudflare-ai", Model: "@cf/some/text-model", Prompt: "p",
		Credentials: cfCreds(cfHexID),
	}
	body, _, status, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len = %d, want 1 (first success result)", len(data))
	}
	item, _ := data[0].(map[string]any)
	if _, ok := item["b64_json"]; !ok {
		t.Errorf("queued result item = %v, want b64_json", item)
	}
}

// === result.data[0].url normalization ===

func TestCloudflare_ResultDataZeroURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"result":{"data":[{"url":"https://cdn/x.png"}]}}`)
	}))
	defer srv.Close()
	h := cfHandler(t, srv, nil)
	req := Request{
		ProviderID: "cloudflare-ai", Model: "@cf/some/text-model", Prompt: "p",
		Credentials: cfCreds(cfHexID),
	}
	body, _, _, err := h.synthCloudflareAI(context.Background(), cfCfg(srv.URL), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len = %d", len(data))
	}
	item, _ := data[0].(map[string]any)
	if item["url"] != "https://cdn/x.png" {
		t.Errorf("item = %v, want url", item)
	}
}

// === Validate helper unit tests ===

func TestValidateCloudflareModel_PathEscaping(t *testing.T) {
	// The "@" and "/" in Cloudflare model IDs are preserved after escaping.
	got, err := validateCloudflareModel("@cf/black-forest-labs/flux-2-dev")
	if err != nil {
		t.Fatal(err)
	}
	want := "@cf/black-forest-labs/flux-2-dev"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCloudflareAccountID_Validation(t *testing.T) {
	cases := []struct {
		id      string
		wantErr bool
	}{
		{cfHexID, false},
		{cfUUID, false},
		{"", true},
		{"abc", true},
		{"g1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6", true},    // non-hex char 'g'
		{"12345678-1234-1234-1234-12345678901", true}, // wrong uuid length
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			_, err := cloudflareAccountID(domainProv.Credentials{ProviderSpecificData: map[string]any{"accountId": c.id}})
			if c.wantErr && err == nil {
				t.Error("want error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Errorf("want nil, got %v", err)
			}
		})
	}
}

func TestCloudflare_NSuppliedHandle(t *testing.T) {
	// Exercise the full Handle path through the registry (cloudflare-ai no
	// longer 501).
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"result":{"image":"https://x/a.png"}}`)
	}))
	defer srv.Close()
	h := cfHandler(t, srv, nil)
	setImageBaseURL(t, "cloudflare-ai", cfCfg(srv.URL))
	res := h.Handle(context.Background(), Request{
		ProviderID: "cloudflare-ai", Model: "@cf/some/text-model", Prompt: "p",
		Credentials: cfCreds(cfHexID),
	})
	if res.Err != nil {
		t.Fatalf("Handle: %v", res.Err)
	}
	if !strings.Contains(gotPath, "/ai/run/") {
		t.Errorf("path = %q", gotPath)
	}
	_ = config.Config{} // keep config import used
}
