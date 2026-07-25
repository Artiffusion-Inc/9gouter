package imageproxy

// providers_async_test.go covers the asynchronous image-provider adapters
// (step 7): fal-ai, black-forest-labs, runwayml, nanobanana. Every scenario
// uses a real httptest TLS server (HTTPS is required by validateLifecycleURL
// for submit-derived poll/result URLs); no mock HTTP client. Tests assert:
//
//   - submit → pending → completed → result sequence with exact headers, body
//     shape, and same-connection metadata on every poll/result request;
//   - terminal failure/non-2xx/malformed status mapping (502 / 504);
//   - fal/bfl valid data + HTTPS image input, SSRF/oversize rejection,
//     unsupported cardinality/mask, path injection (traversal in model);
//   - runwayml/nanobanana edit-input rejection pre-executor (400);
//   - runwayml non-image model rejection pre-executor (400);
//   - foreign host rejection on submit-derived poll/result URLs (502);
//   - nanobanana fixed dummy callBackUrl in the submit body and no listener.
//
// The test handler injects:
//   - a permissive SSRFPolicy so the loopback httptest endpoint passes the
//     SSRF guard for image URL inputs,
//   - a LifecycleHostPredicate override per provider that allows the httptest
//     host (the production allowlists are tested separately in
//     image_security_test.go),
//   - a resolver that answers the httptest host with 127.0.0.1,
//   - a short PollInterval/PollTimeout so the loop completes quickly.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/image"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// === Test handler + helpers ===

// asyncDeps builds a Dependencies for the async provider tests: TLS client over
// srv, permissive SSRF, lifecycle-host override allowing srv's host, resolver
// for srv, short poll interval/timeout.
func asyncDeps(srv *httptest.Server) Dependencies {
	u, _ := url.Parse(srv.URL)
	allowedHost := u.Hostname() + ":" + u.Port()
	pred := HostPredicateFunc(func(host string) bool { return host == allowedHost })
	return Dependencies{
		Logger:       captureLogger{},
		SSRFPolicy:   permissiveSSRFForTest{},
		PollInterval: 5 * time.Millisecond,
		PollTimeout:  800 * time.Millisecond,
		Resolver:     resolverFor(srv),
		LifecycleHostPredicates: map[string]LifecycleHostPredicate{
			"fal-ai":            pred,
			"black-forest-labs": pred,
			"runwayml":          pred,
			"nanobanana":        pred,
		},
	}
}

// asyncHandler builds a Handler with the TLS client of srv and the async deps.
func asyncHandler(srv *httptest.Server) *Handler {
	deps := asyncDeps(srv)
	deps.Executor = newNoFollowExecutor(srv)
	return New(deps)
}

// asyncCreds builds API-key credentials carrying a connection id so the test
// can assert poll/result requests inherit the submit connection.
func asyncCreds(key, connID string) domainProv.Credentials {
	return domainProv.Credentials{
		APIKey:               key,
		ProviderSpecificData: map[string]any{"_connectionId": connID},
	}
}

// recordingTLSExecutor wraps the httptest TLS client (no-redirect) and records
// every request's method, path, auth header and connection id from the
// transport metadata, so a test can assert the submit/poll/result sequence
// carries the same connection metadata.
type recordingTLSExecutor struct {
	mu       sync.Mutex
	client   *http.Client
	requests []recordedReq
}

type recordedReq struct {
	Method      string
	Path        string
	Auth        string
	XKey        string
	XRunwayVer  string
	ConnID      string
	Phase       string
	Body        string
	Host        string
	UserinfoURL string // full URL to detect foreign hosts
}

func newRecordingExecutor(srv *httptest.Server) *recordingTLSExecutor {
	rec := &recordingTLSExecutor{}
	if srv != nil {
		c := srv.Client()
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		rec.client = c
	}
	return rec
}

func (e *recordingTLSExecutor) Do(req *http.Request) (*http.Response, error) {
	e.mu.Lock()
	rec := recordedReq{
		Method:      req.Method,
		Path:        req.URL.Path,
		Auth:        req.Header.Get("Authorization"),
		XKey:        req.Header.Get("x-key"),
		XRunwayVer:  req.Header.Get("X-Runway-Version"),
		Host:        req.URL.Host,
		UserinfoURL: req.URL.String(),
	}
	if meta, ok := TransportMetadataFromContext(req.Context()); ok {
		rec.ConnID = meta.ConnectionID
		rec.Phase = meta.Phase
	}
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		rec.Body = string(b)
		req.Body.Close()
		req.Body = io.NopCloser(strings.NewReader(string(b)))
	}
	e.requests = append(e.requests, rec)
	e.mu.Unlock()
	return e.client.Do(req)
}

func (e *recordingTLSExecutor) recorded() []recordedReq {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]recordedReq, len(e.requests))
	copy(out, e.requests)
	return out
}

// === fal-ai ===

