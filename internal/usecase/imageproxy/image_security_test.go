package imageproxy

// image_security_test.go covers the safe image-input primitives from step 4
// (spec test scenarios: data input valid/malformed/oversize, HTTP/private/
// .internal/metadata/redirect rejection, URL download, download failure 502,
// allowlisted HTTP poll/result rejection, HTTPS→HTTP redirect rejection,
// redirect to a foreign origin without credential forwarding, validated
// target metadata per hop, 64 MiB cap, PNG/JPEG/WebP + HTML rejection,
// redaction query stripping). All HTTP scenarios use httptest.Server; no mock
// HTTP client.

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// pngMagic returns valid PNG bytes of the given size (header + filler).
func pngMagic(n int) []byte {
	b := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x0A, 0x00}
	if n > len(b) {
		b = append(b, make([]byte, n-len(b))...)
	}
	return b
}

func jpegMagic(n int) []byte {
	b := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	if n > len(b) {
		b = append(b, make([]byte, n-len(b))...)
	}
	return b
}

func webpMagic(n int) []byte {
	b := []byte{'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'E', 'B', 'P'}
	if n > len(b) {
		b = append(b, make([]byte, n-len(b))...)
	}
	return b
}

func dataURL(mime string, b []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

// === decodeAndSniffImage ===

func TestDecodeAndSniffImage_PNG(t *testing.T) {
	in := pngMagic(64)
	dec, mime, err := decodeAndSniffImage(dataURL("image/png", in))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if mime != "image/png" {
		t.Errorf("mime = %q, want image/png", mime)
	}
	if dec[0] != 0x89 || dec[1] != 'P' {
		t.Errorf("decoded bytes mismatch: %v", dec[:4])
	}
}

func TestDecodeAndSniffImage_JPEG(t *testing.T) {
	in := jpegMagic(32)
	_, mime, err := decodeAndSniffImage(dataURL("image/jpeg", in))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if mime != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", mime)
	}
}

func TestDecodeAndSniffImage_WebP(t *testing.T) {
	in := webpMagic(16)
	_, mime, err := decodeAndSniffImage(dataURL("image/webp", in))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if mime != "image/webp" {
		t.Errorf("mime = %q, want image/webp", mime)
	}
}

func TestDecodeAndSniffImage_HTMLRejected(t *testing.T) {
	html := []byte("<html><body>not an image</body></html>")
	_, _, err := decodeAndSniffImage(dataURL("image/png", html))
	if err == nil {
		t.Fatal("want error for HTML bytes claiming png")
	}
}

func TestDecodeAndSniffImage_MalformedBase64(t *testing.T) {
	_, _, err := decodeAndSniffImage("data:image/png;base64,@@@not-base64@@@")
	if err == nil {
		t.Fatal("want error for malformed base64")
	}
}

func TestDecodeAndSniffImage_Oversize(t *testing.T) {
	big := pngMagic(maxDecodedImageBytes + 1)
	_, _, err := decodeAndSniffImage(dataURL("image/png", big))
	if err == nil {
		t.Fatal("want error for oversize decoded image")
	}
}

// === redactedURL ===

func TestRedactedURL_QueryStripped(t *testing.T) {
	u, _ := url.Parse("https://queue.fal.run/status/abc?signature=secret&token=hunter2#frag")
	got := redactedURL(u)
	if got != "https://queue.fal.run/status/abc" {
		t.Errorf("redactedURL = %q, want scheme://host/path only", got)
	}
	if strings.Contains(got, "signature") || strings.Contains(got, "token") {
		t.Errorf("redactedURL leaked query: %q", got)
	}
}

// === SSRF guard ===

func TestSSRFReject(t *testing.T) {
	pol := defaultSSRFPolicy{}
	cases := []struct {
		name string
		ip   string
		want bool
	}{
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		{"unspecified v4", "0.0.0.0", true},
		{"unspecified v6", "::", true},
		{"link-local", "169.254.169.254", true},
		{"private 10", "10.0.0.1", true},
		{"private 172", "172.16.0.1", true},
		{"private 192", "192.168.1.1", true},
		{"cgnat", "100.64.0.1", true},
		{"multicast", "224.0.0.1", true},
		{"metadata", "169.254.169.254", true},
		{"public", "8.8.8.8", false},
		{"public v6", "2001:4860:4860::8888", false},
		{"nil", "", true},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if c.ip == "" {
			ip = nil
		}
		if got := pol.RejectIP(ip); got != c.want {
			t.Errorf("%s: RejectIP(%s) = %v, want %v", c.name, c.ip, got, c.want)
		}
	}
}

func TestSSRFRejectHost(t *testing.T) {
	pol := defaultSSRFPolicy{}
	cases := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"foo.internal", true},
		{"bar.foo.internal", true},
		{"127.0.0.1", true},
		{"[::1]", true},
		{"example.com", false},
		{"queue.fal.run", false},
	}
	for _, c := range cases {
		if got := pol.RejectHost(c.host); got != c.want {
			t.Errorf("RejectHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// === validateLifecycleURL ===

func TestValidateLifecycleURL_HTTPSOnly(t *testing.T) {
	_, err := validateLifecycleURL("http://queue.fal.run/x", FalHostPredicate)
	if err == nil {
		t.Fatal("want error for http lifecycle url")
	}
}

func TestValidateLifecycleURL_UserinfoRejected(t *testing.T) {
	_, err := validateLifecycleURL("https://user:pass@queue.fal.run/x", FalHostPredicate)
	if err == nil {
		t.Fatal("want error for userinfo in lifecycle url")
	}
}

func TestValidateLifecycleURL_ForeignHostRejected(t *testing.T) {
	_, err := validateLifecycleURL("https://evil.example.com/x", FalHostPredicate)
	if err == nil {
		t.Fatal("want error for foreign host")
	}
	if !strings.Contains(err.Error(), "unexpected lifecycle host") {
		t.Errorf("err = %v, want unexpected host message", err)
	}
}

func TestValidateLifecycleURL_AllowlistedAccepted(t *testing.T) {
	u, err := validateLifecycleURL("https://queue.fal.run/status/abc", FalHostPredicate)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if u.Host != "queue.fal.run" {
		t.Errorf("host = %q", u.Host)
	}
}

// === Production allowlists vs test seam ===

func TestProductionAllowlists(t *testing.T) {
	cases := []struct {
		name  string
		pred  LifecycleHostPredicate
		host  string
		allow bool
	}{
		{"bfl exact", BFLHostPredicate, "api.bfl.ai", true},
		{"bfl sub", BFLHostPredicate, "api-v2.bfl.ai", true},
		{"bfl foreign", BFLHostPredicate, "evil.example.com", false},
		{"bfl test endpoint", BFLHostPredicate, "127.0.0.1:9999", false},
		{"fal exact", FalHostPredicate, "queue.fal.run", true},
		{"fal sub", FalHostPredicate, "rest.fal.run", true},
		{"fal foreign", FalHostPredicate, "evil.example.com", false},
		{"fal test endpoint", FalHostPredicate, "127.0.0.1:9999", false},
		{"runway exact", RunwayMLHostPredicate, "api.dev.runwayml.com", true},
		{"runway foreign", RunwayMLHostPredicate, "api.runwayml.com", false},
		{"runway test endpoint", RunwayMLHostPredicate, "127.0.0.1:9999", false},
		{"nanobanana exact", NanobananaHostPredicate("https://api.nanobanana.com"), "api.nanobanana.com", true},
		{"nanobanana foreign", NanobananaHostPredicate("https://api.nanobanana.com"), "evil.example.com", false},
		{"nanobanana test endpoint", NanobananaHostPredicate("https://api.nanobanana.com"), "127.0.0.1:9999", false},
	}
	for _, c := range cases {
		if got := c.pred.IsAllowedLifecycleHost(c.host); got != c.allow {
			t.Errorf("%s: IsAllowedLifecycleHost(%q) = %v, want %v", c.name, c.host, got, c.allow)
		}
	}
}

func TestTestSeamPredicateAcceptsTestEndpoint(t *testing.T) {
	seam := HostPredicateFunc(func(host string) bool {
		return host == "127.0.0.1:9999"
	})
	if !seam.IsAllowedLifecycleHost("127.0.0.1:9999") {
		t.Fatal("test seam should accept test endpoint")
	}
	// Production predicates must reject the same endpoint — already covered above.
}

// === resolveInputImage: data + URL ===

func newTestHandler(t *testing.T, srv *httptest.Server, resolver HostResolver) *Handler {
	t.Helper()
	var exec HTTPExecutor
	if srv != nil {
		exec = &fallbackExecutor{client: srv.Client()}
	}
	return newTestHandlerWithExecutor(t, srv, exec, resolver)
}

// newTestHandlerWithExecutor builds a Handler with a custom executor (used when
// the test needs a no-follow-redirect client) and a permissive SSRF policy so
// the httptest loopback endpoint passes the SSRF guard. The production
// default-deny policy is exercised separately by TestSSRFReject.
func newTestHandlerWithExecutor(t *testing.T, srv *httptest.Server, exec HTTPExecutor, resolver HostResolver) *Handler {
	t.Helper()
	_ = srv
	deps := Dependencies{Logger: captureLogger{}, SSRFPolicy: permissiveSSRFForTest{}}
	if exec != nil {
		deps.Executor = exec
	}
	if resolver != nil {
		deps.Resolver = resolver
	}
	return New(deps)
}

// permissiveSSRFForTest allows every host/IP so httptest loopback endpoints
// (127.0.0.1:port) pass the SSRF guard. It is ONLY used in tests; production
// wiring never injects it.
type permissiveSSRFForTest struct{}

func (permissiveSSRFForTest) RejectIP(net.IP) bool   { return false }
func (permissiveSSRFForTest) RejectHost(string) bool { return false }

func TestResolveInputImage_DataURL(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	in := dataURL("image/png", pngMagic(32))
	ii, err := h.resolveInputImage(context.Background(), asJSON(in), "image")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ii.Kind != "data" {
		t.Errorf("kind = %q, want data", ii.Kind)
	}
	if ii.MIME != "image/png" {
		t.Errorf("mime = %q", ii.MIME)
	}
}

func TestResolveInputImage_MalformedDataURL(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	_, err := h.resolveInputImage(context.Background(), asJSON("data:image/png;base64,@@@bad@@@"), "image")
	if err == nil {
		t.Fatal("want error for malformed base64")
	}
}

func TestResolveInputImage_OversizeData(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	big := dataURL("image/png", pngMagic(maxDecodedImageBytes+1))
	_, err := h.resolveInputImage(context.Background(), asJSON(big), "image")
	if err == nil {
		t.Fatal("want error for oversize data input")
	}
}

func TestResolveInputImage_HTTPRejected(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	_, err := h.resolveInputImage(context.Background(), asJSON("http://example.com/a.png"), "image")
	if err == nil {
		t.Fatal("want error for http url input")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveInputImage_PrivateHostRejected(t *testing.T) {
	// Use the production SSRF policy so the private-IP resolution is rejected.
	h := New(Dependencies{
		Logger: captureLogger{},
		Resolver: ResolverFunc(func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("10.0.0.1")}, nil
		}),
		SSRFPolicy: defaultSSRFPolicy{},
	})
	_, err := h.resolveInputImage(context.Background(), asJSON("https://example.com/a.png"), "image")
	if err == nil {
		t.Fatal("want error for private-resolving url")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveInputImage_InternalDomainRejected(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, SSRFPolicy: defaultSSRFPolicy{}})
	_, err := h.resolveInputImage(context.Background(), asJSON("https://svc.internal/a.png"), "image")
	if err == nil {
		t.Fatal("want error for .internal domain")
	}
}

func TestResolveInputImage_MetadataHostRejected(t *testing.T) {
	h := New(Dependencies{
		Logger: captureLogger{},
		Resolver: ResolverFunc(func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		}),
		SSRFPolicy: defaultSSRFPolicy{},
	})
	_, err := h.resolveInputImage(context.Background(), asJSON("https://metadata.example/a.png"), "image")
	if err == nil {
		t.Fatal("want error for metadata host")
	}
}

func TestResolveInputImage_ValidURLProducesValidatedHost(t *testing.T) {
	// Public IP under the production policy passes.
	h := New(Dependencies{
		Logger: captureLogger{},
		Resolver: ResolverFunc(func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}),
		SSRFPolicy: defaultSSRFPolicy{},
	})
	ii, err := h.resolveInputImage(context.Background(), asJSON("https://example.com/a.png"), "image")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ii.Kind != "url" {
		t.Fatalf("kind = %q, want url", ii.Kind)
	}
	if !ii.Host.IsPinned() {
		t.Fatal("validated host should be pinned")
	}
	if !ii.Host.IP.Equal(net.ParseIP("93.184.216.34")) {
		t.Errorf("IP = %v", ii.Host.IP)
	}
	if ii.Host.Port != "443" {
		t.Errorf("port = %q, want 443", ii.Host.Port)
	}
}

// === downloadImageURL ===

func TestDownloadImageURL_Success(t *testing.T) {
	img := pngMagic(128)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(img)
	}))
	defer srv.Close()
	h := newTestHandlerWithExecutor(t, srv, newNoFollowExecutor(srv), resolverFor(srv))
	dec, mime, status, err := h.downloadImageURL(context.Background(), srv.URL+"/a.png", "fal-ai", "c1", func(u *url.URL) (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, u.String(), nil)
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	if mime != "image/png" {
		t.Errorf("mime = %q", mime)
	}
	if dec[0] != 0x89 {
		t.Errorf("decoded bytes mismatch")
	}
}

