package imageproxy

// This file holds the safe image-input primitives shared by every image
// provider adapter (steps 5–7): SSRF guard, lifecycle URL validation,
// provider host allowlists, magic-byte sniff, URL→binary download, redirect
// contract, and the canonical image/mask resolver. None of these functions
// import the proxy package, repositories, or perform real DNS — DNS resolution
// and actual egress pinning live in the injectable resolver seam and the
// production executor (wire.go) respectively. imageproxy only builds the
// ValidatedHost contract the executor consumes.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// maxDecodedImageBytes is the decoded data-URL size limit (16 MiB) per spec.
const maxDecodedImageBytes = 16 << 20

// maxDownloadImageBytes is the binary-download size cap (64 MiB) per spec.
const maxDownloadImageBytes = 64 << 20

// acceptedDataMIMEs is the set of data-URL media types the resolver accepts.
var acceptedDataMIMEs = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
}

// === Resolver seam ===

// HostResolver resolves a hostname to one or more IP addresses. The production
// resolver (wired in app/wire.go) performs real DNS via net.LookupIP; the
// default resolver in imageproxy is an injectable seam so the usecase never
// performs network I/O itself. The SSRF guard runs against every returned
// address; a host that resolves to any disallowed address is rejected before
// a ValidatedHost is built.
type HostResolver interface {
	LookupHost(ctx context.Context, host string) ([]net.IP, error)
}

// ResolverFunc adapts a function to HostResolver.
type ResolverFunc func(ctx context.Context, host string) ([]net.IP, error)

func (f ResolverFunc) LookupHost(ctx context.Context, host string) ([]net.IP, error) {
	return f(ctx, host)
}

// errResolverUnset is returned by the default no-op resolver. The production
// wiring replaces Dependencies.Resolver with a real resolver; a nil resolver
// means SSRF/url-resolution is unavailable and the request fails closed.
var errResolverUnset = errors.New("image host resolver not configured")

// noopResolver is the default HostResolver; it fails closed. The production
// wiring in wire.go substitutes a net.LookupIP-based resolver.
type noopResolver struct{}

func (noopResolver) LookupHost(context.Context, string) ([]net.IP, error) {
	return nil, errResolverUnset
}

// === SSRF guard ===

// SSRFPolicy decides whether an IP or hostname is disallowed for untrusted
// egress. The production policy (defaultSSRFPolicy) is default-deny: loopback,
// unspecified, link-local (incl. cloud metadata 169.254.169.254), RFC1918
// private, CGNAT 100.64.0.0/10, multicast, the explicit metadata host, and
// .internal domains are all rejected. Tests inject a permissive policy so an
// httptest loopback endpoint can exercise the download/redirect path — the
// production policy is never weakened (spec: "test override upstream origin
// допускается только через injected endpoint policy in tests; production
// allowlist не ослабляется").
type SSRFPolicy interface {
	// RejectIP reports whether the resolved IP is forbidden.
	RejectIP(ip net.IP) bool
	// RejectHost reports whether the textual hostname is forbidden before
	// DNS resolution (catches "localhost", ".internal", literal private IPs).
	RejectHost(host string) bool
}

// defaultSSRFPolicy is the production default-deny SSRF policy.
type defaultSSRFPolicy struct{}

func (defaultSSRFPolicy) RejectIP(ip net.IP) bool     { return ssrfRejectIP(ip) }
func (defaultSSRFPolicy) RejectHost(host string) bool { return ssrfRejectHostText(host) }

// ssrfRejectIP reports whether an IP is disallowed for untrusted egress.
func ssrfRejectIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsPrivate() {
		return true
	}
	// CGNAT 100.64.0.0/10.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	// Explicit cloud-metadata host (covered by link-local, but defended
	// separately in case the link-local block is ever loosened).
	if md := ip.To4(); md != nil && md[0] == 169 && md[1] == 254 && md[2] == 169 && md[3] == 254 {
		return true
	}
	return false
}