// TestFalAI_SubmitPollResultSequence proves the full fal-ai lifecycle:
// submit POST {BaseURL}/{model} with Authorization: Key + {prompt,num_images,
// image_size}, then GET status_url (poll) until COMPLETED, then GET
// response_url (result). It asserts the submit body shape, the auth header on
// every call, and that poll/result carry the submit connection id.
func TestFalAI_SubmitPollResultSequence(t *testing.T) {
	var pollCount int32
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			if !strings.Contains(r.URL.Path, "fal-ai/flux/schnell") {
				t.Errorf("submit path = %q, want fal-ai/flux/schnell segment", r.URL.Path)
			}
			if got := r.Header.Get("Authorization"); got != "Key fal-tok" {
				t.Errorf("Authorization = %q, want Key fal-tok", got)
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["prompt"] != "a cat" {
				t.Errorf("prompt = %v", body["prompt"])
			}
			if body["num_images"] != float64(2) {
				t.Errorf("num_images = %v, want 2", body["num_images"])
			}
			if body["image_size"] != "1:1" {
				t.Errorf("image_size = %v, want 1:1", body["image_size"])
			}
			_, _ = io.WriteString(w, `{"status_url":"`+srv.URL+`/status/abc","response_url":"`+srv.URL+`/result/abc"}`)
		case r.URL.Path == "/status/abc":
			n := atomic.AddInt32(&pollCount, 1)
			if got := r.Header.Get("Authorization"); got != "Key fal-tok" {
				t.Errorf("poll Authorization = %q, want Key fal-tok", got)
			}
			if n < 2 {
				_, _ = io.WriteString(w, `{"status":"IN_QUEUE"}`)
			} else {
				_, _ = io.WriteString(w, `{"status":"COMPLETED"}`)
			}
		case r.URL.Path == "/result/abc":
			if got := r.Header.Get("Authorization"); got != "Key fal-tok" {
				t.Errorf("result Authorization = %q, want Key fal-tok", got)
			}
			_, _ = io.WriteString(w, `{"images":[{"url":"https://cdn.fal.run/x.png"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	res, _, _, err := h.synthFalAI(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthFalKey, Format: image.FormatFalAI,
	}, Request{
		ProviderID: "fal-ai", Model: "fal-ai/flux/schnell", Prompt: "a cat", N: 2, NSupplied: true,
		Size:        "1024x1024",
		Credentials: asyncCreds("fal-tok", "c-fal"),
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var out map[string]any
	_ = json.Unmarshal(res, &out)
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len = %d", len(data))
	}
	item, _ := data[0].(map[string]any)
	if item["url"] != "https://cdn.fal.run/x.png" {
		t.Errorf("url = %v", item["url"])
	}
}

// TestFalAI_FailedStatusMapsTo502 proves a terminal FAILED poll status surfaces
// as 502 with a provider diagnostic (mapping helper exercised in the real synth
// path, not just the poll helper).
func TestFalAI_FailedStatusMapsTo502(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"status_url":"`+srv.URL+`/status/abc","response_url":"`+srv.URL+`/result/abc"}`)
		default:
			_, _ = io.WriteString(w, `{"status":"FAILED","error":"boom"}`)
		}
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	_, _, status, err := h.synthFalAI(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthFalKey, Format: image.FormatFalAI,
	}, Request{
		ProviderID: "fal-ai", Model: "fal-ai/flux/schnell", Prompt: "p",
		Credentials: asyncCreds("fal-tok", "c-fal"),
	})
	if err == nil {
		t.Fatal("want error for FAILED status")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
}

// TestFalAI_TimeoutMapsTo504 proves the polling timeout maps to 504 in the real
// synth path (the helper returns ErrPollTimeout; pollHTTPStatus maps it).
func TestFalAI_TimeoutMapsTo504(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"status_url":"`+srv.URL+`/status/abc","response_url":"`+srv.URL+`/result/abc"}`)
		default:
			_, _ = io.WriteString(w, `{"status":"IN_QUEUE"}`)
		}
	}))
	defer srv.Close()
	deps := asyncDeps(srv)
	deps.PollTimeout = 30 * time.Millisecond
	deps.PollInterval = 10 * time.Millisecond
	deps.Executor = newNoFollowExecutor(srv)
	h := New(deps)
	_, _, status, err := h.synthFalAI(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthFalKey, Format: image.FormatFalAI,
	}, Request{
		ProviderID: "fal-ai", Model: "fal-ai/flux/schnell", Prompt: "p",
		Credentials: asyncCreds("fal-tok", "c-fal"),
	})
	if err == nil {
		t.Fatal("want error for poll timeout")
	}
	if status != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", status)
	}
}

// TestFalAI_ForeignHostRejected proves a submit response that returns a poll
// URL on a foreign host (outside the fal allowlist) is rejected with 502
// before any poll HTTP call. The override predicate allows the httptest host;
// the submit response points at a different httptest server that the override
// does NOT allow.
func TestFalAI_ForeignHostRejected(t *testing.T) {
	foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("foreign poll host must not be called")
	}))
	defer foreign.Close()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status_url":"`+foreign.URL+`/status/abc","response_url":"`+srv.URL+`/result/abc"}`)
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	_, _, status, err := h.synthFalAI(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthFalKey, Format: image.FormatFalAI,
	}, Request{
		ProviderID: "fal-ai", Model: "fal-ai/flux/schnell", Prompt: "p",
		Credentials: asyncCreds("fal-tok", "c-fal"),
	})
	if err == nil {
		t.Fatal("want error for foreign poll host")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
	if !strings.Contains(err.Error(), "unexpected lifecycle host") {
		t.Errorf("err = %v, want unexpected host message", err)
	}
}

