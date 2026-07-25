package http

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	imageprov "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/image"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/transport/http/api"
)

// imageLookupForTest delegates to the image provider registry for unit tests
// that assert the static BaseURL of local no-auth providers.
func imageLookupForTest(providerID string) (imageprov.Config, bool) {
	return imageprov.Lookup(providerID)
}

// stubImageHandler records the last request and returns a canned result.
type stubImageHandler struct {
	lastReq ImageRequest
	body    []byte
	ct      string
	status  int
	err     error
}

func (s *stubImageHandler) Handle(ctx context.Context, req ImageRequest) (ImageResult, error) {
	s.lastReq = req
	if s.err != nil {
		return ImageResult{StatusCode: s.status, Err: s.err}, s.err
	}
	st := s.status
	if st == 0 {
		st = http.StatusOK
	}
	return ImageResult{StatusCode: st, Body: s.body, ContentType: s.ct}, nil
}

var _ ImageHandler = (*stubImageHandler)(nil)

func newImageMux(t *testing.T, stub ImageHandler) (*http.ServeMux, *sql.DB) {
	t.Helper()
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	deps := V1Deps{
		APIKeysRepo:    repo.NewAPIKeyRepo(db),
		SettingsRepo:   repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db),
		ComboRepo:      repo.NewComboRepo(db),
		AliasRepo:      repo.NewAliasRepo(db),
		NodeRepo:       repo.NewNodeRepo(db),
		ProxyPoolRepo:  repo.NewProxyPoolRepo(db),
		Config:         config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:         slogDiscard(),
		Image:          stub,
	}
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	return mux, db
}