// ssrfRejectHostText rejects a hostname whose TLD is ".internal" (spec: reject
// .internal domains) OR that is a bare loopback literal. Hostname-level checks
// run before DNS resolution so a rebinding host that points at a private IP is
// still rejected at the IP layer; this catches the obvious textual cases
// cheaply.
func ssrfRejectHostText(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" {
		return true
	}
	if strings.HasSuffix(h, ".internal") {
		return true
	}
	// Literal IP host — delegate to ssrfRejectIP after parsing.
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ssrfRejectIP(ip)
	}
	return false
}

// ssrfRejectHost is the package-level helper used by validateDownloadURL /
// resolveInputImage. It routes through the production policy by default; tests
// that inject a permissive SSRFPolicy via Dependencies bypass it through the
// Handler method (h.deps.SSRFPolicy).
func ssrfRejectHost(host string) bool { return defaultSSRFPolicy{}.RejectHost(host) }

// === Lifecycle URL validation ===

// LifecycleHostPredicate decides whether a host is an allowed destination for a
// given provider's lifecycle URLs (submit/poll/result/download). Production
// predicates are exact documented host allowlists (BFL, fal-ai, RunwayML,
// nanobanana). A test-seam predicate allows a test httptest endpoint so contract
// tests can exercise the polling path without weakening the production
// allowlists.
type LifecycleHostPredicate interface {
	// IsAllowedLifecycleHost reports whether host (the url.URL.Host, i.e. the
	// canonical "hostname:port" or "hostname") is a permitted lifecycle
	// destination for the provider.
	IsAllowedLifecycleHost(host string) bool
}

// HostPredicateFunc adapts a function to LifecycleHostPredicate.
type HostPredicateFunc func(host string) bool

func (f HostPredicateFunc) IsAllowedLifecycleHost(host string) bool { return f(host) }

// --- Production predicates ---

// BFLHostPredicate allows api.bfl.ai and any *.bfl.ai subdomain (spec).
var BFLHostPredicate = HostPredicateFunc(func(host string) bool {
	h := strings.ToLower(hostnameOnly(host))
	return h == "api.bfl.ai" || strings.HasSuffix(h, ".bfl.ai")
})

// FalHostPredicate allows queue.fal.run and any *.fal.run subdomain (spec).
var FalHostPredicate = HostPredicateFunc(func(host string) bool {
	h := strings.ToLower(hostnameOnly(host))
	return h == "queue.fal.run" || strings.HasSuffix(h, ".fal.run")
})

// RunwayMLHostPredicate allows exactly api.dev.runwayml.com (spec).
var RunwayMLHostPredicate = HostPredicateFunc(func(host string) bool {
	return strings.ToLower(hostnameOnly(host)) == "api.dev.runwayml.com"
})

// NanobananaHostPredicate allows the configured base host. The nanobanana
// provider does not have a fixed documented host; the operator configures the
// base URL. This predicate is built per-request from the provider base URL by
// the adapter (step 7); the exported constructor is provided here so tests and
// the adapter share one definition.
func NanobananaHostPredicate(baseURL string) LifecycleHostPredicate {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return HostPredicateFunc(func(string) bool { return false })
	}
	allowed := strings.ToLower(hostnameOnly(u.Host))
	return HostPredicateFunc(func(host string) bool {
		return strings.ToLower(hostnameOnly(host)) == allowed
	})
}

// hostnameOnly strips an explicit port from a "host:port" string. net/url.Host
// carries the port when present; the allowlist matches on the bare hostname.
func hostnameOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// effectivePort returns the explicit port from u.Host, or the scheme default.
func effectivePort(u *url.URL) string {
	if _, port, err := net.SplitHostPort(u.Host); err == nil && port != "" {
		return port
	}
	switch u.Scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// canonicalOrigin returns "https://hostname:port" (with port omitted when it is
// the scheme default) for the credential-forwarding same-origin check.
func canonicalOrigin(u *url.URL) string {
	if u == nil {
		return ""
	}
	host := u.Hostname()
	port := effectivePort(u)
	switch u.Scheme {
	case "https":
		if port == "443" {
			return "https://" + host
		}
	case "http":
		if port == "80" {
			return "http://" + host
		}
	}
	return u.Scheme + "://" + net.JoinHostPort(host, port)
}

// validateLifecycleURL enforces the spec contract for every lifecycle URL
// (submit, poll, result, download): HTTPS-only, no userinfo, host matches the
// provider predicate. It does NOT perform DNS — the SSRF guard runs separately
// for untrusted image input/download URLs (see resolveInputImage /
// downloadImageURL). For provider lifecycle URLs (submit/poll/result) the
// allowlist is the trust boundary; for untrusted image URLs the SSRF guard +
// allowlist both apply.
func validateLifecycleURL(raw string, predicate LifecycleHostPredicate) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("empty lifecycle url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid lifecycle url: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("lifecycle url must be https, got %q", u.Scheme)
	}
	if u.User != nil {
		return nil, errors.New("lifecycle url must not carry userinfo")
	}
	if u.Host == "" {
		return nil, errors.New("lifecycle url missing host")
	}
	if predicate != nil && !predicate.IsAllowedLifecycleHost(u.Host) {
		return nil, fmt.Errorf("provider returned unexpected lifecycle host: %s", u.Host)
	}
	return u, nil
}

