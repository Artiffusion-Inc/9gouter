package imageproxy

// providers_async.go implements the asynchronous image-provider adapters
// (step 7 of the image-provider-parity plan): fal-ai, black-forest-labs,
// runwayml, nanobanana. Each submits a generation request, polls a
// submit-derived URL for completion, and normalises the result into the
// OpenAI {created, data:[{url}]} shape.
//
// The shared polling lifecycle (poll.go) owns ONLY deadline/sleep/context
// mechanics. Each adapter owns:
//
//   - submit URL + body + auth headers (built from the provider static config),
//   - the submit-response parser (extracting the poll URL / task id),
//   - host validation of the poll/result URLs (validateLifecycleURL + the
//     provider's LifecycleHostPredicate — the production allowlist, overrideable
//     via Dependencies.LifecycleHostPredicates for tests),
//   - the poll request factory (rebuilds the GET for each attempt, attaches
//     auth headers only on the same canonical origin),
//   - the provider-local status parser (COMPLETED/Ready/SUCCEEDED/successFlag=1
//     → completed; FAILED/Error/CANCELLED/flag 2/3 → failed; else pending),
//   - the result normaliser.
//
// Invariants (spec):
//   - poll/result URLs are taken from the submit response, NEVER reconstructed
//     from model/base; each is validated through validateLifecycleURL + host
//     predicate before any HTTP call.
//   - credential headers are only carried to the same canonical origin
//     (https, hostname, effective port); a poll/result URL on a foreign host
//     is fetched WITHOUT credentials.
//   - fal-ai / BFL accept exactly one image input (resolveInputImage, SSRF
//     guard, 16 MiB cap); images/mask → 400 pre-executor.
//   - runwayml / nanobanana intentionally reject image/images/mask inputs with
//     400 pre-executor (edit pass-through excluded from this parity tranche —
//     Go transport does not re-host untrusted URLs).
//   - nanobanana requires a fixed dummy callBackUrl in its submit body (provider
//     contract); no callback listener is created. nanobanana uses a separate
//     PollURL (record-info) distinct from its submit BaseURL.
//   - typed errors: timeout → 504 (ErrPollTimeout), failed/malformed/unexpected
//     host/non-2xx → 502.
//
// imageproxy never imports the proxy/DB/repo packages — h.do hands each request
// to the injected HTTPExecutor with transport metadata.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/image"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// === Shared helpers ===

// lifecycleHostPredicate returns the production allowlist for providerID, or an
// injected test override from Dependencies.LifecycleHostPredicates. The
// production allowlists (BFLHostPredicate, FalHostPredicate,
// RunwayMLHostPredicate, NanobananaHostPredicate) remain the default trust
// boundary; tests substitute a permissive predicate so httptest loopback
// endpoints can exercise the poll path without weakening the production
// allowlists.
func (h *Handler) lifecycleHostPredicate(providerID, baseCfgURL string) LifecycleHostPredicate {
	if h.deps.LifecycleHostPredicates != nil {
		if p, ok := h.deps.LifecycleHostPredicates[providerID]; ok {
			return p
		}
	}
	switch providerID {
	case "fal-ai":
		return FalHostPredicate
	case "black-forest-labs":
		return BFLHostPredicate
	case "runwayml":
		return RunwayMLHostPredicate
	case "nanobanana":
		return NanobananaHostPredicate(baseCfgURL)
	}
	return HostPredicateFunc(func(string) bool { return false })
}

// validateSubmitDerivedURL validates a URL returned in a submit response for
// use as a poll/result endpoint. It enforces HTTPS-only, no userinfo, and the
// provider's host allowlist (or the injected test override).
func (h *Handler) validateSubmitDerivedURL(raw, providerID, baseCfgURL string) (*url.URL, error) {
	u, err := validateLifecycleURL(raw, h.lifecycleHostPredicate(providerID, baseCfgURL))
	if err != nil {
		return nil, err
	}
	return u, nil
}

