// Package imageproxy implements the /v1/images/generations pipeline for the Go
// rewrite. It ports src/sse/handlers/imageGeneration.js (handleImageGeneration)
// + open-sse/handlers/imageGenerationCore.js (adapter pattern) + the
// per-provider imageProviders adapters: generate images via the provider's
// static image config, normalize the upstream response into the OpenAI
// {created, data:[{url|b64_json}]} shape (or raw binary when
// response_format=binary).
//
// Supported in this MVP slice:
//   - OpenAI-compatible (openai, minimax, openrouter, recraft, xai with
//     bodyFields whitelist, vercel-ai-gateway, venice) — passthrough OpenAI
//     shape.
//   - Gemini — generateContent with responseModalities ["TEXT","IMAGE"] →
//     candidates[].content.parts[].inlineData.data → {b64_json}.
//   - Codex — Responses API with tools:[{type:"image_generation",…}], SSE
//     parse → {created, data:[{b64_json}]}.
//
// Deferred (501): sdwebui, comfyui (noAuth local), huggingface (raw binary),
// fal-ai / black-forest-labs / runwayml / nanobanana (async polling),
// stability-ai, cloudflare-ai, antigravity. The handler resolves the provider
// from body.model (provider/model prefix or bare → openai fallback).
//
// NOT in this slice (separate slices): combo expansion, account-fallback
// rotation, on-401 token refresh, usage persistence, x-9gouter-connection-id
// forwarding (the JS handler does echo it for pinning; deferred here).
package imageproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/image"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// Logger is a minimal log sink.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

type noopLogger struct{}

func (noopLogger) Infof(string, ...any)  {}
func (noopLogger) Warnf(string, ...any)  {}
func (noopLogger) Debugf(string, ...any) {}

// Dependencies wires the imageproxy Handler.
type Dependencies struct {
	HTTPClient *http.Client
	Logger     Logger
	Config     config.Config
}

// Handler runs the image-generation pipeline.
type Handler struct {
	deps Dependencies
}

// New constructs a Handler with sane defaults (300s body timeout — image gen
// can be slow, especially Codex streaming).
func New(deps Dependencies) *Handler {
	if deps.HTTPClient == nil {
		deps.HTTPClient = &http.Client{Timeout: 300 * time.Second}
	}
	if deps.Logger == nil {
		deps.Logger = noopLogger{}
	}
	return &Handler{deps: deps}
}

// Request is the input to Handle.
type Request struct {
	Ctx            context.Context
	ProviderID     string
	Model          string
	Prompt         string
	N              int
	Size           string
	Quality        string
	Style          string
	ResponseFormat string // "url" (default) | "b64_json" | "binary" (raw image bytes)
	OutputFormat   string // "png" (default) | "jpeg" | "webp" — used by codex + binary
	Background     string // codex
	Credentials    domainProv.Credentials
	UserAgent      string
}

// Result is the output of Handle.
type Result struct {
	StatusCode  int
	Err         error
	Body        []byte
	ContentType string
}

// Handle dispatches the image-generation upstream call by the provider's
// static config.
func (h *Handler) Handle(ctx context.Context, req Request) Result {
	cfg, ok := image.Lookup(req.ProviderID)
	if !ok {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("provider '%s' does not support image generation", req.ProviderID)}
	}
	if cfg.Unsupported {
		return Result{StatusCode: http.StatusNotImplemented, Err: fmt.Errorf("provider '%s' image transport not implemented in Go build", req.ProviderID)}
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("missing required field: prompt")}
	}
	if cfg.AuthType != image.AuthTypeNone && credentialToken(req.Credentials) == "" {
		return Result{StatusCode: http.StatusUnauthorized, Err: fmt.Errorf("no credentials for provider: %s", req.ProviderID)}
	}

	body, contentType, status, err := h.synthesize(ctx, cfg, req)
	if err != nil {
		return Result{StatusCode: status, Err: err}
	}
	return Result{StatusCode: status, Body: body, ContentType: contentType}
}

// synthesize dispatches by the provider's static Format.
func (h *Handler) synthesize(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	switch cfg.Format {
	case image.FormatOpenAI:
		return h.synthOpenAICompatible(ctx, cfg, req)
	case image.FormatGemini:
		return h.synthGemini(ctx, cfg, req)
	case image.FormatCodex:
		return h.synthCodex(ctx, cfg, req)
	default:
		return nil, "", http.StatusNotImplemented, fmt.Errorf("image format %q not implemented", cfg.Format)
	}
}

