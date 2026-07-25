package imageproxy

// cloudflare.go implements the Cloudflare AI image-generation contract (step 6
// of the image-provider-parity plan), porting the legacy JS
// open-sse/handlers/imageProviders/cloudflareAi.js behaviour:
//
//   - Cloudflare account ID is taken from credentials'
//     ProviderSpecificData["accountId"]; it MUST be a 32-hex identifier or a
//     UUID (v4-shape). Invalid/absent → 400 before any outbound call.
//   - The model segment is validated: query, fragment, userinfo and traversal
//     segments are rejected, and the path is escaped via url.PathEscape. The
//     three canonical FLUX.2 multipart IDs are matched exactly and use a
//     multipart/form-data body; all other models use JSON.
//   - Multipart models accept only `prompt`, dimensions (width/height as
//     strings) and the six named optional fields as strings. image/mask are
//     NOT supported on multipart models — supplying them returns 400 before
//     the executor.
//   - JSON models build the exact legacy key set: `prompt`; dimensions derived
//     from `size` regex `^\d+x\d+$` then independently overwritten by finite
//     `width`/`height`; permitted `image` → `image_b64:string` + `image:[]byte`;
//     canonical `mask` → `mask_b64:string`, `mask:[]byte`, `mask_image:[]byte`;
//     six named fields — only present non-null/non-empty values, numeric 0
//     retained. Image/mask inputs are resolved through resolveInputImage (the
//     safe input resolver from step 4: SSRF guard, 16 MiB cap, magic-byte sniff).
//   - The response is parsed into the OpenAI {created, data:[…]} shape. A
//     Content-Type: image/* body is magic-sniffed and base64-encoded as
//     b64_json. JSON is normalised via the legacy normalizeCloudflareResponse
//     rules (result.responses queued shape, result.data[0].{b64_json,url},
//     string result, result.image). Non-image raw bytes (HTML error page) → 502
//     (never base64-encoded).
//
// imageproxy never imports the proxy/DB/repo packages — h.do hands the request
// to the injected HTTPExecutor with transport metadata.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/image"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// CloudflareMultipartModels is the exact legacy FLUX.2 multipart set. These
// models use multipart/form-data and never accept image/mask inputs.
var CloudflareMultipartModels = []string{
	"@cf/black-forest-labs/flux-2-dev",
	"@cf/black-forest-labs/flux-2-klein-4b",
	"@cf/black-forest-labs/flux-2-klein-9b",
}

// CloudflareImg2ImgModel is the single Cloudflare img2img JSON model.
const CloudflareImg2ImgModel = "@cf/runwayml/stable-diffusion-v1-5-img2img"

// CloudflareInpaintingModel is the single Cloudflare inpainting JSON model.
const CloudflareInpaintingModel = "@cf/runwayml/stable-diffusion-v1-5-inpainting"

// cloudflareNamedFields are the six legacy Cloudflare optional fields. They are
// forwarded only when present, non-null and non-empty (numeric 0 is retained).
var cloudflareNamedFields = []struct {
	name string
	get  func(RequestOptions) json.RawMessage
}{
	{"negative_prompt", func(o RequestOptions) json.RawMessage { return o.NegativePrompt }},
	{"guidance", func(o RequestOptions) json.RawMessage { return o.Guidance }},
	{"seed", func(o RequestOptions) json.RawMessage { return o.Seed }},
	{"num_steps", func(o RequestOptions) json.RawMessage { return o.NumSteps }},
	{"steps", func(o RequestOptions) json.RawMessage { return o.Steps }},
	{"strength", func(o RequestOptions) json.RawMessage { return o.Strength }},
}

// cloudflareSizeRE matches the legacy `^(\d+)x(\d+)$` size regex.
var cloudflareSizeRE = regexp.MustCompile(`^(\d+)x(\d+)$`)