// escapePathSegments validates a model as one or more URL path segments. It
// allows "/"-separated sub-segments (e.g. fal-ai "fal-ai/flux/schnell" or BFL
// "flux-1.1-pro") and escapes each via url.PathEscape. It rejects empty
// segments, traversal (".", ".."), query ("?") and fragment ("#"). The
// returned string joins the escaped segments with "/".
func escapePathSegments(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", errors.New("missing model")
	}
	if strings.ContainsAny(model, "?#") {
		return "", fmt.Errorf("model must not contain query or fragment: %q", model)
	}
	segs := strings.Split(model, "/")
	for _, s := range segs {
		s = strings.TrimSpace(s)
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("model contains invalid or traversal segment: %q", model)
		}
	}
	escaped := make([]string, len(segs))
	for i, s := range segs {
		escaped[i] = url.PathEscape(s)
	}
	return strings.Join(escaped, "/"), nil
}

// rejectEditInputs returns a 400 error when image/images/mask inputs are
// supplied to a provider whose image-edit path is intentionally excluded from
// this parity tranche (runwayml, nanobanana). The capability table in the HTTP
// handler has already rejected these fields, but the adapter re-checks so a
// direct synthX call cannot bypass it. Returns true when it rejected.
func rejectEditInputs(provider string, opts RequestOptions) ([]byte, string, int, error) {
	if len(opts.RawImageInputs) > 0 {
		return nil, "", http.StatusBadRequest, fmt.Errorf("%s does not support image for this image model", provider)
	}
	if len(opts.RawMask) > 0 {
		return nil, "", http.StatusBadRequest, fmt.Errorf("%s does not support mask for this image model", provider)
	}
	return nil, "", 0, nil
}

// singleImageInput resolves exactly one image input (the `image` field, not
// `images`) through the safe input resolver and returns it. It enforces
// cardinality: images (array) and mask are rejected (these providers accept
// exactly one image input, not an array or a mask).
func (h *Handler) singleImageInput(ctx context.Context, opts RequestOptions, provider string) (ImageInput, error) {
	if len(opts.RawImageInputs) == 0 {
		return ImageInput{}, nil
	}
	// The capability table authorises image and images for fal-ai/BFL. We accept
	// exactly one image input; an `images` array with more than one value is a
	// cardinality violation.
	// Options.RawImageInputs collects both `image` and `images` raw values; a
	// single string input produces one entry. We reject when more than one raw
	// input is present (would mean both image and images, or an images array).
	if len(opts.RawImageInputs) > 1 {
		return ImageInput{}, fmt.Errorf("%s accepts at most one image input", provider)
	}
	in, err := h.resolveInputImage(ctx, opts.RawImageInputs[0], "image")
	if err != nil {
		return ImageInput{}, err
	}
	// Mask is not supported on these providers (capability table already
	// rejects it; re-check here for direct synth calls).
	if len(opts.RawMask) > 0 {
		return ImageInput{}, fmt.Errorf("%s does not support mask for this image model", provider)
	}
	return in, nil
}

// doSubmit builds and dispatches a POST submit request with the given JSON body
// and auth headers, then reads the full response body. It returns the status
// code, the body bytes, and any transport error (mapped to 502 by the caller).
// On a 4xx/5xx response it returns the upstream error (the caller surfaces the
// upstream status). It is the shared submit helper for all async providers.
func (h *Handler) doSubmit(ctx context.Context, endpoint, provider, phase string, body []byte, creds domainProv.Credentials, setHeaders func(*http.Request)) (int, []byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if setHeaders != nil {
		setHeaders(httpReq)
	}
	resp, err := h.do(ctx, httpReq, provider, phase, creds, connectionID(creds), ValidatedHost{})
	if err != nil {
		return http.StatusBadGateway, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 400 {
		return resp.StatusCode, respBody, upstreamError(respBody)
	}
	return resp.StatusCode, respBody, nil
}

// pollResultRequestFactory builds a GET request to pollURL that carries the
// auth headers only when pollURL shares the canonical origin with submitOrigin.
// Credentials are never forwarded to a foreign host. The factory pre-attaches
// transport metadata (provider, connection id, credentials) to the request
// context so h.do — which preserves an existing metadata block when the
// ValidatedHost/connection is already set — propagates the submit connection
// to every poll attempt. The factory is called by the poll helper for every
// attempt.
func (h *Handler) pollResultRequestFactory(ctx context.Context, pollURL, submitOrigin string, setHeaders func(*http.Request), provider string, creds domainProv.Credentials) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, err
	}
	if setHeaders != nil {
		// Only attach credentials if the poll URL shares the canonical origin
		// with the submit request.
		if sameCanonicalOrigin(pollURL, submitOrigin) {
			setHeaders(req)
		}
	}
	// Pre-attach transport metadata so h.do preserves the submit connection
	// id and provider across poll/result hops (h.do preserves an existing
	// ValidatedHost/ConnectionID/ProviderID set on the request context). The
	// credentials are carried so the production executor can resolve the same
	// connection's proxy settings for poll/result.
	meta := TransportMetadata{
		ProviderID:   provider,
		ConnectionID: connectionID(creds),
		Credentials:  creds,
		Phase:        "poll",
	}
	req = req.WithContext(WithTransportMetadata(req.Context(), meta))
	return req, nil
}