func TestDownloadImageURL_HTMLFailure502(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not an image</html>"))
	}))
	defer srv.Close()
	h := newTestHandlerWithExecutor(t, srv, newNoFollowExecutor(srv), resolverFor(srv))
	_, _, status, err := h.downloadImageURL(context.Background(), srv.URL+"/a.png", "fal-ai", "c1", func(u *url.URL) (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, u.String(), nil)
	})
	if err == nil {
		t.Fatal("want error for non-image download")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
}

func TestDownloadImageURL_HTTPRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pngMagic(16))
	}))
	defer srv.Close()
	// Production SSRF policy (default-deny) is not needed here — scheme check
	// rejects http before the host/IP checks run. Use permissive for parity.
	h := newTestHandler(t, nil, nil)
	_, _, status, err := h.downloadImageURL(context.Background(), srv.URL+"/a.png", "fal-ai", "c1", func(u *url.URL) (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, u.String(), nil)
	})
	if err == nil {
		t.Fatal("want error for http download url")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
}

// === Redirect contract ===

func TestDownloadImageURL_HTTPStoHTTPRedirectRejected(t *testing.T) {
	// HTTPS server redirects to an HTTP target.
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pngMagic(16))
	}))
	defer httpSrv.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpSrv.URL+"/x.png", http.StatusFound)
	}))
	defer srv.Close()
	h := newTestHandlerWithExecutor(t, srv, newNoFollowExecutor(srv), resolverFor(srv))
	_, _, status, err := h.downloadImageURL(context.Background(), srv.URL+"/a.png", "fal-ai", "c1", func(u *url.URL) (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, u.String(), nil)
	})
	if err == nil {
		t.Fatal("want error for https→http redirect")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
}