// cloudflareHexIDRE matches a 32-hex Cloudflare account identifier.
var cloudflareHexIDRE = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// cloudflareUUIDRE matches a UUID v4-shaped string (8-4-4-4-12 hex digits). The
// version nibble is not enforced (Cloudflare accepts any UUID-shape id).
var cloudflareUUIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// synthCloudflareAI builds and dispatches the Cloudflare AI image request. It
// validates the account ID and model, then routes to the multipart or JSON
// body builder.
func (h *Handler) synthCloudflareAI(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	accountID, err := cloudflareAccountID(req.Credentials)
	if err != nil {
		return nil, "", http.StatusBadRequest, err
	}
	modelSeg, err := validateCloudflareModel(req.Model)
	if err != nil {
		return nil, "", http.StatusBadRequest, err
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/" + url.PathEscape(accountID) + "/ai/run/" + modelSeg

	if isCloudflareMultipartModel(req.Model) {
		// Multipart models never accept image/mask inputs — the legacy
		// multipart contract carries no image/mask fields. Reject before the
		// executor.
		if len(req.Options.RawImageInputs) > 0 || len(req.Options.RawMask) > 0 {
			return nil, "", http.StatusBadRequest, fmt.Errorf("cloudflare-ai: image/mask inputs are not supported on multipart models")
		}
		return h.cloudflareMultipart(ctx, cfg, req, endpoint)
	}
	return h.cloudflareJSON(ctx, cfg, req, endpoint)
}

// === Validation ===

// cloudflareAccountID extracts the Cloudflare account ID from the credentials'
// ProviderSpecificData["accountId"] (legacy source) and validates it is a
// 32-hex identifier or a UUID. Absent/invalid → 400.
func cloudflareAccountID(c domainProv.Credentials) (string, error) {
	raw := ""
	if c.ProviderSpecificData != nil {
		if v, ok := c.ProviderSpecificData["accountId"]; ok {
			if s, ok := v.(string); ok {
				raw = strings.TrimSpace(s)
			}
		}
	}
	if raw == "" {
		return "", fmt.Errorf("cloudflare-ai: missing accountId in credentials")
	}
	if !cloudflareHexIDRE.MatchString(raw) && !cloudflareUUIDRE.MatchString(raw) {
		return "", fmt.Errorf("cloudflare-ai: accountId must be a 32-hex identifier or UUID, got %q", raw)
	}
	return raw, nil
}

// validateCloudflareModel validates the model segment for the Cloudflare AI
// `/ai/run/{model}` path. It rejects empty values, query/fragment, userinfo,
// and traversal segments, then escapes each segment via url.PathEscape. A
// leading "@" is preserved (Cloudflare model IDs are "@cf/.../..."). The
// returned string is safe to embed in the path.
func validateCloudflareModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("cloudflare-ai: missing model")
	}
	if strings.ContainsAny(model, "?#") {
		return "", fmt.Errorf("cloudflare-ai: model must not contain query or fragment: %q", model)
	}
	segs := strings.Split(model, "/")
	for _, s := range segs {
		s = strings.TrimSpace(s)
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("cloudflare-ai: model contains invalid or traversal segment: %q", model)
		}
	}
	escaped := make([]string, len(segs))
	for i, s := range segs {
		escaped[i] = url.PathEscape(s)
	}
	return strings.Join(escaped, "/"), nil
}

// isCloudflareMultipartModel reports whether the model is one of the three
// canonical FLUX.2 multipart IDs.
func isCloudflareMultipartModel(model string) bool {
	for _, m := range CloudflareMultipartModels {
		if m == model {
			return true
		}
	}
	return false
}

// === Dimensions ===