// sameCanonicalOrigin reports whether a and b share the canonical origin
// (scheme, hostname, effective port). Used to decide whether credentials may
// be forwarded to a poll/result URL returned in a submit response.
func sameCanonicalOrigin(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}
	return canonicalOrigin(ua) == canonicalOrigin(ub)
}

// asyncOpenAIResult wraps a list of {url} or {b64_json} entries as the OpenAI
// {created, data:[...]} body. If response_format=binary, the caller routes
// through h.toBinary instead.
func asyncOpenAIResult(data []any) ([]byte, error) {
	out := map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
	}
	return json.Marshal(out)
}

// === fal-ai ===

// synthFalAI implements the fal-ai async contract:
//
//   - submit POST {BaseURL}/{model} with {prompt, num_images, image_size?,
//     image_url?} and Authorization: Key <tok>.
//   - the submit response carries {status_url, response_url}.
//   - poll status_url until status === COMPLETED, then fetch response_url with
//     the same headers and return its body.
//   - allowlist: queue.fal.run / *.fal.run (FalHostPredicate).
//   - one image input is accepted → image_url (resolveInputImage); images/mask
//     → 400 pre-executor.
//   - normalise: {images: [...]} OR {image: ...} → [{url}].
func (h *Handler) synthFalAI(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	modelSeg, err := escapePathSegments(req.Model)
	if err != nil {
		return nil, "", http.StatusBadRequest, fmt.Errorf("fal-ai: %w", err)
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/" + modelSeg

	// Image input: fal-ai accepts exactly one image (image_url). images/mask → 400.
	var img *ImageInput
	if len(req.Options.RawImageInputs) > 0 {
		in, rerr := h.singleImageInput(ctx, req.Options, "fal-ai")
		if rerr != nil {
			return nil, "", http.StatusBadRequest, rerr
		}
		img = &in
	}
	if len(req.Options.RawMask) > 0 {
		return nil, "", http.StatusBadRequest, fmt.Errorf("fal-ai does not support mask for this image model")
	}

	body := map[string]any{
		"prompt":     req.Prompt,
		"num_images": falNumImages(req.N, req.NSupplied),
	}
	if req.Size != "" {
		body["image_size"] = sizeToAspectRatio(req.Size)
	}
	if img != nil {
		body["image_url"] = falImageURL(*img)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}

	setHeaders := func(r *http.Request) { setAuthHeader(r, cfg, req.Credentials) }
	status, respBody, serr := h.doSubmit(ctx, endpoint, "fal-ai", "submit", raw, req.Credentials, setHeaders)
	if serr != nil {
		return nil, "", status, serr
	}

	var submit struct {
		StatusURL   string `json:"status_url"`
		ResponseURL string `json:"response_url"`
	}
	if err := json.Unmarshal(respBody, &submit); err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("fal-ai: malformed submit response: %w", err)
	}
	if submit.StatusURL == "" || submit.ResponseURL == "" {
		return nil, "", http.StatusBadGateway, fmt.Errorf("fal-ai: submit missing status_url/response_url")
	}
	if _, err := h.validateSubmitDerivedURL(submit.StatusURL, "fal-ai", cfg.BaseURL); err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("fal-ai: %w", err)
	}
	if _, err := h.validateSubmitDerivedURL(submit.ResponseURL, "fal-ai", cfg.BaseURL); err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("fal-ai: %w", err)
	}

	// Poll status_url.
	parser := func(b []byte) (PollStatus, error) {
		var s struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(b, &s); err != nil {
			return PollMalformed, err
		}
		switch s.Status {
		case "COMPLETED":
			return PollCompleted, nil
		case "FAILED":
			return PollFailed, errors.New(orDefault(s.Error, "fal generation failed"))
		case "IN_QUEUE", "IN_PROGRESS", "PENDING":
			return PollPending, nil
		default:
			return PollPending, nil
		}
	}
	factory := h.pollResultRequestFactoryFactory(ctx, submit.StatusURL, endpoint, setHeaders, "fal-ai", req.Credentials)
	if _, perr := h.poll(ctx, submit.StatusURL, factory, parser); perr != nil {
		return nil, "", pollHTTPStatus(perr), perr
	}

	// Fetch the result from response_url.
	resultReq, err := http.NewRequestWithContext(ctx, http.MethodGet, submit.ResponseURL, nil)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	resultReq.Header.Set("x-9gouter-provider", "fal-ai")
	if sameCanonicalOrigin(submit.ResponseURL, endpoint) {
		setHeaders(resultReq)
	}
	resp, ferr := h.do(ctx, resultReq, "fal-ai", "result", req.Credentials, connectionID(req.Credentials), ValidatedHost{})
	if ferr != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("fal-ai: result fetch: %w", ferr)
	}
	defer resp.Body.Close()
	resultBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		return nil, "", resp.StatusCode, upstreamError(resultBody)
	}

	data, nerr := falNormalise(resultBody)
	if nerr != nil {
		return nil, "", http.StatusBadGateway, nerr
	}
	outBody, merr := asyncOpenAIResult(data)
	if merr != nil {
		return nil, "", http.StatusInternalServerError, merr
	}
	if req.ResponseFormat == "binary" {
		return h.toBinary(outBody, req.OutputFormat)
	}
	return outBody, "application/json", http.StatusOK, nil
}