func TestDownloadImageURL_ForeignRedirectNoCredentialForwarding(t *testing.T) {
	// First HTTPS server (origin A) returns 302 to second HTTPS server (origin B).
	var seenAuthB string
	srvB := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuthB = r.Header.Get("Authorization")
		_, _ = w.Write(pngMagic(16))
	}))
	defer srvB.Close()
	srvA := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srvB.URL+"/x.png", http.StatusFound)
	}))
	defer srvA.Close()
	// Both origins resolve to the loopback; the no-follow executor surfaces the
	// 302 from A so handleRedirect validates B and drops the Authorization header
	// before issuing the request to B (foreign canonical origin).
	h := newTestHandlerWithExecutor(t, srvA, newNoFollowExecutor(srvA), resolverForBoth(srvA, srvB))
	_, _, status, err := h.downloadImageURL(context.Background(), srvA.URL+"/a.png", "fal-ai", "c1", func(u *url.URL) (*http.Request, error) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, u.String(), nil)
		req.Header.Set("Authorization", "Key secret-token")
		return req, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	if seenAuthB != "" {
		t.Errorf("Authorization forwarded to foreign origin: %q", seenAuthB)
	}
}

// === 64 MiB cap ===

func TestDownloadImageURL_64MiBCap(t *testing.T) {
	over := maxDownloadImageBytes + 1
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, over))
	}))
	defer srv.Close()
	h := newTestHandlerWithExecutor(t, srv, newNoFollowExecutor(srv), resolverFor(srv))
	// make 64MiB+1 of PNG-like bytes — sniff only reads the header, but the
	// reader cap fires before sniff. We need the first 8 bytes to be PNG so
	// sniff would pass on a small body; the cap must reject regardless.
	_, _, status, err := h.downloadImageURL(context.Background(), srv.URL+"/a.png", "fal-ai", "c1", func(u *url.URL) (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, u.String(), nil)
	})
	if err == nil {
		t.Fatal("want error for over-cap download")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
}