// cloudflareDimensions derives width/height the legacy way: first from the
// `size` regex `^\d+x\d+$`, then independently overwritten by finite supplied
// `width`/`height` json.RawMessage values. A finite value is a present,
// non-null JSON number (including 0). Returns (width, height, presentW, presentH)
// where present reports whether each dimension was set (for multipart, the
// strings are only emitted when present).
//
// When nothing supplies a dimension the returned pair is (0,0) and the caller
// omits the dimension field from the body (mirrors legacy getDimensions: no
// width/height key unless derived or supplied).
func cloudflareDimensions(size string, w, h json.RawMessage) (width, height int, presentW, presentH bool) {
	if m := cloudflareSizeRE.FindStringSubmatch(strings.TrimSpace(size)); m != nil {
		width, _ = strconv.Atoi(m[1])
		height, _ = strconv.Atoi(m[2])
		presentW, presentH = true, true
	}
	if n, ok, present := finiteIntFromJSON(w); ok {
		width = n
		presentW = present
	}
	if n, ok, present := finiteIntFromJSON(h); ok {
		height = n
		presentH = present
	}
	return width, height, presentW, presentH
}

// finiteIntFromJSON decodes a json.RawMessage as an integer. ok=true when the
// value is a present, non-null JSON number (including 0); present mirrors ok
// for the caller's convenience. nil/absent, null, strings, and non-numeric
// values return ok=false (the dimension is not overwritten).
func finiteIntFromJSON(raw json.RawMessage) (n int, ok bool, present bool) {
	if len(raw) == 0 {
		return 0, false, false
	}
	// null → not finite.
	if strings.TrimSpace(string(raw)) == "null" {
		return 0, false, false
	}
	var v json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return 0, false, false
	}
	i, err := v.Int64()
	if err != nil {
		// A float dimension is unusual but legacy coerced via Number(); fall
		// back to parsing as float and truncating.
		f, ferr := v.Float64()
		if ferr != nil {
			return 0, false, false
		}
		return int(f), true, true
	}
	return int(i), true, true
}

// === Optional fields ===

// addOptionalJSON adds the six named optional fields to the JSON body when they
// are present, non-null and non-empty. Numeric 0 is retained (the legacy
// `value === null || value === ""` check passes 0 through). The value is
// forwarded as-is (JSON number/string/bool), preserving the client's type.
func addOptionalJSON(body map[string]any, opts RequestOptions) {
	for _, f := range cloudflareNamedFields {
		raw := f.get(opts)
		if !cloudflareOptionalPresent(raw) {
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			continue
		}
		body[f.name] = v
	}
}

// addOptionalMultipart writes the six named optional fields as string form
// fields when present, non-null and non-empty. Numeric 0 is retained (written
// as "0"). The value is stringified via the legacy coercion (Number → string,
// string → as-is).
func addOptionalMultipart(w *multipart.Writer, opts RequestOptions) error {
	for _, f := range cloudflareNamedFields {
		raw := f.get(opts)
		if !cloudflareOptionalPresent(raw) {
			continue
		}
		val, err := cloudflareOptionalString(raw)
		if err != nil {
			return err
		}
		if err := w.WriteField(f.name, val); err != nil {
			return err
		}
	}
	return nil
}

// cloudflareOptionalPresent reports whether a json.RawMessage should be
// forwarded as an optional field: present (non-nil), non-null, and non-empty
// string. Numeric 0 is retained (present=true). This mirrors the legacy
// `value === undefined || value === null || value === ""` skip rule.
func cloudflareOptionalPresent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		return false
	}
	// An empty JSON string "" is skipped.
	if trimmed == `""` {
		return false
	}
	return true
}

// cloudflareOptionalString stringifies a json.RawMessage for the multipart
// path. Numbers (including 0) become their decimal string; strings are
// unwrapped; bools become "true"/"false". Null/absent are handled by
// cloudflareOptionalPresent and never reach here.
func cloudflareOptionalString(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	switch t := v.(type) {
	case json.Number:
		// Prefer the integer form when it round-trips; otherwise the raw token.
		if i, err := t.Int64(); err == nil {
			return strconv.FormatInt(i, 10), nil
		}
		return t.String(), nil
	case string:
		return t, nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case nil:
		return "", nil
	}
	// Objects/arrays are not valid optional fields; stringify defensively.
	b, _ := json.Marshal(v)
	return string(b), nil
}