// pollResultRequestFactoryFactory returns a PollRequestFactory that reuses
// submitOrigin for the same-origin credential decision, attaches the provider's
// auth headers when the poll URL shares the submit canonical origin, and
// pre-attaches transport metadata (provider, connection id, credentials) so
// h.do preserves the submit connection across poll hops.
func (h *Handler) pollResultRequestFactoryFactory(ctx context.Context, _ /*pollURL overridden by helper*/, submitOrigin string, setHeaders func(*http.Request), provider string, creds domainProv.Credentials) PollRequestFactory {
	return func(ctx context.Context, pollURL string) (*http.Request, error) {
		return h.pollResultRequestFactory(ctx, pollURL, submitOrigin, setHeaders, provider, creds)
	}
}

// falNumImages returns the num_images value: absent n → 1, explicit n (incl 0)
// → n. Matches the legacy `body.n || 1` (n:0 → 1 in JS, but the legacy comment
// says `body.n || 1`; n:0 falsy → 1). Preserve the legacy behaviour: when n is
// 0, num_images defaults to 1.
func falNumImages(n int, supplied bool) int {
	if !supplied || n <= 0 {
		return 1
	}
	return n
}

// falImageURL returns the upstream image_url value for a resolved ImageInput.
// For a data input it sends the data URL; for a URL input it sends the HTTPS
// URL (the production executor pins the dial through the ValidatedHost on the
// request context for any fetch of this URL — fal fetches image_url server-side,
// so we send the URL as-is and trust fal's fetch; the SSRF guard already ran at
// resolve time to validate the host). Per the legacy contract the image_url is
// a public URL the upstream fetches; data URLs are passed inline as a URL form.
func falImageURL(in ImageInput) string {
	if in.Kind == "data" {
		return "data:" + in.MIME + ";base64," + in.B64
	}
	return in.URL
}