// === Redirect contract ===

// handleRedirect inspects a 3xx response from a lifecycle call, validates the
// Location through validateLifecycleURL + SSRF guard (when the URL resolves to
// an address), and builds the next request. When the redirect changes the
// canonical origin the new request is built WITHOUT auth headers (credentials
// are not forwarded to a different host). When the redirect stays on the same
// canonical origin the auth headers are copied. A redirect that fails
// validation (HTTP target, forbidden host, userinfo, SSRF-rejected address)
// returns an error; the caller surfaces it as 502.
//
// buildAuth is called on a same-origin redirect to re-attach credentials (the
// caller knows how to build the auth header for its provider). It is NOT
// called on a foreign-origin redirect. When buildAuth is nil, no auth headers
// are carried forward even on same-origin redirects (the caller will set them
// itself if needed).
//
// resolvedIP, when non-nil, is the SSRF-validated IP for the new target; it is
// attached to the returned request via WithTransportMetadata by the caller's
// h.do path. handleRedirect itself does not attach metadata — it returns a
// fresh *http.Request and the ValidatedHost for the next hop so the caller can
// pass it to h.do.
func handleRedirect(resp *http.Response, currentReq *http.Request, predicate LifecycleHostPredicate) (*http.Request, ValidatedHost, error) {
	if resp == nil {
		return nil, ValidatedHost{}, errors.New("nil redirect response")
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return nil, ValidatedHost{}, errors.New("redirect response missing Location")
	}
	u, err := validateLifecycleURL(loc, predicate)
	if err != nil {
		return nil, ValidatedHost{}, fmt.Errorf("redirect target rejected: %w", err)
	}
	// Build the next request. Method and body are NOT auto-carried for 3xx per
	// the executor-level redirect contract: the adapter rebuilds the request
	// via its factory for poll/result/download. For a submit redirect, a simple
	// GET on the Location is the legacy behaviour (BFL/fal return the poll URL
	// via Location or a body field; the adapter decides). Here we produce a
	// GET to the validated Location; callers that need POST-redirect build it
	// themselves and call validateLifecycleURL directly.
	nextReq, err := http.NewRequestWithContext(currentReq.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ValidatedHost{}, err
	}
	// Same-origin keeps credentials; foreign origin drops them.
	if canonicalOrigin(currentReq.URL) == canonicalOrigin(u) {
		// Copy auth-bearing headers the caller set on the current request.
		for _, h := range []string{"Authorization", "x-key", "X-Runway-Version", "chatgpt-account-id"} {
			if v := currentReq.Header.Get(h); v != "" {
				nextReq.Header.Set(h, v)
			}
		}
	}
	host := ValidatedHost{Scheme: u.Scheme, Hostname: u.Hostname(), Port: effectivePort(u)}
	return nextReq, host, nil
}

// === Magic-byte sniff ===

