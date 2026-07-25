package imageproxy

// providers_sync.go implements the synchronous image-provider adapters ported
// from the legacy JS handlers (step 5 of the image-provider-parity plan):
//
//   - sdwebui      — POST http://127.0.0.1:7860/sdapi/v1/txt2img, {prompt,width,
//                    height,steps:20,batch_size}; images[] → b64_json. Legacy
//                    literal decision table: size default 1024x1024, split
//                    strictly on "x", invalid/zero part → 512 (Number(x)||512),
//                    steps fixed 20, batch_size = n (absent n → 1, explicit
//                    n:0 → 0).
//   - comfyui      — POST base {prompt}; legacy normalize = passthrough of the
//                    already-OpenAI-shaped body. Malformed/non-OpenAI → 502.
//   - huggingface  — POST /models/{model} (path-escaped, traversal rejected),
//                    Authorization: Bearer, {inputs:prompt}; raw image bytes →
//                    magic-sniffed b64_json. Non-image raw → 502.
//   - stability-ai — POST /{core|ultra|sd3} (segment chosen by model), Bearer
//                    JSON {prompt, output_format, aspect_ratio[, style_preset,
//                    model]}; image (base64) → {created, data:[{b64_json}]}.
//
// sdwebui and comfyui are no-auth local providers (AuthTypeNone) and use the
// direct-only literal loopback origin from the registry; huggingface and
// stability-ai are credentialed connection-backed providers that go through
// the same h.do boundary as the OpenAI/Gemini/Codex paths. None of these
// adapters import the proxy package or repositories — h.do hands the request
// to the injected HTTPExecutor with transport metadata, and the production
// executor (wire.go) resolves the connection/proxy policy.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/image"
)

// === SDWebUI ===