// falNormalise converts the fal result body into the OpenAI data list. The
// legacy normaliser accepts {images:[...]} OR a single {image:{...}} and maps
// each to {url: img.url || img}.
func falNormalise(body []byte) ([]any, error) {
	var resp struct {
		Images []json.RawMessage `json:"images"`
		Image  json.RawMessage   `json:"image"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("fal-ai: malformed result: %w", err)
	}
	var entries []json.RawMessage
	if len(resp.Images) > 0 {
		entries = resp.Images
	} else if len(resp.Image) > 0 {
		entries = []json.RawMessage{resp.Image}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("fal-ai: no image in result")
	}
	data := make([]any, 0, len(entries))
	for _, e := range entries {
		// Each entry is either a string URL or an object {url: ...}.
		var s string
		if json.Unmarshal(e, &s) == nil && s != "" {
			data = append(data, map[string]any{"url": s})
			continue
		}
		var obj map[string]any
		if json.Unmarshal(e, &obj) == nil {
			if u, ok := obj["url"].(string); ok && u != "" {
				data = append(data, map[string]any{"url": u})
				continue
			}
			// Fallback: the legacy `img.url || img` — if the object itself
			// is the URL-bearing value, surface it as-is.
			data = append(data, obj)
			continue
		}
		return nil, fmt.Errorf("fal-ai: malformed image entry")
	}
	return data, nil
}

// === black-forest-labs (BFL) ===

// synthBFL implements the BFL async contract:
//
//   - submit POST {BaseURL}/{model} (BaseURL = https://api.bfl.ai/v1) with
//     {prompt, width?, height?, image_prompt?} and x-key: <tok>.
//   - the submit response carries {polling_url}.
//   - poll polling_url with x-key + Accept: application/json until
//     status === Ready (return s) or status === Error/Failed (throw).
//   - allowlist: api.bfl.ai / *.bfl.ai (BFLHostPredicate).
//   - one image input → image_prompt; images/mask → 400.
//   - normalise: result.sample → [{url}].
func (h *Handler) synthBFL(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	modelSeg, err := escapePathSegments(req.Model)
	if err != nil {
		return nil, "", http.StatusBadRequest, fmt.Errorf("black-forest-labs: %w", err)
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/" + modelSeg

	var img *ImageInput
	if len(req.Options.RawImageInputs) > 0 {
		in, rerr := h.singleImageInput(ctx, req.Options, "black-forest-labs")
		if rerr != nil {
			return nil, "", http.StatusBadRequest, rerr
		}
		img = &in
	}
	if len(req.Options.RawMask) > 0 {
		return nil, "", http.StatusBadRequest, fmt.Errorf("black-forest-labs does not support mask for this image model")
	}

	body := map[string]any{"prompt": req.Prompt}
	if w, hgt, ok := bflDimensions(req.Size); ok {
		if w > 0 {
			body["width"] = w
		}
		if hgt > 0 {
			body["height"] = hgt
		}
	}
	if img != nil {
		body["image_prompt"] = bflImagePrompt(*img)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}

	setHeaders := func(r *http.Request) {
		setAuthHeader(r, cfg, req.Credentials)
		r.Header.Set("Accept", "application/json")
	}
	status, respBody, serr := h.doSubmit(ctx, endpoint, "black-forest-labs", "submit", raw, req.Credentials, setHeaders)
	if serr != nil {
		return nil, "", status, serr
	}

	var submit struct {
		PollingURL string `json:"polling_url"`
	}
	if err := json.Unmarshal(respBody, &submit); err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("black-forest-labs: malformed submit response: %w", err)
	}
	if submit.PollingURL == "" {
		return nil, "", http.StatusBadGateway, fmt.Errorf("black-forest-labs: submit missing polling_url")
	}
	if _, err := h.validateSubmitDerivedURL(submit.PollingURL, "black-forest-labs", cfg.BaseURL); err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("black-forest-labs: %w", err)
	}

	parser := func(b []byte) (PollStatus, error) {
		var s struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(b, &s); err != nil {
			return PollMalformed, err
		}
		switch s.Status {
		case "Ready":
			return PollCompleted, nil
		case "Error", "Failed":
			return PollFailed, errors.New(orDefault(s.Error, "BFL generation failed"))
		default:
			return PollPending, nil
		}
	}
	factory := h.pollResultRequestFactoryFactory(ctx, submit.PollingURL, endpoint, setHeaders, "black-forest-labs", req.Credentials)
	pres, perr := h.poll(ctx, submit.PollingURL, factory, parser)
	if perr != nil {
		return nil, "", pollHTTPStatus(perr), perr
	}

	data, nerr := bflNormalise(pres.Body)
	if nerr != nil {
		return nil, "", http.StatusBadGateway, nerr
	}
	outBody, merr := asyncOpenAIResult(data)
	if merr != nil {
		return nil, "", http.StatusInternalServerError, merr
	}
	if req.ResponseFormat == "binary" {
		return h.toBinary(outBody, req.OutputFormat)
	}
	return outBody, "application/json", http.StatusOK, nil
}

// bflDimensions parses a "WxH" size string into separate width/height values.
// Returns ok=false when the size is empty or malformed (in which case width/
// height are omitted from the upstream body — legacy behaviour).
func bflDimensions(size string) (width, height int, ok bool) {
	size = strings.TrimSpace(size)
	if size == "" {
		return 0, 0, false
	}
	parts := strings.SplitN(size, "x", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, werr := parseIntStrict(parts[0])
	h, herr := parseIntStrict(parts[1])
	if werr != nil || herr != nil {
		return 0, 0, false
	}
	return w, h, true
}

func parseIntStrict(s string) (int, error) {
	s = strings.TrimSpace(s)
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-numeric")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// bflImagePrompt returns the image_prompt value for a resolved ImageInput.
// BFL accepts the same forms as fal (data URL or HTTPS URL).
func bflImagePrompt(in ImageInput) string {
	if in.Kind == "data" {
		return "data:" + in.MIME + ";base64," + in.B64
	}
	return in.URL
}

// bflNormalise converts the BFL poll body (status=Ready) into the OpenAI data
// list. The legacy normaliser maps result.sample → [{url: sample}].
func bflNormalise(body []byte) ([]any, error) {
	var s struct {
		Result struct {
			Sample string `json:"sample"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("black-forest-labs: malformed poll result: %w", err)
	}
	if s.Result.Sample == "" {
		return nil, fmt.Errorf("black-forest-labs: no sample in result")
	}
	return []any{map[string]any{"url": s.Result.Sample}}, nil
}