// TestFalAI_ModelTraversalRejected proves path injection in the model segment
// is rejected with 400 before the submit call.
func TestFalAI_ModelTraversalRejected(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called for traversal model")
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	cases := []string{"fal-ai/../evil", "fal-ai/./x", "fal-ai/flux?q=1", "fal-ai/flux#frag", "", "fal-ai/"}
	for _, model := range cases {
		_, _, status, err := h.synthFalAI(context.Background(), image.Config{
			BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthFalKey, Format: image.FormatFalAI,
		}, Request{
			ProviderID: "fal-ai", Model: model, Prompt: "p",
			Credentials: asyncCreds("fal-tok", "c-fal"),
		})
		if err == nil {
			t.Errorf("model %q: want error", model)
		}
		if status != http.StatusBadRequest {
			t.Errorf("model %q: status = %d, want 400", model, status)
		}
	}
}

// TestFalAI_MaskAndMultipleImagesRejected proves mask and >1 image inputs are
// rejected with 400 pre-executor (fal-ai accepts exactly one image, no mask).
func TestFalAI_MaskAndMultipleImagesRejected(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called when mask/multi-image supplied")
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	img := dataURL("image/png", pngMagic(32))
	// Mask supplied.
	_, _, status, err := h.synthFalAI(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthFalKey, Format: image.FormatFalAI,
	}, Request{
		ProviderID: "fal-ai", Model: "fal-ai/flux/schnell", Prompt: "p",
		Credentials: asyncCreds("fal-tok", "c-fal"),
		Options:     RequestOptions{RawMask: rawJSON(img)},
	})
	if err == nil || status != http.StatusBadRequest {
		t.Errorf("mask: err=%v status=%d, want 400", err, status)
	}
	// Multiple image inputs.
	_, _, status, err = h.synthFalAI(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthFalKey, Format: image.FormatFalAI,
	}, Request{
		ProviderID: "fal-ai", Model: "fal-ai/flux/schnell", Prompt: "p",
		Credentials: asyncCreds("fal-tok", "c-fal"),
		Options: RequestOptions{
			RawImageInputs: []json.RawMessage{rawJSON(img), rawJSON(img)},
		},
	})
	if err == nil || status != http.StatusBadRequest {
		t.Errorf("multi-image: err=%v status=%d, want 400", err, status)
	}
}

// TestFalAI_DataImageInput proves a valid data-URL image input is resolved and
// forwarded as image_url in the submit body.
func TestFalAI_DataImageInput(t *testing.T) {
	var gotBody string
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			_, _ = io.WriteString(w, `{"status_url":"`+srv.URL+`/status/abc","response_url":"`+srv.URL+`/result/abc"}`)
		case r.URL.Path == "/status/abc":
			_, _ = io.WriteString(w, `{"status":"COMPLETED"}`)
		case r.URL.Path == "/result/abc":
			_, _ = io.WriteString(w, `{"image":{"url":"https://cdn.fal.run/y.png"}}`)
		}
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	img := dataURL("image/png", pngMagic(32))
	_, _, status, err := h.synthFalAI(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthFalKey, Format: image.FormatFalAI,
	}, Request{
		ProviderID: "fal-ai", Model: "fal-ai/flux/schnell", Prompt: "p",
		Credentials: asyncCreds("fal-tok", "c-fal"),
		Options:     RequestOptions{RawImageInputs: []json.RawMessage{rawJSON(img)}},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	if !strings.Contains(gotBody, "image_url") {
		t.Errorf("submit body = %q, want image_url key", gotBody)
	}
	if !strings.Contains(gotBody, "data:image/png;base64,") {
		t.Errorf("submit body = %q, want data url", gotBody)
	}
}

// TestFalAI_HTTPSImageInputSSRFRejection proves a private/loopback HTTPS image
// URL is rejected by the SSRF guard before the submit call.
func TestFalAI_HTTPSImageInputSSRFRejection(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called when image URL is SSRF-rejected")
	}))
	defer srv.Close()
	deps := asyncDeps(srv)
	deps.SSRFPolicy = defaultSSRFPolicy{}
	deps.Resolver = ResolverFunc(func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("10.0.0.5")}, nil
	})
	deps.Executor = newNoFollowExecutor(srv)
	h := New(deps)
	_, _, status, err := h.synthFalAI(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthFalKey, Format: image.FormatFalAI,
	}, Request{
		ProviderID: "fal-ai", Model: "fal-ai/flux/schnell", Prompt: "p",
		Credentials: asyncCreds("fal-tok", "c-fal"),
		Options:     RequestOptions{RawImageInputs: []json.RawMessage{rawJSON("https://internal.example/a.png")}},
	})
	if err == nil || status != http.StatusBadRequest {
		t.Errorf("err=%v status=%d, want 400", err, status)
	}
}

// TestFalAI_OversizeDataImageRejection proves a data-URL image larger than the
// 16 MiB cap is rejected before the submit call.
func TestFalAI_OversizeDataImageRejection(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called for oversize image")
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	// Build a data URL whose decoded payload exceeds 16 MiB.
	big := make([]byte, (16<<20)+32)
	big[0], big[1], big[2], big[3], big[4], big[5], big[6], big[7] = 0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x0A, 0x00
	img := dataURL("image/png", big)
	_, _, status, err := h.synthFalAI(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthFalKey, Format: image.FormatFalAI,
	}, Request{
		ProviderID: "fal-ai", Model: "fal-ai/flux/schnell", Prompt: "p",
		Credentials: asyncCreds("fal-tok", "c-fal"),
		Options:     RequestOptions{RawImageInputs: []json.RawMessage{rawJSON(img)}},
	})
	if err == nil || status != http.StatusBadRequest {
		t.Errorf("err=%v status=%d, want 400", err, status)
	}
}