// synthOpenAICompatible builds the OpenAI {model,prompt,n,size,quality,style,
// response_format} body (optionally whitelisted via cfg.BodyFields), POSTs to
// cfg.BaseURL, and returns the upstream response verbatim (OpenAI shape
// {created, data:[…]}). For response_format=binary it extracts the first
// image (b64_json or downloads url) and returns raw image bytes.
func (h *Handler) synthOpenAICompatible(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	body := buildOpenAIBody(req, cfg.BodyFields)
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setAuthHeader(httpReq, cfg, req.Credentials)
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	resp, err := h.deps.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, "", resp.StatusCode, upstreamError(respBody)
	}
	// Binary output: extract first image and return raw bytes.
	if req.ResponseFormat == "binary" {
		return h.toBinary(respBody, req.OutputFormat)
	}
	return respBody, "application/json", resp.StatusCode, nil
}

// synthGemini calls generateContent with responseModalities ["TEXT","IMAGE"]
// and reshapes candidates[].content.parts[].inlineData.data into the OpenAI
// {created, data:[{b64_json}]} shape. Binary output returns the first image
// raw bytes.
func (h *Handler) synthGemini(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	modelID := strings.TrimPrefix(req.Model, "models/")
	url := fmt.Sprintf("%s/%s:generateContent", cfg.BaseURL, modelID)
	// Gemini uses ?key=<tok> — append it.
	tok := credentialToken(req.Credentials)
	if tok != "" {
		url += "?key=" + tok
	}
	payload := map[string]any{
		"contents": []any{
			map[string]any{"parts": []any{map[string]any{"text": req.Prompt}}},
		},
		"generationConfig": map[string]any{
			"responseModalities": []string{"TEXT", "IMAGE"},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	resp, err := h.deps.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, "", resp.StatusCode, upstreamError(respBody)
	}
	// Reshape: collect all inlineData.data base64 blobs.
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *struct {
						Data     string `json:"data"`
						MimeType string `json:"mimeType"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("gemini: failed to parse response: %w", err)
	}
	var images []map[string]any
	for _, c := range parsed.Candidates {
		for _, p := range c.Content.Parts {
			if p.InlineData != nil && p.InlineData.Data != "" {
				images = append(images, map[string]any{"b64_json": p.InlineData.Data})
			}
		}
	}
	if len(images) == 0 {
		return nil, "", http.StatusBadGateway, fmt.Errorf("gemini: no image in response")
	}
	out := map[string]any{
		"created": time.Now().Unix(),
		"data":    images,
	}
	outBody, _ := json.Marshal(out)
	if req.ResponseFormat == "binary" {
		return h.toBinary(outBody, req.OutputFormat)
	}
	return outBody, "application/json", http.StatusOK, nil
}

// synthCodex calls the Codex Responses API with tools:[{type:"image_generation",
// output_format,size,quality,background}], parses the SSE stream for the
// image_generation_call / output_item.done events carrying the base64 result,
// and returns {created, data:[{b64_json}]}. Codex input images / streaming
// passthrough are deferred.
func (h *Handler) synthCodex(ctx context.Context, cfg image.Config, req Request) ([]byte, string, int, error) {
	// Codex uses the model id without the -image suffix (per JS: gpt-5.x-image
	// → upstream model drops -image).
	upstreamModel := strings.TrimSuffix(req.Model, "-image")
	url := strings.TrimRight(cfg.BaseURL, "/") + "/responses"
	payload := map[string]any{
		"model":  upstreamModel,
		"input":  []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": req.Prompt}}}},
		"stream": true,
		"tools": []any{
			map[string]any{
				"type":          "image_generation",
				"output_format": orDefault(req.OutputFormat, "png"),
				"size":          orDefault(req.Size, "1024x1024"),
				"quality":       orDefault(req.Quality, "auto"),
				"background":    orDefault(req.Background, "auto"),
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, "", http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	tok := credentialToken(req.Credentials)
	if tok != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}
	// chatgpt-account-id is carried in the credentials' providerSpecificData.
	if acct, _ := req.Credentials.ProviderSpecificData["chatgptAccountID"].(string); acct != "" {
		httpReq.Header.Set("chatgpt-account-id", acct)
	}
	if req.UserAgent != "" {
		httpReq.Header.Set("User-Agent", req.UserAgent)
	}
	resp, err := h.deps.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", resp.StatusCode, upstreamError(body)
	}
	// Parse the SSE stream for image base64 results.
	imgs, err := parseCodexSSE(resp.Body)
	if err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("codex: %w", err)
	}
	if len(imgs) == 0 {
		return nil, "", http.StatusBadGateway, fmt.Errorf("codex: no image in response")
	}
	data := make([]any, 0, len(imgs))
	for _, b64 := range imgs {
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

// parseCodexSSE reads the Codex SSE stream and collects image base64 payloads
// from response.image_generation_call.partial_image (b64 field) and
// response.output_item.done item.result (base64 string). The final
// output_item.done carries the complete image.
func parseCodexSSE(r io.Reader) ([]string, error) {
	var images []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20) // images are large
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "response.image_generation_call.partial_image":
			if b64, _ := ev["b64"].(string); b64 != "" {
				images = append(images, b64)
			}
		case "response.output_item.done":
			item, _ := ev["item"].(map[string]any)
			if res, _ := item["result"].(string); res != "" {
				images = append(images, res)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return images, err
	}
	return images, nil
}

// buildOpenAIBody constructs the OpenAI images/generations request body,
// optionally whitelisting fields via bodyFields (xai). n defaults to 1, size
// to "1024x1024" when not set.
func buildOpenAIBody(req Request, bodyFields []string) map[string]any {
	all := map[string]any{
		"model":  req.Model,
		"prompt": req.Prompt,
	}
	if req.N > 0 {
		all["n"] = req.N
	} else {
		all["n"] = 1
	}
	if req.Size != "" {
		all["size"] = req.Size
	} else {
		all["size"] = "1024x1024"
	}
	if req.Quality != "" {
		all["quality"] = req.Quality
	}
	if req.Style != "" {
		all["style"] = req.Style
	}
	if req.ResponseFormat == "url" || req.ResponseFormat == "b64_json" {
		all["response_format"] = req.ResponseFormat
	}
	if len(bodyFields) == 0 {
		return all
	}
	out := make(map[string]any, len(bodyFields))
	for _, f := range bodyFields {
		if v, ok := all[f]; ok {
			out[f] = v
		}
	}
	return out
}

// toBinary extracts the first image from an OpenAI-shape body and returns the
// raw decoded image bytes. If data[0].b64_json is set, decode it; if data[0].url
// is set, fetch it (deferred to a follow-up — returns an error for now). The
// Content-Type is derived from outputFormat (png/jpeg/webp, default png).
func (h *Handler) toBinary(openAIBody []byte, outputFormat string) ([]byte, string, int, error) {
	var parsed struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(openAIBody, &parsed); err != nil || len(parsed.Data) == 0 {
		return nil, "", http.StatusBadGateway, fmt.Errorf("no image data to emit as binary")
	}
	ext := orDefault(outputFormat, "png")
	ct := "image/" + ext
	if parsed.Data[0].B64JSON != "" {
		// Reuse base64 decode.
		return decodeBase64(parsed.Data[0].B64JSON), ct, http.StatusOK, nil
	}
	if parsed.Data[0].URL != "" {
		// URL fetch deferred — surface a clear 501.
		return nil, "", http.StatusNotImplemented, fmt.Errorf("binary output from url response_format not implemented; use b64_json")
	}
	return nil, "", http.StatusBadGateway, fmt.Errorf("no image data to emit as binary")
}

// === helpers ===

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func credentialToken(c domainProv.Credentials) string {
	if c.AccessToken != "" {
		return c.AccessToken
	}
	return c.APIKey
}

func setAuthHeader(r *http.Request, cfg image.Config, c domainProv.Credentials) {
	if cfg.AuthType == image.AuthTypeNone {
		return
	}
	tok := credentialToken(c)
	switch cfg.AuthHeader {
	case image.AuthBearer, image.AuthBearerAccount:
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
	case image.AuthKey:
		// Gemini uses ?key=, handled inline in synthGemini.
	case image.AuthXKey:
		if tok != "" {
			r.Header.Set("x-key", tok)
		}
	case image.AuthFalKey:
		if tok != "" {
			r.Header.Set("Authorization", "Key "+tok)
		}
	}
}

func upstreamError(body []byte) error {
	// OpenAI-shape error: {"error":{"message":...}}.
	var wrapped struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &wrapped) == nil && len(wrapped.Error) > 0 {
		var nested struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(wrapped.Error, &nested) == nil && nested.Message != "" {
			return fmt.Errorf("upstream: %s", nested.Message)
		}
		var s string
		if json.Unmarshal(wrapped.Error, &s) == nil && s != "" {
			return fmt.Errorf("upstream: %s", s)
		}
	}
	// Bare {"message":...} shape.
	var bare struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &bare) == nil && bare.Message != "" {
		return fmt.Errorf("upstream: %s", bare.Message)
	}
	// Raw string body.
	var raw json.RawMessage
	if json.Unmarshal(body, &raw) == nil {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return fmt.Errorf("upstream: %s", s)
		}
	}
	return fmt.Errorf("upstream error")
}

func decodeBase64(s string) []byte {
	// Try standard base64 first, then URL-safe (Gemini/Codex may emit either).
	if out, err := base64.StdEncoding.DecodeString(s); err == nil {
		return out
	}
	if out, err := base64.URLEncoding.DecodeString(s); err == nil {
		return out
	}
	out, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return out
}