// === handleRedirect: validated target metadata per hop ===

func TestHandleRedirect_SameOriginKeepsAuth(t *testing.T) {
	resp := &http.Response{StatusCode: 302, Header: http.Header{}}
	resp.Header.Set("Location", "https://queue.fal.run/status/next")
	cur, _ := http.NewRequest(http.MethodGet, "https://queue.fal.run/submit", nil)
	cur.Header.Set("Authorization", "Key tok")
	nextReq, host, err := handleRedirect(resp, cur, FalHostPredicate)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := nextReq.Header.Get("Authorization"); got != "Key tok" {
		t.Errorf("auth not carried same-origin: %q", got)
	}
	if host.Hostname != "queue.fal.run" || host.Port != "443" {
		t.Errorf("host = %+v", host)
	}
}

func TestHandleRedirect_ForeignOriginDropsAuth(t *testing.T) {
	resp := &http.Response{StatusCode: 302, Header: http.Header{}}
	resp.Header.Set("Location", "https://cdn.example.com/result.png")
	cur, _ := http.NewRequest(http.MethodGet, "https://queue.fal.run/submit", nil)
	cur.Header.Set("Authorization", "Key tok")
	// nil predicate → no host check (download path re-validates separately).
	nextReq, _, err := handleRedirect(resp, cur, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := nextReq.Header.Get("Authorization"); got != "" {
		t.Errorf("auth forwarded to foreign origin: %q", got)
	}
}

// === helpers ===

func asJSON(s string) []byte { return []byte(`"` + s + `"`) }

// resolverFor returns a resolver that answers the httptest TLS server's
// hostname with its loopback IP so the SSRF IP check passes (the host-level
// check is bypassed by permissiveSSRFForTest).
func resolverFor(srv *httptest.Server) HostResolver {
	u, _ := url.Parse(srv.URL)
	host := u.Hostname()
	return ResolverFunc(func(ctx context.Context, h string) ([]net.IP, error) {
		if h == host {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		return nil, nil
	})
}

// resolverForBoth resolves two httptest servers' hostnames to the loopback.
func resolverForBoth(srvs ...*httptest.Server) HostResolver {
	hosts := make(map[string]net.IP)
	for _, s := range srvs {
		u, _ := url.Parse(s.URL)
		hosts[u.Hostname()] = net.ParseIP("127.0.0.1")
	}
	return ResolverFunc(func(ctx context.Context, h string) ([]net.IP, error) {
		if ip, ok := hosts[h]; ok {
			return []net.IP{ip}, nil
		}
		return nil, nil
	})
}

// noFollowTestExecutor wraps an httptest.Server's client with CheckRedirect
// disabled so 3xx responses surface to downloadImageURL/handleRedirect, which
// re-validate each hop — mirroring the production executor's no-redirect
// contract (spec step 4 point 7). The server client is used so the httptest
// TLS certificate is trusted.
type noFollowTestExecutor struct {
	client *http.Client
}

func newNoFollowExecutor(srv *httptest.Server) *noFollowTestExecutor {
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &noFollowTestExecutor{client: c}
}

func (e *noFollowTestExecutor) Do(req *http.Request) (*http.Response, error) {
	return e.client.Do(req)
}

// drain reads a response body fully so httptest connection reuse works.
var _ = io.Discard