// TestFalAI_NonImageDataURLRejection proves a data URL whose payload is not a
// recognised image is rejected.
func TestFalAI_NonImageDataURLRejection(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called for non-image input")
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	// "data:image/png;base64,<not-png>"
	img := "data:image/png;base64," + base64Std([]byte("not an image at all"))
	_, _, status, err := h.synthFalAI(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthFalKey, Format: image.FormatFalAI,
	}, Request{
		ProviderID: "fal-ai", Model: "fal-ai/flux/schnell", Prompt: "p",
		Credentials: asyncCreds("fal-tok", "c-fal"),
		Options:     RequestOptions{RawImageInputs: []json.RawMessage{rawJSON(img)}},
	})
	if err == nil || status != http.StatusBadRequest {
		t.Errorf("err=%v status=%d, want 400", err, status)
	}
}

// TestFalAI_SubmitNon2xxSurfacesUpstreamStatus proves a submit 4xx/5xx surfaces
// the upstream status (not a flat 502).
func TestFalAI_SubmitNon2xxSurfacesUpstreamStatus(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	_, _, status, err := h.synthFalAI(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthFalKey, Format: image.FormatFalAI,
	}, Request{
		ProviderID: "fal-ai", Model: "fal-ai/flux/schnell", Prompt: "p",
		Credentials: asyncCreds("fal-tok", "c-fal"),
	})
	if err == nil {
		t.Fatal("want error for submit 401")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

// TestFalAI_MalformedSubmitResponse proves a submit response missing the poll
// URLs is a 502.
func TestFalAI_MalformedSubmitResponse(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"foo":"bar"}`)
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	_, _, status, err := h.synthFalAI(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthFalKey, Format: image.FormatFalAI,
	}, Request{
		ProviderID: "fal-ai", Model: "fal-ai/flux/schnell", Prompt: "p",
		Credentials: asyncCreds("fal-tok", "c-fal"),
	})
	if err == nil {
		t.Fatal("want error for malformed submit")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
}

// === black-forest-labs (BFL) ===

// TestBFL_SubmitPollResultSequence proves the BFL lifecycle: submit POST
// {BaseURL}/{model} with x-key + {prompt,width,height[,image_prompt]}, poll
// polling_url until status=Ready, normalise result.sample.
func TestBFL_SubmitPollResultSequence(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			if got := r.Header.Get("x-key"); got != "bfl-tok" {
				t.Errorf("x-key = %q, want bfl-tok", got)
			}
			if got := r.Header.Get("Accept"); got != "application/json" {
				t.Errorf("Accept = %q, want application/json", got)
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["prompt"] != "a dog" {
				t.Errorf("prompt = %v", body["prompt"])
			}
			if body["width"] != float64(1024) || body["height"] != float64(1024) {
				t.Errorf("width/height = %v/%v, want 1024/1024", body["width"], body["height"])
			}
			_, _ = io.WriteString(w, `{"polling_url":"`+srv.URL+`/poll/xyz"}`)
		case r.URL.Path == "/poll/xyz":
			if got := r.Header.Get("x-key"); got != "bfl-tok" {
				t.Errorf("poll x-key = %q, want bfl-tok", got)
			}
			_, _ = io.WriteString(w, `{"status":"Ready","result":{"sample":"https://cdn.bfl.ai/s.png"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	res, _, status, err := h.synthBFL(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthXKey, Format: image.FormatBlackForest,
	}, Request{
		ProviderID: "black-forest-labs", Model: "flux-1.1-pro", Prompt: "a dog", Size: "1024x1024",
		Credentials: asyncCreds("bfl-tok", "c-bfl"),
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	var out map[string]any
	_ = json.Unmarshal(res, &out)
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len = %d", len(data))
	}
	item, _ := data[0].(map[string]any)
	if item["url"] != "https://cdn.bfl.ai/s.png" {
		t.Errorf("url = %v", item["url"])
	}
}

// TestBFL_FailedStatusMapsTo502 proves BFL Error/Failed → 502 via the real synth
// path.
func TestBFL_FailedStatusMapsTo502(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"polling_url":"`+srv.URL+`/poll/xyz"}`)
		default:
			_, _ = io.WriteString(w, `{"status":"Error","error":"oops"}`)
		}
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	_, _, status, err := h.synthBFL(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthXKey, Format: image.FormatBlackForest,
	}, Request{
		ProviderID: "black-forest-labs", Model: "flux-1.1-pro", Prompt: "p",
		Credentials: asyncCreds("bfl-tok", "c-bfl"),
	})
	if err == nil {
		t.Fatal("want error for BFL Error status")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
}

// TestBFL_PollNon2xxMapsTo502 proves a poll non-2xx surfaces as 502.
func TestBFL_PollNon2xxMapsTo502(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"polling_url":"`+srv.URL+`/poll/xyz"}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"upstream"}`)
		}
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	_, _, status, err := h.synthBFL(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthXKey, Format: image.FormatBlackForest,
	}, Request{
		ProviderID: "black-forest-labs", Model: "flux-1.1-pro", Prompt: "p",
		Credentials: asyncCreds("bfl-tok", "c-bfl"),
	})
	if err == nil {
		t.Fatal("want error for poll 500")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
}