func imageReq(t *testing.T, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestV1Images_HappyPath_ProviderPrefixStripped(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[{"url":"https://x/a.png"}]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k-openai"}`)

	req := imageReq(t, `{"model":"openai/dall-e-3","prompt":"cat","n":1,"size":"1024x1024"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "openai" {
		t.Errorf("ProviderID = %q, want openai", stub.lastReq.ProviderID)
	}
	if stub.lastReq.Model != "dall-e-3" {
		t.Errorf("Model = %q, want dall-e-3 (prefix stripped)", stub.lastReq.Model)
	}
	if stub.lastReq.Prompt != "cat" {
		t.Errorf("Prompt = %q", stub.lastReq.Prompt)
	}
	if stub.lastReq.N != 1 {
		t.Errorf("N = %d, want 1", stub.lastReq.N)
	}
}

func TestV1Images_ConnectionIdHeader(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)

	req := imageReq(t, `{"model":"dall-e-3","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("x-9gouter-connection-id") == "" {
		t.Error("expected x-9gouter-connection-id header for image gen")
	}
}

func TestV1Images_BareModelDefaultsToOpenAI(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)

	req := imageReq(t, `{"model":"dall-e-3","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if stub.lastReq.ProviderID != "openai" {
		t.Errorf("ProviderID = %q, want openai (bare fallback)", stub.lastReq.ProviderID)
	}
}

func TestV1Images_GeminiProvider(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[{"b64_json":"x"}]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "gemini", `{"apiKey":"k-gem"}`)

	req := imageReq(t, `{"model":"gemini/gemini-2.5-flash-image","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if stub.lastReq.ProviderID != "gemini" {
		t.Errorf("ProviderID = %q, want gemini", stub.lastReq.ProviderID)
	}
}

func TestV1Images_ResponseFormatQueryFallback(t *testing.T) {
	stub := &stubImageHandler{body: []byte("RAW"), ct: "image/png"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations?response_format=binary", strings.NewReader(`{"model":"dall-e-3","prompt":"cat","output_format":"png"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if stub.lastReq.ResponseFormat != "binary" {
		t.Errorf("ResponseFormat = %q, want binary (query fallback)", stub.lastReq.ResponseFormat)
	}
}

func TestV1Images_MissingModel(t *testing.T) {
	stub := &stubImageHandler{}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	req := imageReq(t, `{"prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing model)", rec.Code)
	}
}

func TestV1Images_MissingPrompt(t *testing.T) {
	stub := &stubImageHandler{}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	req := imageReq(t, `{"model":"dall-e-3"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing prompt)", rec.Code)
	}
}

func TestV1Images_InvalidJSON(t *testing.T) {
	stub := &stubImageHandler{}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	req := imageReq(t, `{not json`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (invalid json)", rec.Code)
	}
}

func TestV1Images_NoCredentials(t *testing.T) {
	stub := &stubImageHandler{}
	mux, _ := newImageMux(t, stub)
	// No connection created for openai.
	req := imageReq(t, `{"model":"dall-e-3","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no credentials)", rec.Code)
	}
}

func TestV1Images_NotWired(t *testing.T) {
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	deps := V1Deps{
		APIKeysRepo: repo.NewAPIKeyRepo(db), SettingsRepo: repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db), ComboRepo: repo.NewComboRepo(db),
		AliasRepo: repo.NewAliasRepo(db), NodeRepo: repo.NewNodeRepo(db),
		ProxyPoolRepo: repo.NewProxyPoolRepo(db),
		Config:        config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:        slogDiscard(),
	}
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	req := imageReq(t, `{"model":"dall-e-3","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 (not wired)", rec.Code)
	}
}

func TestV1Images_UpstreamError(t *testing.T) {
	stub := &stubImageHandler{status: http.StatusUnauthorized, err: errUpstreamBadKey}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	req := imageReq(t, `{"model":"dall-e-3","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestV1Images_DashboardPassthrough(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)
	api.RegisterV1Dashboard(mux, api.Deps{V1Dispatch: mux.ServeHTTP})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/images/generations", strings.NewReader(`{"model":"dall-e-3","prompt":"cat"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (passthrough); body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "openai" {
		t.Errorf("passthrough did not reach /v1/images/generations")
	}
}

// remoteImageReq builds an image-generation request that looks like it came
// from a non-loopback viewer (used for the sdwebui/comfyui local-guard tests).
func remoteImageReq(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	req.RemoteAddr = "203.0.113.7:55555"
	req.Header.Set("Content-Type", "application/json")
	return req
}

// imageReqWithConn builds a loopback image request with a preferred connection
// pin header.
func imageReqWithConn(t *testing.T, body, connID string) *http.Request {
	t.Helper()
	req := imageReq(t, body)
	if connID != "" {
		req.Header.Set("x-9gouter-connection-id", connID)
	}
	return req
}

// === Step 3: capability table, presence semantics, local guard ===

func TestImageCapabilities_Table(t *testing.T) {
	cases := []struct {
		name                                          string
		provider, model                               string
		allowImage, allowMask, allowDims, allowNamed6 bool
		ok                                            bool
	}{
		{"fal-ai any model", "fal-ai", "fal-ai/flux/schnell", true, false, false, false, true},
		{"bfl kontext pro", "black-forest-labs", "flux-kontext-pro", true, false, false, false, true},
		{"bfl kontext max", "black-forest-labs", "flux-kontext-max", true, false, false, false, true},
		{"bfl other", "black-forest-labs", "flux-pro-1.1", false, false, false, false, true},
		{"cf inpainting", "cloudflare-ai", cloudflareInpaintingModel, true, true, true, true, true},
		{"cf img2img", "cloudflare-ai", cloudflareImg2ImgModel, true, false, true, true, true},
		{"cf multipart", "cloudflare-ai", cloudflareMultipartModels[0], false, false, true, true, true},
		{"cf other json", "cloudflare-ai", "@cf/some/other-model", false, false, true, true, true},
		{"runwayml", "runwayml", "gen4_image", false, false, false, false, true},
		{"nanobanana", "nanobanana", "nano-1", false, false, false, false, true},
		{"sdwebui", "sdwebui", "", false, false, false, false, true},
		{"comfyui", "comfyui", "", false, false, false, false, true},
		{"openai (not in matrix)", "openai", "dall-e-3", false, false, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cap, ok := imageCapabilities(c.provider, c.model)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if cap.allowImage != c.allowImage || cap.allowMask != c.allowMask ||
				cap.allowDims != c.allowDims || cap.allowNamed6 != c.allowNamed6 {
				t.Fatalf("row = %+v, want image=%v mask=%v dims=%v named6=%v",
					cap, c.allowImage, c.allowMask, c.allowDims, c.allowNamed6)
			}
		})
	}
}

func TestCheckImageCapabilities_UnsupportedFieldRejected(t *testing.T) {
	// openai has no matrix row → default-deny every extended field, even null/0.
	cap, _ := imageCapabilities("openai", "dall-e-3")
	cases := []struct {
		field string
		body  string
	}{
		{"image", `{"image":null}`},
		{"image", `{"image":""}`},
		{"image", `{"image":0}`},
		{"images", `{"images":[]}`},
		{"mask", `{"mask":null}`},
		{"width", `{"width":0}`},
		{"height", `{"height":null}`},
		{"negative_prompt", `{"negative_prompt":""}`},
		{"guidance", `{"guidance":0}`},
		{"seed", `{"seed":0}`},
		{"num_steps", `{"num_steps":0}`},
		{"steps", `{"steps":0}`},
		{"strength", `{"strength":0}`},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			var b imagesRequestBody
			if err := json.Unmarshal([]byte(`{"model":"dall-e-3","prompt":"cat",`+c.body[1:]), &b); err != nil {
				t.Fatal(err)
			}
			supplied := suppliedImageFields{
				image: b.Image, images: b.Images, width: b.Width, height: b.Height,
				negativePrompt: b.NegativePrompt, guidance: b.Guidance, seed: b.Seed,
				numSteps: b.NumSteps, steps: b.Steps, strength: b.Strength,
				maskImage: b.MaskImage, maskCamel: b.MaskCamel, mask: b.Mask,
			}
			_, maskSupplied, _ := canonicalizeMask(supplied)
			cerr := checkImageCapabilities("openai", "dall-e-3", cap, supplied, maskSupplied)
			if cerr == nil {
				t.Fatalf("expected 400 for unsupported %s, got nil", c.field)
			}
			if !strings.Contains(cerr.Error(), "openai does not support") {
				t.Fatalf("err = %q, want '<provider> does not support <field>'", cerr.Error())
			}
		})
	}
}

func TestCheckImageCapabilities_SupportedCloudflareSeedZeroRetained(t *testing.T) {
	// Cloudflare JSON models accept seed; numeric 0 must be retained (permitted),
	// not rejected. The capability check should pass for seed:0.
	cap, _ := imageCapabilities("cloudflare-ai", cloudflareImg2ImgModel)
	var b imagesRequestBody
	if err := json.Unmarshal([]byte(`{"seed":0}`), &b); err != nil {
		t.Fatal(err)
	}
	supplied := suppliedImageFields{seed: b.Seed}
	if cerr := checkImageCapabilities("cloudflare-ai", cloudflareImg2ImgModel, cap, supplied, false); cerr != nil {
		t.Fatalf("seed:0 on supported cloudflare model should be retained, got %v", cerr)
	}
	// Confirm the raw value is preserved as 0 (not dropped).
	if string(b.Seed) != "0" {
		t.Fatalf("seed raw = %q, want %q", string(b.Seed), "0")
	}
}

func TestCheckImageCapabilities_UnsupportedSeedZeroRejected(t *testing.T) {
	// openai has no named6 row → seed:0 is supplied and must be rejected even
	// though it is numeric zero.
	cap, _ := imageCapabilities("openai", "dall-e-3")
	var b imagesRequestBody
	if err := json.Unmarshal([]byte(`{"seed":0}`), &b); err != nil {
		t.Fatal(err)
	}
	supplied := suppliedImageFields{seed: b.Seed}
	cerr := checkImageCapabilities("openai", "dall-e-3", cap, supplied, false)
	if cerr == nil {
		t.Fatal("expected 400 for unsupported seed:0, got nil")
	}
}

func TestCanonicalizeMask_Aliases(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
		wantSet bool
	}{
		{"none", `{"model":"x"}`, false, false},
		{"mask_image only", `{"mask_image":"a"}`, false, true},
		{"maskImage only", `{"maskImage":"a"}`, false, true},
		{"mask only", `{"mask":"a"}`, false, true},
		{"mask_image null counts", `{"mask_image":null}`, false, true},
		{"mask conflict two", `{"mask_image":"a","mask":"b"}`, true, false},
		{"mask conflict all three", `{"mask_image":"a","maskImage":"b","mask":"c"}`, true, false},
		{"mask conflict with null", `{"mask_image":null,"mask":null}`, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var b imagesRequestBody
			if err := json.Unmarshal([]byte(c.body), &b); err != nil {
				t.Fatal(err)
			}
			f := suppliedImageFields{maskImage: b.MaskImage, maskCamel: b.MaskCamel, mask: b.Mask}
			_, set, err := canonicalizeMask(f)
			if c.wantErr && err == nil {
				t.Fatal("expected alias-conflict error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if c.wantSet && !set {
				t.Fatal("expected mask to be supplied")
			}
			if !c.wantSet && set {
				t.Fatal("expected mask to be absent")
			}
		})
	}
}