// === runwayml ===

// runwayImageModelRE matches the spec-allowed image model ids for the runwayml
// image endpoint: ^gen4_image[A-Za-z0-9._-]{0,64}$. Non-matching models return
// 400 with guidance to use /v1/videos/* (video models are not routed through
// the images endpoint).
var runwayImageModelRE = regexp.MustCompile(`^gen4_image[A-Za-z0-9._-]{0,64}$`)

// synthRunwayML implements the runwayml async contract:
//
//   - validate model against ^gen4_image[A-Za-z0-9._-]{0,64}$ before any HTTP
//     call; non-matching → 400 with guidance /v1/videos/*.
//   - supplied image/images/mask → 400 pre-executor (edit pass-through
//     intentionally excluded from this parity tranche).
//   - submit POST {BaseURL}/text_to_image with {promptText, model, ratio},
//     Bearer + X-Runway-Version: 2024-11-06.
//   - the submit response carries {id}; poll {BaseURL}/tasks/{id}.
//   - poll until status === SUCCEEDED (return s) or FAILED/CANCELLED (throw).
//   - allowlist: api.dev.runwayml.com (RunwayMLHostPredicate).
//   - normalise: output[] → [{url}].
func (h *Handler) synthRunwayML(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	if b, ct, st, e := rejectEditInputs("runwayml", req.Options); e != nil {
		return b, ct, st, e
	}
	if !runwayImageModelRE.MatchString(req.Model) {
		return nil, "", http.StatusBadRequest, fmt.Errorf("runwayml: model %q is not an image model; use /v1/videos/* for video models", req.Model)
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/text_to_image"

	body := map[string]any{
		"promptText": req.Prompt,
		"model":      req.Model,
		"ratio":      sizeToAspectRatio(req.Size),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	setHeaders := func(r *http.Request) {
		setAuthHeader(r, cfg, req.Credentials)
		r.Header.Set("X-Runway-Version", "2024-11-06")
	}
	status, respBody, serr := h.doSubmit(ctx, endpoint, "runwayml", "submit", raw, req.Credentials, setHeaders)
	if serr != nil {
		return nil, "", status, serr
	}

	var submit struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &submit); err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("runwayml: malformed submit response: %w", err)
	}
	if submit.ID == "" {
		return nil, "", http.StatusBadGateway, fmt.Errorf("runwayml: submit missing task id")
	}
	taskURL := strings.TrimRight(cfg.BaseURL, "/") + "/tasks/" + url.PathEscape(submit.ID)
	if _, err := h.validateSubmitDerivedURL(taskURL, "runwayml", cfg.BaseURL); err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("runwayml: %w", err)
	}

	parser := func(b []byte) (PollStatus, error) {
		var s struct {
			Status  string `json:"status"`
			Failure string `json:"failure"`
		}
		if err := json.Unmarshal(b, &s); err != nil {
			return PollMalformed, err
		}
		switch s.Status {
		case "SUCCEEDED":
			return PollCompleted, nil
		case "FAILED", "CANCELLED":
			return PollFailed, errors.New(orDefault(s.Failure, "runway task failed"))
		default:
			return PollPending, nil
		}
	}
	factory := h.pollResultRequestFactoryFactory(ctx, taskURL, endpoint, setHeaders, "runwayml", req.Credentials)
	pres, perr := h.poll(ctx, taskURL, factory, parser)
	if perr != nil {
		return nil, "", pollHTTPStatus(perr), perr
	}

	data, nerr := runwayNormalise(pres.Body)
	if nerr != nil {
		return nil, "", http.StatusBadGateway, nerr
	}
	outBody, merr := asyncOpenAIResult(data)
	if merr != nil {
		return nil, "", http.StatusInternalServerError, merr
	}
	if req.ResponseFormat == "binary" {
		return h.toBinary(outBody, req.OutputFormat)
	}
	return outBody, "application/json", http.StatusOK, nil
}