// decodeAndSniffImage decodes a base64 data-URL payload (or raw base64 bytes)
// and returns the decoded bytes plus the authoritative MIME sniffed from the
// magic bytes. The claimed data-URL media type is NOT trusted; MIME is derived
// solely from the magic bytes. Non-image or malformed base64 returns an error
// the caller surfaces as 502 (spec: "non-image or malformed base64 → 502").
//
// `input` is either:
//   - a full data URL "data:image/png;base64,...",
//   - a bare base64 string (Gemini/Codex inline data),
//   - raw bytes (already decoded) — caller passes them via decodeAndSniffBytes.
//
// The decoded size is capped at maxDecodedImageBytes.
func decodeAndSniffImage(dataURL string) ([]byte, string, error) {
	b64, claimed, err := splitDataURL(dataURL)
	if err != nil {
		return nil, "", err
	}
	_ = claimed // claimed MIME is intentionally ignored; magic bytes are authoritative.
	dec, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		dec, err = base64.URLEncoding.DecodeString(b64)
		if err != nil {
			dec, err = base64.RawStdEncoding.DecodeString(b64)
			if err != nil {
				return nil, "", fmt.Errorf("malformed base64 image: %w", err)
			}
		}
	}
	return sniffImage(dec)
}

// decodeAndSniffBytes sniffs already-decoded raw bytes (used by downloadImageURL
// and the b64_json binary branch in toBinary).
func decodeAndSniffBytes(b []byte) ([]byte, string, error) {
	return sniffImage(b)
}

// sniffImage returns the bytes and the authoritative MIME derived from the
// magic bytes. Recognised: PNG (\x89PNG\r\n), JPEG (\xff\xd8\xff), WebP
// (RIFF....WEBP). Anything else → ErrNotImage (502).
func sniffImage(b []byte) ([]byte, string, error) {
	if len(b) > maxDecodedImageBytes {
		return nil, "", fmt.Errorf("decoded image exceeds %d bytes", maxDecodedImageBytes)
	}
	switch {
	case len(b) >= 8 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G' && b[4] == 0x0D && b[5] == 0x0A:
		return b, "image/png", nil
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return b, "image/jpeg", nil
	case len(b) >= 12 && b[0] == 'R' && b[1] == 'I' && b[2] == 'F' && b[3] == 'F' && b[8] == 'W' && b[9] == 'E' && b[10] == 'B' && b[11] == 'P':
		return b, "image/webp", nil
	}
	return nil, "", ErrNotImage
}

// ErrNotImage is returned when sniffImage cannot identify the bytes as
// PNG/JPEG/WebP. Callers map it to 502.
var ErrNotImage = errors.New("not a recognised image (png/jpeg/webp)")

// splitDataURL parses a data URL "data:<mime>;base64,<payload>" and returns the
// base64 payload and the claimed (untrusted) MIME. A bare base64 string is
// accepted with an empty claimed MIME.
func splitDataURL(s string) (b64, claimedMIME string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", errors.New("empty image input")
	}
	if !strings.HasPrefix(s, "data:") {
		// Bare base64 — accept it.
		return s, "", nil
	}
	rest := strings.TrimPrefix(s, "data:")
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", errors.New("malformed data url: missing comma")
	}
	meta := rest[:comma]
	b64 = rest[comma+1:]
	// meta is "<mime>;base64" or "<mime>;base64" with optional parameters.
	if sem := strings.IndexByte(meta, ';'); sem >= 0 {
		claimedMIME = meta[:sem]
	} else {
		claimedMIME = meta
	}
	if !strings.Contains(meta, "base64") {
		return "", "", errors.New("data url must be base64-encoded")
	}
	if claimedMIME != "" && !acceptedDataMIMEs[strings.ToLower(claimedMIME)] {
		// Reject claimed types outside the accepted set up front; magic bytes
		// remain the final authority for the returned MIME.
		return "", "", fmt.Errorf("data url claims unsupported media type %q", claimedMIME)
	}
	return b64, claimedMIME, nil
}

// === Canonical image/mask resolver ===

