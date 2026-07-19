package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	imageprov "github.com/Artiffusion-Inc/9router/internal/adapter/provider/image"
)

// imageMaxBodyBytes caps the JSON request body read for
// /v1/images/generations. Image prompts are small; the cap is a guard.
const imageMaxBodyBytes int64 = 16 << 20

// imagesRequestBody is the OpenAI-compatible /v1/images/generations request
// body. Only the fields the usecase consumes are parsed; unknown fields are
// ignored.
type imagesRequestBody struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	Quality        string `json:"quality"`
	Style          string `json:"style"`
	ResponseFormat string `json:"response_format"`
	OutputFormat   string `json:"output_format"`
	Background     string `json:"background"`
}

// handleImagesGenerations implements POST /v1/images/generations — the
// OpenAI image-generation-compatible endpoint. It ports
// src/sse/handlers/imageGeneration.js (handleImageGeneration): parse the JSON
// body, validate the API key, resolve the provider from body.model
// (provider/model prefix → strip; bare model → openai fallback), then
// dispatch to the imageproxy usecase.
//
// response_format precedence: body → ?response_format= query → "url". The
// "binary" value is a 9router-internal flag (not an OpenAI field) that returns
// raw image bytes; output_format then selects the Content-Type (png/jpeg/webp,
// default png). Like chat, image gen echoes x-9router-connection-id so the
// dashboard can pin the connection.
func (h *v1Handler) handleImagesGenerations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

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

	// response_format precedence: body → query → "url".
	responseFormat := strings.TrimSpace(body.ResponseFormat)
	if responseFormat == "" {
		responseFormat = strings.TrimSpace(r.URL.Query().Get("response_format"))
	}
	if responseFormat == "" {
		responseFormat = "url"
	}

	// Resolve provider from model. "provider/model" → provider prefix only when
	// the first segment is a known image provider; bare model → openai fallback.
	providerID, bareModel := resolveImageProvider(body.Model)
	if providerID == "" {
		h.writeError(w, http.StatusBadRequest, "Could not resolve image provider from model: "+body.Model)
		return
	}

	creds, err := h.resolveCredentials(ctx, providerID, "")
	if err != nil {
		h.writeError(w, http.StatusNotFound, "No active credentials for provider: "+providerID)
		return
	}

	if h.deps.Image == nil {
		h.writeError(w, http.StatusNotImplemented, "Image generation pipeline not wired")
		return
	}

	res, err := h.deps.Image.Handle(ctx, ImageRequest{
		Ctx:            ctx,
		ProviderID:     providerID,
		Model:          bareModel,
		Prompt:         body.Prompt,
		N:              body.N,
		Size:           body.Size,
		Quality:        body.Quality,
		Style:          body.Style,
		ResponseFormat: responseFormat,
		OutputFormat:   body.OutputFormat,
		Background:     body.Background,
		Credentials:    creds,
		UserAgent:      r.UserAgent(),
	})
	if err != nil && res.Err == nil {
		res.Err = err
	}
	h.writeImageResult(w, res, creds.ProviderSpecificData["_connectionId"])
}

// writeImageResult writes the generated image response to the client with the
// usecase-supplied Content-Type, CORS, and x-9router-connection-id (mirroring
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
		w.Header().Set("x-9router-connection-id", id)
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