// runwayNormalise converts the runwayml poll body (status=SUCCEEDED) into the
// OpenAI data list. The legacy normaliser maps output[] → [{url}].
func runwayNormalise(body []byte) ([]any, error) {
	var s struct {
		Output []string `json:"output"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("runwayml: malformed poll result: %w", err)
	}
	if len(s.Output) == 0 {
		return nil, fmt.Errorf("runwayml: no output in result")
	}
	data := make([]any, 0, len(s.Output))
	for _, u := range s.Output {
		if u == "" {
			continue
		}
		data = append(data, map[string]any{"url": u})
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("runwayml: no output url in result")
	}
	return data, nil
}

// === nanobanana ===

// synthNanobanana implements the nanobanana async contract:
//
//   - supplied image/images/mask → 400 pre-executor (edit pass-through
//     intentionally excluded; type is always TEXTTOIAMGE).
//   - submit POST {BaseURL} with {prompt, type:"TEXTTOIAMGE", numImages,
//     image_size, callBackUrl:"https://localhost/callback"}, Bearer.
//   - the submit response carries {code, msg, data:{taskId}}; code !== 200 →
//     throw msg.
//   - poll {PollURL}?taskId={taskId} until data.successFlag === 1 (return
//     s.data) or 2/3 (throw errorMessage).
//   - allowlist: the configured base host (NanobananaHostPredicate).
//   - normalise: response.resultImageUrl || response.originImageUrl →
//     [{url, revised_prompt: prompt}].
//   - no callback listener is created; callBackUrl is a fixed dummy field
//     required by the provider contract.
func (h *Handler) synthNanobanana(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	if b, ct, st, e := rejectEditInputs("nanobanana", req.Options); e != nil {
		return b, ct, st, e
	}
	if cfg.PollURL == "" {
		return nil, "", http.StatusInternalServerError, fmt.Errorf("nanobanana: poll URL not configured")
	}
	if _, err := h.validateSubmitDerivedURL(cfg.BaseURL, "nanobanana", cfg.BaseURL); err != nil {
		return nil, "", http.StatusInternalServerError, fmt.Errorf("nanobanana: configured base URL rejected: %w", err)
	}
	if _, err := h.validateSubmitDerivedURL(cfg.PollURL, "nanobanana", cfg.BaseURL); err != nil {
		return nil, "", http.StatusInternalServerError, fmt.Errorf("nanobanana: configured poll URL rejected: %w", err)
	}

	body := map[string]any{
		"prompt":      req.Prompt,
		"type":        "TEXTTOIAMGE",
		"numImages":   nanobananaNumImages(req.N, req.NSupplied),
		"image_size":  sizeToAspectRatio(req.Size),
		"callBackUrl": "https://localhost/callback",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	setHeaders := func(r *http.Request) { setAuthHeader(r, cfg, req.Credentials) }
	status, respBody, serr := h.doSubmit(ctx, cfg.BaseURL, "nanobanana", "submit", raw, req.Credentials, setHeaders)
	if serr != nil {
		return nil, "", status, serr
	}

	var submit struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			TaskID string `json:"taskId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &submit); err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("nanobanana: malformed submit response: %w", err)
	}
	if submit.Code != 200 {
		return nil, "", http.StatusBadGateway, fmt.Errorf("nanobanana: %s", orDefault(submit.Msg, "submit failed"))
	}
	if submit.Data.TaskID == "" {
		return nil, "", http.StatusBadGateway, fmt.Errorf("nanobanana: submit missing taskId")
	}
	pollURL := strings.TrimRight(cfg.PollURL, "/") + "?taskId=" + url.QueryEscape(submit.Data.TaskID)
	if _, err := h.validateSubmitDerivedURL(pollURL, "nanobanana", cfg.BaseURL); err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("nanobanana: %w", err)
	}

	parser := func(b []byte) (PollStatus, error) {
		var s struct {
			Data struct {
				SuccessFlag  int    `json:"successFlag"`
				ErrorMessage string `json:"errorMessage"`
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &s); err != nil {
			return PollMalformed, err
		}
		switch s.Data.SuccessFlag {
		case 1:
			return PollCompleted, nil
		case 2, 3:
			return PollFailed, errors.New(orDefault(s.Data.ErrorMessage, "nanobanana generation failed"))
		case 0:
			return PollPending, nil
		default:
			return PollPending, nil
		}
	}
	factory := h.pollResultRequestFactoryFactory(ctx, pollURL, cfg.BaseURL, setHeaders, "nanobanana", req.Credentials)
	pres, perr := h.poll(ctx, pollURL, factory, parser)
	if perr != nil {
		return nil, "", pollHTTPStatus(perr), perr
	}

	data, nerr := nanobananaNormalise(pres.Body, req.Prompt)
	if nerr != nil {
		return nil, "", http.StatusBadGateway, nerr
	}
	outBody, merr := asyncOpenAIResult(data)
	if merr != nil {
		return nil, "", http.StatusInternalServerError, merr
	}
	if req.ResponseFormat == "binary" {
		return h.toBinary(outBody, req.OutputFormat)
	}
	return outBody, "application/json", http.StatusOK, nil
}