// synthSDWebUI calls POST {BaseURL}/sdapi/v1/txt2img (registry base is the full
// txt2img endpoint) with {prompt, width, height, steps:20, batch_size} and
// normalizes images[] into {created, data:[{b64_json}]}.
func (h *Handler) synthSDWebUI(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	width, height := sdwebuiDimensions(req.Size)
	batchSize := sdwebuiBatchSize(req.N, req.NSupplied)
	payload := map[string]any{
		"prompt":     req.Prompt,
		"width":      width,
		"height":     height,
		"steps":      20,
		"batch_size": batchSize,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// noAuth: cfg.AuthType == AuthTypeNone — no Authorization header.
	resp, err := h.do(ctx, httpReq, req.ProviderID, "submit", req.Credentials, connectionID(req.Credentials), ValidatedHost{})
	if err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, "", resp.StatusCode, upstreamError(respBody)
	}
	var parsed struct {
		Images []string `json:"images"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("sdwebui: failed to parse response: %w", err)
	}
	if len(parsed.Images) == 0 {
		return nil, "", http.StatusBadGateway, fmt.Errorf("sdwebui: no image in response")
	}
	data := make([]any, 0, len(parsed.Images))
	for _, b64 := range parsed.Images {
		data = append(data, map[string]any{"b64_json": b64})
	}
	out := map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
	}
	outBody, _ := json.Marshal(out)
	if req.ResponseFormat == "binary" {
		return h.toBinary(outBody, req.OutputFormat)
	}
	return outBody, "application/json", http.StatusOK, nil
}

// sdwebuiDimensions parses a "WxH" size string the way the legacy sdwebui.js
// did: default "1024x1024", split strictly on "x" into two parts, each part
// coerced via Number(part) with `Number(x) || 512` — i.e. an invalid or zero
// dimension falls back to 512, never 0.
//
// Examples:
//   - "" → "1024x1024" → 1024,1024
//   - "1024x1024" → 1024,1024
//   - "768x1280" → 768,1280
//   - "badx512" → 512,512 (bad → 0 → 512; 512 stays)
//   - "512xbad" → 512,512
//   - "0x0" → 512,512
func sdwebuiDimensions(size string) (width, height int) {
	if strings.TrimSpace(size) == "" {
		size = "1024x1024"
	}
	parts := strings.SplitN(size, "x", 2)
	width = dimOr512(parts[0])
	if len(parts) > 1 {
		height = dimOr512(parts[1])
	} else {
		height = 512
	}
	return width, height
}

// dimOr512 mirrors the legacy `Number(part) || 512`: a non-numeric or zero
// value yields 512.
func dimOr512(part string) int {
	part = strings.TrimSpace(part)
	n, err := strconv.Atoi(part)
	if err != nil || n == 0 {
		return 512
	}
	return n
}

// sdwebuiBatchSize returns batch_size = n where absent n → 1 and explicit
// n:0 → 0. supplied is false when the JSON body had no `n` key.
func sdwebuiBatchSize(n int, supplied bool) int {
	if !supplied {
		return 1
	}
	return n
}

// === ComfyUI ===

// synthComfyUI POSTs {prompt} to the configured base and treats the response as
// already-OpenAI-shaped (legacy normalize = passthrough). A response that does
// not decode into an OpenAI {created, data:[…]} shape is a provider diagnostic
// (502).
func (h *Handler) synthComfyUI(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	payload := map[string]any{"prompt": req.Prompt}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// noAuth.
	resp, err := h.do(ctx, httpReq, req.ProviderID, "submit", req.Credentials, connectionID(req.Credentials), ValidatedHost{})
	if err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, "", resp.StatusCode, upstreamError(respBody)
	}
	// Legacy normalize is passthrough, but the response must be OpenAI-shaped.
	// Reject a malformed/non-OpenAI body as a provider diagnostic (502).
	if !looksLikeOpenAIImageBody(respBody) {
		return nil, "", http.StatusBadGateway, fmt.Errorf("comfyui: malformed response (expected OpenAI {created,data:[...]} shape)")
	}
	if req.ResponseFormat == "binary" {
		return h.toBinary(respBody, req.OutputFormat)
	}
	return respBody, "application/json", http.StatusOK, nil
}

// looksLikeOpenAIImageBody reports whether body decodes to a JSON object with
// a `data` array — the minimum OpenAI image-response shape ComfyUI's legacy
// passthrough assumes. A non-object body, a missing data field, or a non-array
// data fails the check.
func looksLikeOpenAIImageBody(body []byte) bool {
	var probe struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	if len(probe.Data) == 0 {
		return false
	}
	var arr []json.RawMessage
	return json.Unmarshal(probe.Data, &arr) == nil
}

// === HuggingFace ===

// synthHuggingFace POSTs {inputs:prompt} to {BaseURL}/{model} (BaseURL is
// https://api-inference.huggingface.co/models), Authorization: Bearer. The
// upstream returns raw image bytes; they are magic-sniffed and normalized to
// {created, data:[{b64_json}]}. Non-image raw bytes (e.g. an HTML error page)
// return 502.
func (h *Handler) synthHuggingFace(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	endpoint, err := huggingfaceEndpoint(cfg.BaseURL, req.Model)
	if err != nil {
		return nil, "", http.StatusBadRequest, err
	}
	payload := map[string]any{"inputs": req.Prompt}
	raw, err := json.Marshal(payload)
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
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", resp.StatusCode, upstreamError(body)
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxDecodedImageBytes+1))
	if rerr != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("huggingface: read body: %w", rerr)
	}
	if len(body) > maxDecodedImageBytes {
		return nil, "", http.StatusBadGateway, fmt.Errorf("huggingface: image exceeds %d bytes", maxDecodedImageBytes)
	}
	dec, mime, serr := sniffImage(body)
	if serr != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("huggingface: %w", ErrNotImage)
	}
	_ = mime
	out := map[string]any{
		"created": time.Now().Unix(),
		"data":    []any{map[string]any{"b64_json": base64StdEncoding(dec)}},
	}
	outBody, _ := json.Marshal(out)
	if req.ResponseFormat == "binary" {
		return h.toBinary(outBody, req.OutputFormat)
	}
	return outBody, "application/json", http.StatusOK, nil
}

// huggingfaceEndpoint builds {baseURL}/{escapedModel} and validates the model
// path. HuggingFace Inference API uses `models/{org}/{model}` paths (e.g.
// `black-forest-labs/FLUX.1-dev`), so a single leading model segment with
// sub-segments is allowed; each segment is escaped via url.PathEscape and
// traversal segments (".", "..") are rejected. The model must not carry a
// query or fragment.
func huggingfaceEndpoint(baseURL, model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("huggingface: missing model")
	}
	if strings.ContainsAny(model, "?#") {
		return "", fmt.Errorf("huggingface: model must not contain query or fragment: %q", model)
	}
	segs := strings.Split(model, "/")
	for _, s := range segs {
		s = strings.TrimSpace(s)
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("huggingface: model contains invalid or traversal segment: %q", model)
		}
	}
	escaped := make([]string, len(segs))
	for i, s := range segs {
		escaped[i] = url.PathEscape(s)
	}
	base := strings.TrimRight(baseURL, "/")
	return base + "/" + strings.Join(escaped, "/"), nil
}

// base64StdEncoding encodes bytes to standard base64 (used by the raw-image
// adapters — HF and Stability — after sniffing).
func base64StdEncoding(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// === Stability AI ===

// synthStability POSTs a Bearer JSON payload to {BaseURL}/{segment} where
// segment is "ultra", "sd3", or "core" depending on the model name. The legacy
// payload is {prompt, output_format, aspect_ratio[, style_preset, model]}.
// The response carries a base64 `image` field normalized to
// {created, data:[{b64_json}]}.
func (h *Handler) synthStability(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	segment := stabilitySegment(req.Model)
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/" + segment
	payload := map[string]any{
		"prompt":        req.Prompt,
		"output_format": strings.ToLower(orDefault(req.OutputFormat, "png")),
		"aspect_ratio":  sizeToAspectRatio(req.Size),
	}
	if strings.TrimSpace(req.Style) != "" {
		payload["style_preset"] = req.Style
	}
	// sd3 models include the model id in the body (legacy stabilityAi.js).
	if strings.Contains(req.Model, "sd3") {
		payload["model"] = req.Model
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	setAuthHeader(httpReq, cfg, req.Credentials)
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	resp, err := h.do(ctx, httpReq, req.ProviderID, "submit", req.Credentials, connectionID(req.Credentials), ValidatedHost{})
	if err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, "", resp.StatusCode, upstreamError(respBody)
	}
	var parsed struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("stability: failed to parse response: %w", err)
	}
	var data []any
	if parsed.Image != "" {
		data = []any{map[string]any{"b64_json": parsed.Image}}
	} else {
		data = []any{}
	}
	out := map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
	}
	outBody, _ := json.Marshal(out)
	if req.ResponseFormat == "binary" {
		return h.toBinary(outBody, req.OutputFormat)
	}
	return outBody, "application/json", http.StatusOK, nil
}

// stabilitySegment returns the endpoint segment for a Stability AI model:
// "ultra" when the model id contains "ultra", "sd3" when it contains "sd3",
// otherwise "core".
func stabilitySegment(model string) string {
	switch {
	case strings.Contains(model, "ultra"):
		return "ultra"
	case strings.Contains(model, "sd3"):
		return "sd3"
	default:
		return "core"
	}
}

// sizeToAspectRatio maps an OpenAI size string to the Stability aspect_ratio
// field using the legacy exact map. Unknown sizes default to "1:1".
func sizeToAspectRatio(size string) string {
	switch strings.TrimSpace(size) {
	case "1024x1024":
		return "1:1"
	case "1024x1792":
		return "9:16"
	case "1792x1024":
		return "16:9"
	case "1024x1536":
		return "2:3"
	case "1536x1024":
		return "3:2"
	default:
		return "1:1"
	}
}

// === Codex session id helper ===

// codexSessionID returns a fresh random UUID-ish string for the Codex CLI
// session_id / x-client-request-id headers. The legacy codex.js used
// randomUUID; here we use a simple monotonic counter seeded from time for
// test determinism — the headers only need to be non-empty, unique per
// request, and stable for the duration of one call.
func codexSessionID() string {
	return codexUUID(time.Now().UnixNano())
}

// codexUUID formats a 32-hex v4-shaped UUID from a 64-bit seed. It is NOT
// cryptographically random; it only needs to look like a UUID for the upstream
// contract.
func codexUUID(seed int64) string {
	// Distribute the seed across the 16 bytes via simple mixing.
	var b [16]byte
	for i := 0; i < 8; i++ {
		seed = seed*6364136223846793005 + 1442695040888963407
		b[i] = byte(seed >> (i * 1))
	}
	// Set version (4) and variant (10xx) bits to match a v4 UUID shape.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	const hex = "0123456789abcdef"
	out := make([]byte, 36)
	out[8] = '-'
	out[13] = '-'
	out[18] = '-'
	out[23] = '-'
	j := 0
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			// already wrote the hyphen before this byte's position
		}
		out[j] = hex[b[i]>>4]
		out[j+1] = hex[b[i]&0x0f]
		j += 2
		if i == 3 || i == 5 || i == 7 || i == 9 {
			out[j] = '-'
			j++
		}
	}
	return string(out[:36])
}