// resolveInputImage converts one canonical image or mask input (the result of
// the handler's alias canonicalization) into a typed ImageInput. It accepts:
//   - a data URL "data:image/<png|jpeg|webp>;base64,..." (decoded ≤16 MiB),
//   - a bare base64 string (treated as a data URL without the prefix),
//   - an HTTPS URL (resolved through the injectable resolver, SSRF-guarded,
//     producing a pinned ValidatedHost the production executor dials).
//
// The resolver enforces:
//   - magic-byte sniff for data inputs (MIME from bytes, not the claim),
//   - cardinality (one input per call),
//   - SSRF guard for URL inputs (loopback/private/CGNAT/metadata/.internal
//     rejected before any HTTP call),
//   - HTTPS-only + no userinfo for URL inputs.
//
// imageproxy does NOT perform the HTTP fetch for URL inputs here — the adapter
// fetches the image as part of building the upstream request (image_b64 /
// image_url). The resolver only validates and, for URLs, builds the
// ValidatedHost the adapter attaches to the fetch via h.do so the production
// executor pins the dial. This keeps imageproxy free of the proxy package.
func (h *Handler) resolveInputImage(ctx context.Context, raw json.RawMessage, kind string) (ImageInput, error) {
	if len(raw) == 0 {
		return ImageInput{}, fmt.Errorf("missing %s input", kind)
	}
	// Trim a JSON string wrapper (image inputs arrive as a string field).
	s := strings.Trim(string(raw), `"`)
	if s == "" || s == "null" {
		return ImageInput{}, fmt.Errorf("missing %s input", kind)
	}
	if strings.HasPrefix(s, "data:") {
		dec, mime, err := decodeAndSniffImage(s)
		if err != nil {
			return ImageInput{}, fmt.Errorf("%s: %w", kind, err)
		}
		return ImageInput{Kind: "data", B64: base64.StdEncoding.EncodeToString(dec), MIME: mime}, nil
	}
	// Try base64 (bare) — if it decodes to image bytes, treat as data.
	if dec, mime, err := decodeAndSniffImage(s); err == nil {
		return ImageInput{Kind: "data", B64: base64.StdEncoding.EncodeToString(dec), MIME: mime}, nil
	}
	// Otherwise treat as a URL.
	u, err := url.Parse(s)
	if err != nil {
		return ImageInput{}, fmt.Errorf("%s: invalid url: %w", kind, err)
	}
	if u.Scheme != "https" {
		return ImageInput{}, fmt.Errorf("%s url must be https, got %q", kind, u.Scheme)
	}
	if u.User != nil {
		return ImageInput{}, fmt.Errorf("%s url must not carry userinfo", kind)
	}
	if h.deps.SSRFPolicy.RejectHost(u.Hostname()) {
		return ImageInput{}, fmt.Errorf("%s url host rejected by SSRF policy", kind)
	}
	resolver := h.deps.Resolver
	if resolver == nil {
		resolver = noopResolver{}
	}
	ips, err := resolver.LookupHost(ctx, u.Hostname())
	if err != nil {
		return ImageInput{}, fmt.Errorf("%s: resolve host: %w", kind, err)
	}
	for _, ip := range ips {
		if h.deps.SSRFPolicy.RejectIP(ip) {
			return ImageInput{}, fmt.Errorf("%s url resolves to forbidden address", kind)
		}
	}
	if len(ips) == 0 {
		return ImageInput{}, fmt.Errorf("%s: host resolved to no addresses", kind)
	}
	host := ValidatedHost{
		Scheme:   u.Scheme,
		Hostname: u.Hostname(),
		Port:     effectivePort(u),
		IP:       ips[0],
	}
	return ImageInput{Kind: "url", URL: u.String(), MIME: "", Host: host}, nil
}

// === URL → binary download ===