func TestV1Images_HandlerRejectsUnsupportedFieldBeforeExecutor(t *testing.T) {
	// openai + seed:0 → 400 before the executor (stub must not be called).
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)

	req := imageReq(t, `{"model":"dall-e-3","prompt":"cat","seed":0}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unsupported seed); body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "" {
		t.Fatal("executor was called before capability check rejected the field")
	}
	if !strings.Contains(rec.Body.String(), "openai does not support seed") {
		t.Fatalf("body = %q, want capability error", rec.Body.String())
	}
}

func TestV1Images_HandlerRejectsUnsupportedImageForBFLNonKontext(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "black-forest-labs", `{"apiKey":"k"}`)

	req := imageReq(t, `{"model":"black-forest-labs/flux-pro-1.1","prompt":"cat","image":"https://x/a.png"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (image not supported for flux-pro-1.1); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "black-forest-labs does not support image") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestV1Images_HandlerAcceptsFalAIImage(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "fal-ai", `{"apiKey":"k"}`)

	req := imageReq(t, `{"model":"fal-ai/flux/schnell","prompt":"cat","image":"https://x/a.png"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "fal-ai" {
		t.Errorf("ProviderID = %q", stub.lastReq.ProviderID)
	}
	// Raw image presence forwarded into Options.
	if len(stub.lastReq.Options.RawImageInputs) != 1 {
		t.Fatalf("RawImageInputs = %v, want 1", stub.lastReq.Options.RawImageInputs)
	}
}

func TestV1Images_HandlerRejectsMaskConflict(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "cloudflare-ai", `{"apiKey":"k"}`)

	req := imageReq(t, `{"model":"cloudflare-ai/`+cloudflareInpaintingModel+`","prompt":"cat","mask_image":"a","mask":"b"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (mask conflict); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "conflicting mask aliases") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestV1Images_HandlerAcceptsSingleMask(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "cloudflare-ai", `{"apiKey":"k"}`)

	req := imageReq(t, `{"model":"cloudflare-ai/`+cloudflareInpaintingModel+`","prompt":"cat","mask":"a"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.Options.RawMask == nil {
		t.Fatal("RawMask not forwarded")
	}
}

func TestV1Images_PreferredConnectionHeaderPins(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnectionWithID(t, db, "openai-conn-a", "openai", `{"apiKey":"k-a"}`)
	mustCreateConnectionWithID(t, db, "openai-conn-b", "openai", `{"apiKey":"k-b"}`)
	const connB = "openai-conn-b"

	// Pin to connB via header; the echoed x-9gouter-connection-id must be connB.
	req := imageReqWithConn(t, `{"model":"dall-e-3","prompt":"cat"}`, connB)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("x-9gouter-connection-id"); got != connB {
		t.Fatalf("x-9gouter-connection-id = %q, want %q (pin not honoured)", got, connB)
	}
}

func TestV1Images_SdWebUIExternalViewer403(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, _ := newImageMux(t, stub)

	// External viewer (non-loopback remote addr) hits sdwebui → 403 before the
	// executor is reached, so the stub handler is never called.
	req := remoteImageReq(t, `{"model":"sdwebui/sd-1.5","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (external viewer); body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.ProviderID != "" {
		t.Fatal("executor was called despite local guard")
	}
}

func TestV1Images_ComfyUIExternalViewer403(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, _ := newImageMux(t, stub)

	req := remoteImageReq(t, `{"model":"comfyui/default","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestV1Images_SdWebUILoopbackNoCredentialRequirement(t *testing.T) {
	// sdwebui is no-auth: from a loopback viewer it must NOT 404 for missing
	// credentials and must NOT 403. The stub handler is wired (deps.Image set),
	// so the request reaches the stub and returns 200 — proving virtual
	// credentials were resolved (no 404) and the local guard let it through
	// (no 403). The real imageproxy.Handler now dispatches sdwebui by Format
	// (step 5 lifted Unsupported); the full real-handler route is exercised
	// separately by the usecase + e2e tests.
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, _ := newImageMux(t, stub)

	req := imageReq(t, `{"model":"sdwebui/sd-1.5","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	switch rec.Code {
	case http.StatusNotFound:
		t.Fatal("sdwebui 404'd for missing credentials — no-auth resolver not applied")
	case http.StatusForbidden:
		t.Fatal("sdwebui 403'd from loopback viewer — local guard too strict")
	case http.StatusOK:
		// expected: stub returned 200, virtual creds resolved, guard passed
	default:
		t.Fatalf("status = %d, want 200 (stub) — no-auth resolver/guard; body=%s", rec.Code, rec.Body.String())
	}
}

func TestV1Images_NoAuthProviderDoesNotEchoConnectionHeader(t *testing.T) {
	// sdwebui is no-auth: virtual credentials carry no _connectionId, so the
	// response must not carry x-9gouter-connection-id. The stub handler returns
	// 200 (deps.Image wired) so the request reaches the handler and the
	// writeImageResult path; the header must be absent because virtual creds
	// carry no _connectionId. The full real-handler route (sdwebui via direct
	// client, no proxy-aware fetch seam) is exercised by the e2e tests.
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, _ := newImageMux(t, stub)

	// Loopback viewer, sdwebui → stub returns 200; header must be absent.
	req := imageReq(t, `{"model":"sdwebui/sd-1.5","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("x-9gouter-connection-id"); got != "" {
		t.Fatalf("x-9gouter-connection-id = %q, want empty (no-auth provider)", got)
	}
}

func TestV1Images_LocalProvidersUseLiteralLoopbackTarget(t *testing.T) {
	for _, p := range []string{"sdwebui", "comfyui"} {
		cfg, ok := imageLookupForTest(p)
		if !ok {
			t.Fatalf("%s not in registry", p)
		}
		if !strings.HasPrefix(cfg.BaseURL, "http://127.0.0.1:") {
			t.Fatalf("%s BaseURL = %q, want literal http://127.0.0.1:* (not localhost/DNS)", p, cfg.BaseURL)
		}
		if strings.Contains(cfg.BaseURL, "localhost") {
			t.Fatalf("%s BaseURL = %q must not use localhost", p, cfg.BaseURL)
		}
	}
}

func TestV1Images_OptionsForwardedToStub(t *testing.T) {
	// Verify parsed optional fields reach the ImageHandler probe via Options.
	// Use fal-ai (image allowed) plus its supported fields — only image is
	// authorised by fal-ai's row, so the others must be absent. We assert image
	// is forwarded; unsupported fields on fal-ai are tested elsewhere.
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "fal-ai", `{"apiKey":"k"}`)

	req := imageReq(t, `{"model":"fal-ai/flux/schnell","prompt":"cat","image":"https://x/a.png"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastReq.Options.RawImageInputs == nil || len(stub.lastReq.Options.RawImageInputs) != 1 {
		t.Fatalf("RawImageInputs = %v, want 1 entry", stub.lastReq.Options.RawImageInputs)
	}
	if string(stub.lastReq.Options.RawImageInputs[0]) != `"https://x/a.png"` {
		t.Fatalf("raw image = %q", string(stub.lastReq.Options.RawImageInputs[0]))
	}
}

func TestV1Images_BodySizeLimit24MiB(t *testing.T) {
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "fal-ai", `{"apiKey":"k"}`)

	// A ~16 MiB base64 data URL must pass the 24 MiB envelope cap.
	// 16 MiB of base64 = ~22 MiB of text. Build a data URL just under that.
	b64 := strings.Repeat("A", 16<<20) // 16 MiB of base64 chars → fits in 24 MiB
	body := fmt.Sprintf(`{"model":"fal-ai/flux/schnell","prompt":"cat","image":"data:image/png;base64,%s"}`, b64)
	if int64(len(body)) > imageMaxBodyBytes {
		t.Fatalf("test body %d exceeds cap %d — adjust the test", len(body), imageMaxBodyBytes)
	}
	req := imageReq(t, body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("16 MiB data URL should pass 24 MiB envelope cap; status = %d; body=%s", rec.Code, rec.Body.String())
	}

	// A body one byte over the 24 MiB cap must be rejected (json decode reads
	// until EOF and hits the LimitReader → error → 400).
	over := make([]byte, int(imageMaxBodyBytes)+1)
	for i := range over {
		over[i] = ' '
	}
	// Make it not valid JSON but force the decoder to read past the limit.
	body2 := `{"model":"fal-ai/flux/schnell","prompt":"cat","image":"` + string(over) + `"}`
	req2 := imageReq(t, body2)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("24MiB+1 body should be rejected; status = %d; body=%s", rec2.Code, rec2.Body.String())
	}
}

func TestV1Images_AbsentVsSuppliedNullPresence(t *testing.T) {
	// openai + absent seed → OK (no field supplied). openai + seed:null → 400.
	stub := &stubImageHandler{body: []byte(`{"created":1,"data":[]}`), ct: "application/json"}
	mux, db := newImageMux(t, stub)
	mustCreateConnection(t, db, "openai", `{"apiKey":"k"}`)

	// Absent: no seed key → handler proceeds to executor (200 from stub).
	req := imageReq(t, `{"model":"dall-e-3","prompt":"cat"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("absent seed: status = %d; body=%s", rec.Code, rec.Body.String())
	}

	// Supplied null: seed:null → 400 (presence, not value).
	req2 := imageReq(t, `{"model":"dall-e-3","prompt":"cat","seed":null}`)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("seed:null: status = %d, want 400 (supplied null is still supplied)", rec2.Code)
	}
}
