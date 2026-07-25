package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	imageprov "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/image"
	"github.com/Artiffusion-Inc/9gouter/internal/usecase/imageproxy"
)

// imageMaxBodyBytes caps the JSON request body read for
// /v1/images/generations. The envelope may carry base64-encoded image/mask
// inputs; the spec allows up to 24 MiB for the JSON/base64 envelope. The safe
// input resolver (step 4) independently caps each decoded image at 16 MiB.
const imageMaxBodyBytes int64 = 24 << 20

// imagesRequestBody is the OpenAI-compatible /v1/images/generations request
// body. Base fields are typed; extended fields are json.RawMessage so the
// handler can distinguish an absent key (nil) from a supplied null/""/0
// (non-nil) — the capability table (image_capabilities.go) treats any
// non-nil value as supplied and rejects it unless the provider/model row
// authorises that field.
type imagesRequestBody struct {
	// Base fields (existing OpenAI contract).
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	Quality        string `json:"quality"`
	Style          string `json:"style"`
	ResponseFormat string `json:"response_format"`
	OutputFormat   string `json:"output_format"`
	Background     string `json:"background"`

	// Extended image inputs (presence-bearing). nil = key absent.
	Image  json.RawMessage `json:"image"`
	Images json.RawMessage `json:"images"`

	// Mask aliases (mutually exclusive). At most one may be supplied.
	MaskImage json.RawMessage `json:"mask_image"`
	MaskCamel json.RawMessage `json:"maskImage"`
	Mask      json.RawMessage `json:"mask"`

	// Dimension overrides (Cloudflare JSON / providers that use separate
	// width/height instead of a size string).
	Width  json.RawMessage `json:"width"`
	Height json.RawMessage `json:"height"`

	// Six named Cloudflare-ish optional fields.
	NegativePrompt json.RawMessage `json:"negative_prompt"`
	Guidance       json.RawMessage `json:"guidance"`
	Seed           json.RawMessage `json:"seed"`
	NumSteps       json.RawMessage `json:"num_steps"`
	Steps          json.RawMessage `json:"steps"`
	Strength       json.RawMessage `json:"strength"`
}