// === Multipart ===

// cloudflareMultipart builds a multipart/form-data body with `prompt`,
// dimensions (width/height as strings when present) and the six named optional
// fields as strings. No image/mask fields are written (image/mask on multipart
// models is rejected upstream). The Content-Type is set by the multipart writer
// (boundary included); the caller must NOT set Content-Type manually.
func (h *Handler) cloudflareMultipart(ctx context.Context, cfg image.Config, req Request, endpoint string) ([]byte, string, int, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("prompt", req.Prompt); err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	width, height, presentW, presentH := cloudflareDimensions(req.Size, req.Options.Width, req.Options.Height)
	if presentW {
		if err := w.WriteField("width", strconv.Itoa(width)); err != nil {
			return nil, "", http.StatusInternalServerError, err
		}
	}
	if presentH {
		if err := w.WriteField("height", strconv.Itoa(height)); err != nil {
			return nil, "", http.StatusInternalServerError, err
		}
	}
	if err := addOptionalMultipart(w, req.Options); err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	if err := w.Close(); err != nil {
		return nil, "", http.StatusInternalServerError, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	// Do NOT set Content-Type: the multipart writer owns the boundary.
	httpReq.Header.Set("Content-Type", w.FormDataContentType())
	setAuthHeader(httpReq, cfg, req.Credentials)
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	resp, err := h.do(ctx, httpReq, req.ProviderID, "submit", req.Credentials, connectionID(req.Credentials), ValidatedHost{})
	if err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	return h.parseCloudflareResponse(resp, req)
}

// === JSON ===

// cloudflareJSON builds the JSON body with the exact legacy key set and POSTs
// it as application/json. image/mask inputs are resolved through
// resolveInputImage (SSRF guard + magic-byte sniff); the resolved bytes feed
// image_b64/mask_b64 (strings) and image/mask/mask_image ([]byte base64 strings
// — the JSON encoder serialises []byte as base64 automatically, matching the
// legacy contract).
func (h *Handler) cloudflareJSON(ctx context.Context, cfg image.Config, req Request, endpoint string) ([]byte, string, int, error) {
	body := map[string]any{
		"prompt": req.Prompt,
	}
	width, height, presentW, presentH := cloudflareDimensions(req.Size, req.Options.Width, req.Options.Height)
	if presentW {
		body["width"] = width
	}
	if presentH {
		body["height"] = height
	}
	addOptionalJSON(body, req.Options)

	// Resolve permitted image input (the first raw image input). The capability
	// table has already authorised the image field; here we convert the raw
	// canonical value through the safe input resolver.
	if len(req.Options.RawImageInputs) > 0 {
		ii, err := h.resolveInputImage(ctx, req.Options.RawImageInputs[0], "image")
		if err != nil {
			return nil, "", http.StatusBadRequest, err
		}
		if ii.Kind == "data" {
			body["image_b64"] = ii.B64
			// image:[]byte — Go's json.Marshal encodes []byte as base64. We
			// store the decoded bytes so the wire shape is "image":"<b64>".
			dec, err := base64.StdEncoding.DecodeString(ii.B64)
			if err != nil {
				return nil, "", http.StatusInternalServerError, fmt.Errorf("cloudflare-ai: image decode: %w", err)
			}
			body["image"] = dec
		} else if ii.Kind == "url" {
			// The legacy urlToBase64 fetches the URL and embeds the bytes. The
			// safe resolver already validated the host; fetch it now via h.do.
			b, _, status, err := h.downloadImageURL(ctx, ii.URL, req.ProviderID, connectionID(req.Credentials), func(u *url.URL) (*http.Request, error) {
				httpReq, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
				if e != nil {
					return nil, e
				}
				setAuthHeader(httpReq, cfg, req.Credentials)
				return httpReq, nil
			})
			if err != nil {
				return nil, "", status, fmt.Errorf("cloudflare-ai: image: %w", err)
			}
			body["image_b64"] = base64.StdEncoding.EncodeToString(b)
			body["image"] = b
		}
	}

	// Resolve canonical mask (mask_image/maskImage/mask already canonicalised
	// by the handler into RawMask).
	if len(req.Options.RawMask) > 0 {
		ii, err := h.resolveInputImage(ctx, req.Options.RawMask, "mask")
		if err != nil {
			return nil, "", http.StatusBadRequest, err
		}
		if ii.Kind == "data" {
			body["mask_b64"] = ii.B64
			dec, err := base64.StdEncoding.DecodeString(ii.B64)
			if err != nil {
				return nil, "", http.StatusInternalServerError, fmt.Errorf("cloudflare-ai: mask decode: %w", err)
			}
			body["mask"] = dec
			body["mask_image"] = dec
		} else if ii.Kind == "url" {
			b, _, status, err := h.downloadImageURL(ctx, ii.URL, req.ProviderID, connectionID(req.Credentials), func(u *url.URL) (*http.Request, error) {
				httpReq, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
				if e != nil {
					return nil, e
				}
				setAuthHeader(httpReq, cfg, req.Credentials)
				return httpReq, nil
			})
			if err != nil {
				return nil, "", status, fmt.Errorf("cloudflare-ai: mask: %w", err)
			}
			body["mask_b64"] = base64.StdEncoding.EncodeToString(b)
			body["mask"] = b
			body["mask_image"] = b
		}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setAuthHeader(httpReq, cfg, req.Credentials)
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	resp, err := h.do(ctx, httpReq, req.ProviderID, "submit", req.Credentials, connectionID(req.Credentials), ValidatedHost{})
	if err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	return h.parseCloudflareResponse(resp, req)
}

// === Response parsing ===

// parseCloudflareResponse consumes the Cloudflare AI upstream response and
// normalises it into the OpenAI {created, data:[…]} shape. A Content-Type:
// image/* body is magic-sniffed and base64-encoded as b64_json. A JSON body is
// normalised via the legacy normalizeCloudflareResponse rules. Non-image raw
// bytes (e.g. an HTML error page) return 502 — they are never base64-encoded.
func (h *Handler) parseCloudflareResponse(resp *http.Response, req Request) ([]byte, string, int, error) {
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", resp.StatusCode, upstreamError(body)
	}
	ct := resp.Header.Get("Content-Type")
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxDownloadImageBytes+1))
	if rerr != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("cloudflare-ai: read body: %w", rerr)
	}
	if len(body) > maxDownloadImageBytes {
		return nil, "", http.StatusBadGateway, fmt.Errorf("cloudflare-ai: response exceeds %d bytes", maxDownloadImageBytes)
	}

	// Raw image response: sniff magic bytes and base64-encode.
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "image/") {
		dec, mime, serr := sniffImage(body)
		if serr != nil {
			return nil, "", http.StatusBadGateway, fmt.Errorf("cloudflare-ai: %w", ErrNotImage)
		}
		_ = mime
		out := map[string]any{
			"created": time.Now().Unix(),
			"data":    []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString(dec)}},
		}
		outBody, _ := json.Marshal(out)
		if req.ResponseFormat == "binary" {
			return h.toBinary(outBody, req.OutputFormat)
		}
		return outBody, "application/json", http.StatusOK, nil
	}

	// Try JSON normalisation. If the body is not valid JSON, treat it as a
	// non-image raw response → 502 (never base64-encode HTML/error pages).
	var parsed json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("cloudflare-ai: non-JSON non-image response: %w", ErrNotImage)
	}
	normalised, err := normalizeCloudflareResponse(parsed)
	if err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	outBody, _ := json.Marshal(normalised)
	if req.ResponseFormat == "binary" {
		return h.toBinary(outBody, req.OutputFormat)
	}
	return outBody, "application/json", http.StatusOK, nil
}