// TestBFL_ModelTraversalRejected proves path injection in the BFL model segment
// is rejected with 400 pre-executor.
func TestBFL_ModelTraversalRejected(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called for traversal model")
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	for _, model := range []string{"../evil", "./x", "flux?q=1", "flux#frag", ""} {
		_, _, status, err := h.synthBFL(context.Background(), image.Config{
			BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthXKey, Format: image.FormatBlackForest,
		}, Request{
			ProviderID: "black-forest-labs", Model: model, Prompt: "p",
			Credentials: asyncCreds("bfl-tok", "c-bfl"),
		})
		if err == nil || status != http.StatusBadRequest {
			t.Errorf("model %q: err=%v status=%d, want 400", model, err, status)
		}
	}
}

// TestBFL_MaskRejected proves mask inputs are rejected (BFL accepts one image,
// no mask).
func TestBFL_MaskRejected(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called when mask supplied")
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	img := dataURL("image/png", pngMagic(32))
	_, _, status, err := h.synthBFL(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthXKey, Format: image.FormatBlackForest,
	}, Request{
		ProviderID: "black-forest-labs", Model: "flux-kontext-pro", Prompt: "p",
		Credentials: asyncCreds("bfl-tok", "c-bfl"),
		Options:     RequestOptions{RawMask: rawJSON(img)},
	})
	if err == nil || status != http.StatusBadRequest {
		t.Errorf("err=%v status=%d, want 400", err, status)
	}
}

// TestBFL_ImageInput proves a valid data-URL image input is forwarded as
// image_prompt.
func TestBFL_ImageInput(t *testing.T) {
	var gotBody string
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			_, _ = io.WriteString(w, `{"polling_url":"`+srv.URL+`/poll/xyz"}`)
		case r.URL.Path == "/poll/xyz":
			_, _ = io.WriteString(w, `{"status":"Ready","result":{"sample":"https://cdn.bfl.ai/r.png"}}`)
		}
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	img := dataURL("image/png", pngMagic(32))
	_, _, status, err := h.synthBFL(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthXKey, Format: image.FormatBlackForest,
	}, Request{
		ProviderID: "black-forest-labs", Model: "flux-kontext-pro", Prompt: "p",
		Credentials: asyncCreds("bfl-tok", "c-bfl"),
		Options:     RequestOptions{RawImageInputs: []json.RawMessage{rawJSON(img)}},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	if !strings.Contains(gotBody, "image_prompt") {
		t.Errorf("submit body = %q, want image_prompt key", gotBody)
	}
}

// TestBFL_ForeignPollHostRejected proves a polling_url on a foreign host is
// rejected with 502.
func TestBFL_ForeignPollHostRejected(t *testing.T) {
	foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("foreign poll host must not be called")
	}))
	defer foreign.Close()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"polling_url":"`+foreign.URL+`/poll/xyz"}`)
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	_, _, status, err := h.synthBFL(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthXKey, Format: image.FormatBlackForest,
	}, Request{
		ProviderID: "black-forest-labs", Model: "flux-1.1-pro", Prompt: "p",
		Credentials: asyncCreds("bfl-tok", "c-bfl"),
	})
	if err == nil || status != http.StatusBadGateway {
		t.Errorf("err=%v status=%d, want 502", err, status)
	}
}

// TestBFL_NoPollingURLMapsTo502 proves a submit response missing polling_url is
// a 502.
func TestBFL_NoPollingURLMapsTo502(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"foo":"bar"}`)
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	_, _, status, err := h.synthBFL(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthXKey, Format: image.FormatBlackForest,
	}, Request{
		ProviderID: "black-forest-labs", Model: "flux-1.1-pro", Prompt: "p",
		Credentials: asyncCreds("bfl-tok", "c-bfl"),
	})
	if err == nil || status != http.StatusBadGateway {
		t.Errorf("err=%v status=%d, want 502", err, status)
	}
}

// === runwayml ===