// downloadImageURL fetches an image result URL for response_format=binary. It
// enforces: HTTPS-only, no userinfo, SSRF guard (loopback/unspecified/link-
// local/private/CGNAT/multicast/metadata rejected), 64 MiB cap, the executor
// redirect contract (each 3xx surfaced without auto-follow, re-validated,
// credentials not forwarded to a foreign origin), and magic-byte sniff. It
// returns the decoded image bytes + authoritative MIME.
//
// buildReq is a factory the adapter supplies to build the initial GET request
// for the validated URL. It decides whether to attach auth headers: same-origin
// with the submit call attaches them, foreign origin does not. The factory
// receives the already-validated *url.URL (after scheme/userinfo/SSRF checks).
// Each redirect hop is re-validated and the request rebuilt; auth headers are
// only carried forward when the next hop shares the canonical origin with the
// current request.
//
// The fetch goes through h.do so the production executor (wire.go) receives the
// ValidatedHost and pins the dial. connID selects the submit connection's
// proxy settings.
func (h *Handler) downloadImageURL(ctx context.Context, imageURL string, provider, connID string, buildReq func(u *url.URL) (*http.Request, error)) ([]byte, string, int, error) {
	u, err := url.Parse(imageURL)
	if err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("download url parse: %w", err)
	}
	if err := h.validateDownloadURL(u); err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	host, err := h.resolveValidatedHost(ctx, u)
	if err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	req, err := buildReq(u)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	// Follow up to 8 redirect hops, re-validating each.
	const maxHops = 8
	for i := 0; i < maxHops; i++ {
		resp, ferr := h.do(ctx, req, provider, "output", domainProv.Credentials{}, connID, host)
		if ferr != nil {
			return nil, "", http.StatusBadGateway, fmt.Errorf("download fetch: %w", ferr)
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			nextReq, _, rerr := handleRedirect(resp, req, nil)
			resp.Body.Close()
			if rerr != nil {
				return nil, "", http.StatusBadGateway, fmt.Errorf("download redirect: %w", rerr)
			}
			// Re-SSRF-check the redirect target's resolved address.
			nu := nextReq.URL
			if err := h.validateDownloadURL(nu); err != nil {
				return nil, "", http.StatusBadGateway, err
			}
			nh, rerr := h.resolveValidatedHost(ctx, nu)
			if rerr != nil {
				return nil, "", http.StatusBadGateway, rerr
			}
			req, host = nextReq, nh
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return nil, "", http.StatusBadGateway, fmt.Errorf("download upstream status %d", resp.StatusCode)
		}
		limited := io.LimitReader(resp.Body, maxDownloadImageBytes+1)
		body, rerr := io.ReadAll(limited)
		if rerr != nil {
			return nil, "", http.StatusBadGateway, fmt.Errorf("download read: %w", rerr)
		}
		if len(body) > maxDownloadImageBytes {
			return nil, "", http.StatusBadGateway, fmt.Errorf("download exceeds %d bytes", maxDownloadImageBytes)
		}
		dec, mime, serr := sniffImage(body)
		if serr != nil {
			return nil, "", http.StatusBadGateway, ErrDownloadFailed
		}
		return dec, mime, http.StatusOK, nil
	}
	return nil, "", http.StatusBadGateway, errors.New("download: too many redirects")
}

// validateDownloadURL enforces the URL-level guards for a binary-download URL:
// HTTPS-only, no userinfo, no SSRF-rejected hostname. It routes the host check
// through the injected SSRFPolicy so tests can authorise an httptest loopback
// endpoint without weakening the production default-deny policy.
func (h *Handler) validateDownloadURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("download url must be https, got %q", u.Scheme)
	}
	if u.User != nil {
		return errors.New("download url must not carry userinfo")
	}
	if u.Host == "" {
		return errors.New("download url missing host")
	}
	if h.deps.SSRFPolicy.RejectHost(u.Hostname()) {
		return errors.New("download url host rejected by SSRF policy")
	}
	return nil
}

// resolveValidatedHost resolves u.Hostname through the injectable resolver,
// SSRF-checks every returned address via the injected policy, and returns a
// pinned ValidatedHost.
func (h *Handler) resolveValidatedHost(ctx context.Context, u *url.URL) (ValidatedHost, error) {
	resolver := h.deps.Resolver
	if resolver == nil {
		resolver = noopResolver{}
	}
	ips, err := resolver.LookupHost(ctx, u.Hostname())
	if err != nil {
		return ValidatedHost{}, fmt.Errorf("resolve host: %w", err)
	}
	for _, ip := range ips {
		if h.deps.SSRFPolicy.RejectIP(ip) {
			return ValidatedHost{}, errors.New("url resolves to forbidden address")
		}
	}
	if len(ips) == 0 {
		return ValidatedHost{}, errors.New("host resolved to no addresses")
	}
	return ValidatedHost{Scheme: u.Scheme, Hostname: u.Hostname(), Port: effectivePort(u), IP: ips[0]}, nil
}

// ErrDownloadFailed is the 502-mapped error for a binary-download failure
// (non-image bytes, size cap, fetch error).
var ErrDownloadFailed = errors.New("image download failed")