// nanobananaNumImages returns the numImages value. Legacy `body.n || 1`: n:0
// is falsy → 1.
func nanobananaNumImages(n int, supplied bool) int {
	if !supplied || n <= 0 {
		return 1
	}
	return n
}

// nanobananaNormalise converts the nanobanana poll body (successFlag=1) into the
// OpenAI data list. The poll helper returns the full poll response, whose
// shape is {"data":{"successFlag":1,"response":{"resultImageUrl":...,
// "originImageUrl":...}}}. The legacy normaliser maps
// response.resultImageUrl || response.originImageUrl → [{url, revised_prompt}].
func nanobananaNormalise(body []byte, prompt string) ([]any, error) {
	var s struct {
		Data struct {
			Response struct {
				ResultImageURL string `json:"resultImageUrl"`
				OriginImageURL string `json:"originImageUrl"`
			} `json:"response"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("nanobanana: malformed poll result: %w", err)
	}
	u := s.Data.Response.ResultImageURL
	if u == "" {
		u = s.Data.Response.OriginImageURL
	}
	if u == "" {
		return nil, fmt.Errorf("nanobanana: no image url in result")
	}
	return []any{map[string]any{"url": u, "revised_prompt": prompt}}, nil
}

// === Error mapping ===

// pollHTTPStatus extracts the HTTP status the handler should return for a poll
// error. ErrPollTimeout (and its underlying DeadlineExceeded) maps to 504;
// any other typed poll error (failed/malformed/unexpected host/non-2xx) maps
// to 502. A plain context.Canceled surfaces the cancellation directly.
func pollHTTPStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if errors.Is(err, ErrPollTimeout) {
		return http.StatusGatewayTimeout
	}
	if pe, ok := err.(*pollError); ok {
		return pe.HTTPStatus()
	}
	if errors.Is(err, context.Canceled) {
		return http.StatusBadGateway
	}
	return http.StatusBadGateway
}