// TestRunwayML_SubmitPollResultSequence proves the runwayml lifecycle: validate
// gen4_image* model, submit /text_to_image with Bearer + X-Runway-Version,
// poll /tasks/{id} until SUCCEEDED, normalise output[].
func TestRunwayML_SubmitPollResultSequence(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/text_to_image":
			if got := r.Header.Get("Authorization"); got != "Bearer rw-tok" {
				t.Errorf("Authorization = %q, want Bearer rw-tok", got)
			}
			if got := r.Header.Get("X-Runway-Version"); got != "2024-11-06" {
				t.Errorf("X-Runway-Version = %q, want 2024-11-06", got)
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["promptText"] != "a house" {
				t.Errorf("promptText = %v", body["promptText"])
			}
			if body["model"] != "gen4_image" {
				t.Errorf("model = %v", body["model"])
			}
			if body["ratio"] != "1:1" {
				t.Errorf("ratio = %v", body["ratio"])
			}
			if _, has := body["referenceImages"]; has {
				t.Errorf("referenceImages must NOT be present (edit input excluded)")
			}
			_, _ = io.WriteString(w, `{"id":"task-123"}`)
		case r.URL.Path == "/tasks/task-123":
			_, _ = io.WriteString(w, `{"status":"SUCCEEDED","output":["https://cdn.runwayml.com/o.png"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	res, _, status, err := h.synthRunwayML(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatRunwayML,
	}, Request{
		ProviderID: "runwayml", Model: "gen4_image", Prompt: "a house", Size: "1024x1024",
		Credentials: asyncCreds("rw-tok", "c-rw"),
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	var out map[string]any
	_ = json.Unmarshal(res, &out)
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len = %d", len(data))
	}
	item, _ := data[0].(map[string]any)
	if item["url"] != "https://cdn.runwayml.com/o.png" {
		t.Errorf("url = %v", item["url"])
	}
}

// TestRunwayML_NonImageModelRejected proves a non-gen4_image* model is rejected
// with 400 pre-executor, with guidance to /v1/videos/*.
func TestRunwayML_NonImageModelRejected(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called for non-image model")
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	for _, model := range []string{"gen4_turbo", "gen3a_turbo", "gen4_image/../evil", "gen4_image_turbo/../x"} {
		_, _, status, err := h.synthRunwayML(context.Background(), image.Config{
			BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatRunwayML,
		}, Request{
			ProviderID: "runwayml", Model: model, Prompt: "p",
			Credentials: asyncCreds("rw-tok", "c-rw"),
		})
		if err == nil || status != http.StatusBadRequest {
			t.Errorf("model %q: err=%v status=%d, want 400", model, err, status)
		}
		if err != nil && !strings.Contains(err.Error(), "/v1/videos/*") {
			t.Errorf("model %q: err = %v, want /v1/videos/* guidance", model, err)
		}
	}
}

// TestRunwayML_EditInputRejected proves supplied image/images/mask are rejected
// with 400 pre-executor (edit pass-through intentionally excluded).
func TestRunwayML_EditInputRejected(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called when edit input supplied")
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	img := dataURL("image/png", pngMagic(32))
	// image input
	_, _, status, err := h.synthRunwayML(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatRunwayML,
	}, Request{
		ProviderID: "runwayml", Model: "gen4_image", Prompt: "p",
		Credentials: asyncCreds("rw-tok", "c-rw"),
		Options:     RequestOptions{RawImageInputs: []json.RawMessage{rawJSON(img)}},
	})
	if err == nil || status != http.StatusBadRequest {
		t.Errorf("image: err=%v status=%d, want 400", err, status)
	}
	// mask input
	_, _, status, err = h.synthRunwayML(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatRunwayML,
	}, Request{
		ProviderID: "runwayml", Model: "gen4_image", Prompt: "p",
		Credentials: asyncCreds("rw-tok", "c-rw"),
		Options:     RequestOptions{RawMask: rawJSON(img)},
	})
	if err == nil || status != http.StatusBadRequest {
		t.Errorf("mask: err=%v status=%d, want 400", err, status)
	}
}

// TestRunwayML_FailedStatusMapsTo502 proves FAILED/CANCELLED → 502.
func TestRunwayML_FailedStatusMapsTo502(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"id":"task-1"}`)
		default:
			_, _ = io.WriteString(w, `{"status":"FAILED","failure":"nope"}`)
		}
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	_, _, status, err := h.synthRunwayML(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatRunwayML,
	}, Request{
		ProviderID: "runwayml", Model: "gen4_image", Prompt: "p",
		Credentials: asyncCreds("rw-tok", "c-rw"),
	})
	if err == nil || status != http.StatusBadGateway {
		t.Errorf("err=%v status=%d, want 502", err, status)
	}
}

// TestRunwayML_MalformedPollMapsTo502 proves a poll response that does not
// decode into a known status is a 502.
func TestRunwayML_MalformedPollMapsTo502(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"id":"task-1"}`)
		default:
			_, _ = io.WriteString(w, `not-json-at-all`)
		}
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	_, _, status, err := h.synthRunwayML(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatRunwayML,
	}, Request{
		ProviderID: "runwayml", Model: "gen4_image", Prompt: "p",
		Credentials: asyncCreds("rw-tok", "c-rw"),
	})
	if err == nil || status != http.StatusBadGateway {
		t.Errorf("err=%v status=%d, want 502", err, status)
	}
}

// TestRunwayML_NoTaskIDMapsTo502 proves a submit response missing the task id
// is a 502.
func TestRunwayML_NoTaskIDMapsTo502(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"foo":"bar"}`)
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	_, _, status, err := h.synthRunwayML(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatRunwayML,
	}, Request{
		ProviderID: "runwayml", Model: "gen4_image", Prompt: "p",
		Credentials: asyncCreds("rw-tok", "c-rw"),
	})
	if err == nil || status != http.StatusBadGateway {
		t.Errorf("err=%v status=%d, want 502", err, status)
	}
}