// handleImagesGenerations implements POST /v1/images/generations — the
// OpenAI image-generation-compatible endpoint. It ports
// src/sse/handlers/imageGeneration.js (handleImageGeneration): parse the JSON
// body, validate the API key, resolve the provider from body.model
// (provider/model prefix → strip; bare model → openai fallback), then
// dispatch to the imageproxy usecase.
//
// response_format precedence: body → ?response_format= query → "url". The
// "binary" value is a 9gouter-internal flag (not an OpenAI field) that returns
// raw image bytes; output_format then selects the Content-Type (png/jpeg/webp,
// default png). Like chat, image gen echoes x-9gouter-connection-id so the
// dashboard can pin the connection.
//
// Step 3 (image-provider-parity): the handler enforces the default-deny
// capability table (image_capabilities.go) BEFORE the executor, canonicalises
// the three mask aliases into a single mask, and guards the no-auth local
// providers (sdwebui/comfyui) against external viewers with a 403 returned
// before the image usecase is called.
func (h *v1Handler) handleImagesGenerations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Decode the body first so the provider can be resolved before the auth
	// gate: the no-auth local providers (sdwebui/comfyui) must reject external
	// viewers with 403 regardless of API-key presence, and a missing model
	// should report 400 before auth too.
	var body imagesRequestBody
	if err := json.NewDecoder(io.LimitReader(r.Body, imageMaxBodyBytes)).Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	body.Model = strings.TrimSpace(body.Model)
	if body.Model == "" {
		h.writeError(w, http.StatusBadRequest, "Missing required field: model")
		return
	}
	body.Prompt = strings.TrimSpace(body.Prompt)
	if body.Prompt == "" {
		h.writeError(w, http.StatusBadRequest, "Missing required field: prompt")
		return
	}

	// Resolve provider from model. "provider/model" → provider prefix only when
	// the first segment is a known image provider; bare model → openai fallback.
	providerID, bareModel := resolveImageProvider(body.Model)
	if providerID == "" {
		h.writeError(w, http.StatusBadRequest, "Could not resolve image provider from model: "+body.Model)
		return
	}

	// Local guard (before auth gate and executor): sdwebui/comfyui are no-auth
	// loopback-only providers. An external viewer (non-loopback remote address,
	// or a request that arrived through the dashboard proxy stamp X-9r-Via-Proxy)
	// gets a 403 BEFORE the auth gate, the credential resolution, and the image
	// usecase — so the guard is visible even before step 5 lifts the Unsupported
	// flag. This deliberately precedes the API-key gate: a loopback-only service
	// must not be reachable by an external viewer under any auth state.
	if isNoAuthImageProvider(providerID) && !isLocalRequest(r) {
		h.writeError(w, http.StatusForbidden, "local image provider only accessible from loopback viewer")
		return
	}

	// API-key gate (same as /v1/chat).
	apiKey := extractAPIKey(r)
	requireKey, err := h.requireAPIKey(ctx)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Auth check failed")
		return
	}
	if requireKey || !isLocalRequest(r) {
		if apiKey == "" {
			h.writeError(w, http.StatusUnauthorized, "Missing API key")
			return
		}
		valid, err := h.deps.APIKeysRepo.Validate(ctx, apiKey)
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, "Auth check failed")
			return
		}
		if !valid {
			h.writeError(w, http.StatusUnauthorized, "Invalid API key")
			return
		}
	}

	// response_format precedence: body → query → "url".
	responseFormat := strings.TrimSpace(body.ResponseFormat)
	if responseFormat == "" {
		responseFormat = strings.TrimSpace(r.URL.Query().Get("response_format"))
	}
	if responseFormat == "" {
		responseFormat = "url"
	}

	// Canonicalise mask aliases before the capability check: at most one of
	// mask_image/maskImage/mask may be supplied. The capability table then
	// decides whether a mask is permitted for this provider/model.
	supplied := suppliedImageFields{
		image:          body.Image,
		images:         body.Images,
		width:          body.Width,
		height:         body.Height,
		negativePrompt: body.NegativePrompt,
		guidance:       body.Guidance,
		seed:           body.Seed,
		numSteps:       body.NumSteps,
		steps:          body.Steps,
		strength:       body.Strength,
		maskImage:      body.MaskImage,
		maskCamel:      body.MaskCamel,
		mask:           body.Mask,
	}
	canonicalMask, maskSupplied, err := canonicalizeMask(supplied)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Capability table (default-deny). The first matching row wins; any supplied
	// field not authorised by the row is rejected with 400 before the executor.
	cap, ok := imageCapabilities(providerID, bareModel)
	if !ok {
		// Unknown provider to the matrix: still default-deny all extended fields.
		cap = imageCapability{provider: providerID}
	}
	if cerr := checkImageCapabilities(providerID, bareModel, cap, supplied, maskSupplied); cerr != nil {
		h.writeError(w, http.StatusBadRequest, cerr.Error())
		return
	}

	// Preferred connection pin (x-9gouter-connection-id). The same credential
	// resolution path chat uses for combos honours the pin so the image lifecycle
	// runs on the operator-selected connection/account. For no-auth providers
	// (sdwebui/comfyui) the resolver returns virtual credentials and the pin is
	// ignored (resolveCredentialsWithOpts short-circuits before the pin lookup).
	preferredConnID := strings.TrimSpace(r.Header.Get("x-9gouter-connection-id"))
	creds, err := h.resolveCredentialsWithOpts(ctx, providerID, "", nil, preferredConnID)
	if err != nil {
		h.writeError(w, http.StatusNotFound, "No active credentials for provider: "+providerID)
		return
	}

	// Local guard: sdwebui/comfyui are no-auth loopback-only providers. An
	// external viewer (non-loopback remote address, or a request that arrived
	// through the dashboard proxy stamp X-9r-Via-Proxy) gets a 403 BEFORE the
	// image usecase is called — earlier than the 501 the Unsupported flag would
	// produce, so the guard is visible even before step 5 lifts the flag.
	if isNoAuthImageProvider(providerID) && !isLocalRequest(r) {
		h.writeError(w, http.StatusForbidden, "local image provider only accessible from loopback viewer")
		return
	}

	if h.deps.Image == nil {
		h.writeError(w, http.StatusNotImplemented, "Image generation pipeline not wired")
		return
	}

	// Build the provider-specific Options from the supplied, capability-
	// authorised fields. The safe input resolver (step 4) will convert
	// RawImageInputs/RawMask into typed ImageInputs/Mask; for step 3 the raw
	// values are forwarded so the adapter probe can observe presence.
	opts := imageproxy.RequestOptions{
		RawImageInputs: rawImageInputs(supplied),
		RawMask:        canonicalMask,
		Width:          supplied.width,
		Height:         supplied.height,
		NegativePrompt: supplied.negativePrompt,
		Guidance:       supplied.guidance,
		Seed:           supplied.seed,
		NumSteps:       supplied.numSteps,
		Steps:          supplied.steps,
		Strength:       supplied.strength,
	}

	res, err := h.deps.Image.Handle(ctx, ImageRequest{
		Ctx:                   ctx,
		ProviderID:            providerID,
		Model:                 bareModel,
		Prompt:                body.Prompt,
		N:                     body.N,
		Size:                  body.Size,
		Quality:               body.Quality,
		Style:                 body.Style,
		ResponseFormat:        responseFormat,
		OutputFormat:          body.OutputFormat,
		Background:            body.Background,
		Credentials:           creds,
		UserAgent:             r.UserAgent(),
		PreferredConnectionID: preferredConnID,
		Options:               opts,
	})
	if err != nil && res.Err == nil {
		res.Err = err
	}
	h.writeImageResult(w, res, creds.ProviderSpecificData["_connectionId"])
}

// writeImageResult writes the generated image response to the client with the
// usecase-supplied Content-Type, CORS, and x-9gouter-connection-id (mirroring
// the JS image handler, which echoes the connection pin).
func (h *v1Handler) writeImageResult(w http.ResponseWriter, res ImageResult, connID any) {
	if res.Err != nil {
		status := res.StatusCode
		if status == 0 {
			status = http.StatusBadGateway
		}
		h.writeError(w, status, res.Err.Error())
		return
	}
	if res.ContentType != "" {
		w.Header().Set("Content-Type", res.ContentType)
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if id, ok := connID.(string); ok && id != "" {
		w.Header().Set("x-9gouter-connection-id", id)
	}
	if res.StatusCode == 0 {
		res.StatusCode = http.StatusOK
	}
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(res.Body)
}

// resolveImageProvider splits a "provider/model" string into its parts. For a
// bare model (no "/" or a first segment that is not a known image provider —
// e.g. "dall-e-3" or "gpt-image-1"), it falls back to openai (the canonical
// OpenAI-image default).
func resolveImageProvider(modelStr string) (providerID, bareModel string) {
	if !strings.Contains(modelStr, "/") {
		return openaiOrDefault(modelStr)
	}
	parts := strings.SplitN(modelStr, "/", 2)
	first := parts[0]
	if _, ok := imageprov.Lookup(first); ok {
		return first, parts[1]
	}
	return openaiOrDefault(modelStr)
}