// normalizeCloudflareResponse ports the legacy normalizeCloudflareResponse +
// parseResponse logic. It accepts:
//   - an already-OpenAI-shaped {created, data:[…]} body (passthrough),
//   - a Cloudflare result envelope: {result: …} or {result: {responses:[…]}},
//     where each queued response carries {success, result}; the first
//     success!==false result is recursed,
//   - a result with image as a string (data URL → b64_json, http(s) → {url},
//     bare base64 → {b64_json}),
//   - a result with {image, data:[{b64_json|url}]}.
//
// It returns the OpenAI {created, data:[…]} shape.
func normalizeCloudflareResponse(body json.RawMessage) (map[string]any, error) {
	// Passthrough: already {created, data:[…]}.
	var openAI struct {
		Created json.RawMessage `json:"created"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &openAI); err == nil && len(openAI.Data) > 0 {
		var arr []json.RawMessage
		if json.Unmarshal(openAI.Data, &arr) == nil {
			out := map[string]any{"data": []any{}}
			if len(openAI.Created) > 0 {
				var c any
				if json.Unmarshal(openAI.Created, &c) == nil {
					out["created"] = c
				}
			}
			if _, ok := out["created"]; !ok {
				out["created"] = time.Now().Unix()
			}
			for _, item := range arr {
				out["data"] = append(out["data"].([]any), json.RawMessage(item))
			}
			return out, nil
		}
	}

	// Cloudflare envelope: {result: …} (top-level result) or the body itself
	// is the result.
	var wrapper struct {
		Result json.RawMessage `json:"result"`
	}
	hasResult := false
	if err := json.Unmarshal(body, &wrapper); err == nil && len(wrapper.Result) > 0 {
		hasResult = true
	}
	result := body
	if hasResult {
		result = wrapper.Result
	}

	// Queued shape: result.responses[] (each {success, result}); pick the first
	// success!==false result and recurse.
	var queued struct {
		Responses []struct {
			Success bool            `json:"success"`
			Result  json.RawMessage `json:"result"`
		} `json:"responses"`
	}
	if err := json.Unmarshal(result, &queued); err == nil && len(queued.Responses) > 0 {
		for _, r := range queued.Responses {
			if r.Success && len(r.Result) > 0 {
				return normalizeCloudflareResponse(r.Result)
			}
		}
		return map[string]any{"created": time.Now().Unix(), "data": []any{}}, nil
	}

	// String result: data URL → b64_json, http(s) → {url}, bare → {b64_json}.
	var s string
	if err := json.Unmarshal(result, &s); err == nil && s != "" {
		item := cloudflareImageItemFromString(s)
		return map[string]any{"created": time.Now().Unix(), "data": []any{item}}, nil
	}

	// Object result: image (string) and/or data[].
	var obj struct {
		Image string `json:"image"`
		Data  []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(result, &obj); err != nil {
		return nil, fmt.Errorf("cloudflare-ai: unrecognised response shape")
	}
	var data []any
	if obj.Image != "" {
		data = append(data, cloudflareImageItemFromString(obj.Image))
	}
	for _, d := range obj.Data {
		if d.B64JSON != "" {
			data = append(data, map[string]any{"b64_json": d.B64JSON})
		} else if d.URL != "" {
			data = append(data, map[string]any{"url": d.URL})
		}
	}
	return map[string]any{"created": time.Now().Unix(), "data": data}, nil
}

// cloudflareImageItemFromString converts a string image result into the OpenAI
// item shape: a data URL → {b64_json} (prefix stripped), an http(s) URL → {url},
// a bare base64 string → {b64_json}.
func cloudflareImageItemFromString(s string) map[string]any {
	if strings.HasPrefix(s, "data:") {
		// Strip "data:<mime>;base64," prefix.
		if comma := strings.IndexByte(s, ','); comma >= 0 {
			return map[string]any{"b64_json": s[comma+1:]}
		}
		return map[string]any{"b64_json": strings.TrimPrefix(s, "data:")}
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return map[string]any{"url": s}
	}
	return map[string]any{"b64_json": s}
}