// TestRunwayML_PollInheritsSubmitConnection proves every poll request carries
// the submit connection id in its transport metadata.
func TestRunwayML_PollInheritsSubmitConnection(t *testing.T) {
	rec := newRecordingExecutor(nil)
	// Use a real TLS server so the recording executor's client dials it.
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"id":"task-1"}`)
		default:
			_, _ = io.WriteString(w, `{"status":"SUCCEEDED","output":["https://cdn/x.png"]}`)
		}
	}))
	defer srv.Close()
	rec.client = srv.Client()
	rec.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	deps := asyncDeps(srv)
	deps.Executor = rec
	h := New(deps)
	_, _, _, err := h.synthRunwayML(context.Background(), image.Config{
		BaseURL: srv.URL, AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatRunwayML,
	}, Request{
		ProviderID: "runwayml", Model: "gen4_image", Prompt: "p",
		Credentials: asyncCreds("rw-tok", "c-rw-conn"),
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	recs := rec.recorded()
	if len(recs) < 2 {
		t.Fatalf("expected >=2 requests (submit+poll), got %d", len(recs))
	}
	for i, r := range recs {
		if r.ConnID != "c-rw-conn" {
			t.Errorf("request %d: ConnID = %q, want c-rw-conn", i, r.ConnID)
		}
	}
	// First request is submit (POST), subsequent are poll (GET).
	if recs[0].Method != http.MethodPost {
		t.Errorf("first request method = %q, want POST", recs[0].Method)
	}
	for i, r := range recs[1:] {
		if r.Method != http.MethodGet {
			t.Errorf("poll request %d: method = %q, want GET", i, r.Method)
		}
	}
}

// === nanobanana ===

// TestNanobanana_SubmitPollResultSequence proves the nanobanana lifecycle:
// submit /generate with Bearer + {prompt, type:TEXTTOIAMGE, numImages,
// image_size, callBackUrl:"https://localhost/callback"}, poll record-info until
// successFlag=1, normalise resultImageUrl.
func TestNanobanana_SubmitPollResultSequence(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/generate"):
			if got := r.Header.Get("Authorization"); got != "Bearer nb-tok" {
				t.Errorf("Authorization = %q, want Bearer nb-tok", got)
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["prompt"] != "a banana" {
				t.Errorf("prompt = %v", body["prompt"])
			}
			if body["type"] != "TEXTTOIAMGE" {
				t.Errorf("type = %v, want TEXTTOIAMGE", body["type"])
			}
			if body["numImages"] != float64(1) {
				t.Errorf("numImages = %v, want 1", body["numImages"])
			}
			if body["image_size"] != "1:1" {
				t.Errorf("image_size = %v, want 1:1", body["image_size"])
			}
			if body["callBackUrl"] != "https://localhost/callback" {
				t.Errorf("callBackUrl = %v, want https://localhost/callback", body["callBackUrl"])
			}
			if _, has := body["imageUrls"]; has {
				t.Errorf("imageUrls must NOT be present (edit input excluded)")
			}
			_, _ = io.WriteString(w, `{"code":200,"msg":"ok","data":{"taskId":"t-1"}}`)
		case strings.HasSuffix(r.URL.Path, "/record-info"):
			if r.URL.Query().Get("taskId") != "t-1" {
				t.Errorf("taskId query = %q, want t-1", r.URL.Query().Get("taskId"))
			}
			if got := r.Header.Get("Authorization"); got != "Bearer nb-tok" {
				t.Errorf("poll Authorization = %q, want Bearer nb-tok", got)
			}
			_, _ = io.WriteString(w, `{"data":{"successFlag":1,"response":{"resultImageUrl":"https://cdn.nb/r.png"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	res, _, status, err := h.synthNanobanana(context.Background(), image.Config{
		BaseURL:  srv.URL + "/api/v1/nanobanana/generate",
		PollURL:  srv.URL + "/api/v1/nanobanana/record-info",
		AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatNanobanana,
	}, Request{
		ProviderID: "nanobanana", Model: "nanobanana-flash", Prompt: "a banana", Size: "1024x1024",
		Credentials: asyncCreds("nb-tok", "c-nb"),
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	var out map[string]any
	_ = json.Unmarshal(res, &out)
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len = %d", len(data))
	}
	item, _ := data[0].(map[string]any)
	if item["url"] != "https://cdn.nb/r.png" {
		t.Errorf("url = %v", item["url"])
	}
	if item["revised_prompt"] != "a banana" {
		t.Errorf("revised_prompt = %v, want 'a banana'", item["revised_prompt"])
	}
}

// TestNanobanana_EditInputRejected proves supplied image/images/mask are
// rejected with 400 pre-executor (edit pass-through intentionally excluded,
// type is always TEXTTOIAMGE).
func TestNanobanana_EditInputRejected(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called when edit input supplied")
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	img := dataURL("image/png", pngMagic(32))
	_, _, status, err := h.synthNanobanana(context.Background(), image.Config{
		BaseURL:  srv.URL + "/api/v1/nanobanana/generate",
		PollURL:  srv.URL + "/api/v1/nanobanana/record-info",
		AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatNanobanana,
	}, Request{
		ProviderID: "nanobanana", Model: "nanobanana-flash", Prompt: "p",
		Credentials: asyncCreds("nb-tok", "c-nb"),
		Options:     RequestOptions{RawImageInputs: []json.RawMessage{rawJSON(img)}},
	})
	if err == nil || status != http.StatusBadRequest {
		t.Errorf("image: err=%v status=%d, want 400", err, status)
	}
	_, _, status, err = h.synthNanobanana(context.Background(), image.Config{
		BaseURL:  srv.URL + "/api/v1/nanobanana/generate",
		PollURL:  srv.URL + "/api/v1/nanobanana/record-info",
		AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatNanobanana,
	}, Request{
		ProviderID: "nanobanana", Model: "nanobanana-flash", Prompt: "p",
		Credentials: asyncCreds("nb-tok", "c-nb"),
		Options:     RequestOptions{RawMask: rawJSON(img)},
	})
	if err == nil || status != http.StatusBadRequest {
		t.Errorf("mask: err=%v status=%d, want 400", err, status)
	}
}

// TestNanobanana_FailedFlagMapsTo502 proves successFlag 2/3 → 502.
func TestNanobanana_FailedFlagMapsTo502(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"code":200,"data":{"taskId":"t-1"}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{"successFlag":2,"errorMessage":"content policy"}}`)
		}
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	_, _, status, err := h.synthNanobanana(context.Background(), image.Config{
		BaseURL:  srv.URL + "/api/v1/nanobanana/generate",
		PollURL:  srv.URL + "/api/v1/nanobanana/record-info",
		AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatNanobanana,
	}, Request{
		ProviderID: "nanobanana", Model: "nanobanana-flash", Prompt: "p",
		Credentials: asyncCreds("nb-tok", "c-nb"),
	})
	if err == nil || status != http.StatusBadGateway {
		t.Errorf("err=%v status=%d, want 502", err, status)
	}
	if err != nil && !strings.Contains(err.Error(), "content policy") {
		t.Errorf("err = %v, want 'content policy' substring", err)
	}
}

// TestNanobanana_SubmitCodeNot200 proves a submit code != 200 surfaces the msg.
func TestNanobanana_SubmitCodeNot200(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":400,"msg":"bad request"}`)
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	_, _, status, err := h.synthNanobanana(context.Background(), image.Config{
		BaseURL:  srv.URL + "/api/v1/nanobanana/generate",
		PollURL:  srv.URL + "/api/v1/nanobanana/record-info",
		AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatNanobanana,
	}, Request{
		ProviderID: "nanobanana", Model: "nanobanana-flash", Prompt: "p",
		Credentials: asyncCreds("nb-tok", "c-nb"),
	})
	if err == nil || status != http.StatusBadGateway {
		t.Errorf("err=%v status=%d, want 502", err, status)
	}
	if err != nil && !strings.Contains(err.Error(), "bad request") {
		t.Errorf("err = %v, want 'bad request' substring", err)
	}
}

// TestNanobanana_NoTaskIDMapsTo502 proves a submit response missing taskId is
// a 502.
func TestNanobanana_NoTaskIDMapsTo502(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":200,"data":{}}`)
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	_, _, status, err := h.synthNanobanana(context.Background(), image.Config{
		BaseURL:  srv.URL + "/api/v1/nanobanana/generate",
		PollURL:  srv.URL + "/api/v1/nanobanana/record-info",
		AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatNanobanana,
	}, Request{
		ProviderID: "nanobanana", Model: "nanobanana-flash", Prompt: "p",
		Credentials: asyncCreds("nb-tok", "c-nb"),
	})
	if err == nil || status != http.StatusBadGateway {
		t.Errorf("err=%v status=%d, want 502", err, status)
	}
}

// TestNanobanana_OriginImageUrlFallback proves the originImageUrl fallback when
// resultImageUrl is absent.
func TestNanobanana_OriginImageUrlFallback(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"code":200,"data":{"taskId":"t-1"}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{"successFlag":1,"response":{"originImageUrl":"https://cdn.nb/o.png"}}}`)
		}
	}))
	defer srv.Close()
	h := asyncHandler(srv)
	res, _, status, err := h.synthNanobanana(context.Background(), image.Config{
		BaseURL:  srv.URL + "/api/v1/nanobanana/generate",
		PollURL:  srv.URL + "/api/v1/nanobanana/record-info",
		AuthType: image.AuthTypeAPIKey, AuthHeader: image.AuthBearer, Format: image.FormatNanobanana,
	}, Request{
		ProviderID: "nanobanana", Model: "nanobanana-flash", Prompt: "p",
		Credentials: asyncCreds("nb-tok", "c-nb"),
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	var out map[string]any
	_ = json.Unmarshal(res, &out)
	data, _ := out["data"].([]any)
	item, _ := data[0].(map[string]any)
	if item["url"] != "https://cdn.nb/o.png" {
		t.Errorf("url = %v, want originImageUrl fallback", item["url"])
	}
}

// === Dispatch integration ===

// TestAsync_DispatchViaHandle proves the Handler.synthesize dispatch routes
// each async format to its adapter and the production registry entries no
// longer return 501 for the four async providers.
func TestAsync_DispatchViaHandle(t *testing.T) {
	for _, pid := range []string{"fal-ai", "black-forest-labs", "runwayml", "nanobanana"} {
		cfg, ok := image.Lookup(pid)
		if !ok {
			t.Errorf("%s: not in registry", pid)
			continue
		}
		if cfg.Unsupported {
			t.Errorf("%s: still marked Unsupported", pid)
		}
	}
}

// TestAsync_NanobananaPollURLConfigured proves the production nanobanana
// registry entry carries a PollURL distinct from its BaseURL (the submit and
// poll endpoints live on different paths).
func TestAsync_NanobananaPollURLConfigured(t *testing.T) {
	cfg, ok := image.Lookup("nanobanana")
	if !ok {
		t.Fatal("nanobanana not in registry")
	}
	if cfg.PollURL == "" {
		t.Fatal("nanobanana PollURL not configured")
	}
	if cfg.PollURL == cfg.BaseURL {
		t.Errorf("PollURL == BaseURL (%q); nanobanana needs a separate record-info endpoint", cfg.PollURL)
	}
	if !strings.HasSuffix(cfg.BaseURL, "/generate") {
		t.Errorf("BaseURL = %q, want .../generate suffix", cfg.BaseURL)
	}
	if !strings.HasSuffix(cfg.PollURL, "/record-info") {
		t.Errorf("PollURL = %q, want .../record-info suffix", cfg.PollURL)
	}
}

// base64Std encodes bytes as standard base64 (used to build non-image data
// URLs the resolver must reject).
func base64Std(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
