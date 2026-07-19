package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	ttsprov "github.com/Artiffusion-Inc/9router/internal/adapter/provider/tts"
)

// ttsMaxBodyBytes caps the JSON request body read for /v1/audio/speech. TTS
// requests carry text (small) — the value mirrors the legacy
// NINEROUTER_PROXY_CLIENT_MAX_BODY_SIZE ceiling but a hard cap is kept as a
// guard against pathological inputs.
const ttsMaxBodyBytes int64 = 16 << 20

// ttsRequestBody is the OpenAI-compatible /v1/audio/speech request body. Only
// the fields the usecase consumes are parsed; unknown fields are ignored.
type ttsRequestBody struct {
	Model          string `json:"model"`
	Input          string `json:"input"`
	Language       string `json:"language"`
	ResponseFormat string `json:"response_format"`
}

// handleAudioSpeech implements POST /v1/audio/speech — the OpenAI
// TTS-compatible endpoint. It ports src/sse/handlers/tts.js (handleTts) +
// open-sse/handlers/ttsCore.js: parse the JSON body, validate the API key,
// resolve the provider from body.model (provider/model prefix → strip; bare
// model → openai fallback), then dispatch to the ttsproxy usecase.
//
// response_format is taken from the JSON body, falling back to the
// ?response_format= query param, then "mp3" (raw binary) — matching the JS
// handler precedence. Like STT, TTS does not echo x-9router-connection-id.
func (h *v1Handler) handleAudioSpeech(w http.ResponseWriter, r *http.Request) {
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

	// Parse the JSON body.
	var body ttsRequestBody
	if err := json.NewDecoder(io.LimitReader(r.Body, ttsMaxBodyBytes)).Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	body.Model = strings.TrimSpace(body.Model)
	if body.Model == "" {
		h.writeError(w, http.StatusBadRequest, "Missing required field: model")
		return
	}
	body.Input = strings.TrimSpace(body.Input)
	if body.Input == "" {
		h.writeError(w, http.StatusBadRequest, "Missing required field: input")
		return
	}

	// response_format precedence: body → query → "mp3".
	responseFormat := strings.TrimSpace(body.ResponseFormat)
	if responseFormat == "" {
		responseFormat = strings.TrimSpace(r.URL.Query().Get("response_format"))
	}
	if responseFormat == "" {
		responseFormat = "mp3"
	}

	// Resolve provider from model. "provider/model" → provider prefix +
	// bare model; bare model → openai (the canonical OpenAI-TTS default).
	providerID, bareModel := resolveTtsProvider(body.Model)
	if providerID == "" {
		h.writeError(w, http.StatusBadRequest, "Could not resolve TTS provider from model: "+body.Model)
		return
	}

	creds, err := h.resolveCredentials(ctx, providerID, "")
	if err != nil {
		h.writeError(w, http.StatusNotFound, "No active credentials for provider: "+providerID)
		return
	}

	if h.deps.Tts == nil {
		h.writeError(w, http.StatusNotImplemented, "TTS pipeline not wired")
		return
	}

	res, err := h.deps.Tts.Handle(ctx, TtsRequest{
		Ctx:            ctx,
		ProviderID:     providerID,
		Model:          bareModel,
		Input:          body.Input,
		Language:       body.Language,
		ResponseFormat: responseFormat,
		Credentials:    creds,
		UserAgent:      r.UserAgent(),
	})
	if err != nil && res.Err == nil {
		res.Err = err
	}
	h.writeTtsResult(w, res)
}

// writeTtsResult writes the synthesized audio response to the client with the
// usecase-supplied Content-Type and CORS, mirroring the JS passthrough helper.
// TTS does not emit x-9router-connection-id.
func (h *v1Handler) writeTtsResult(w http.ResponseWriter, res TtsResult) {
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
	if res.StatusCode == 0 {
		res.StatusCode = http.StatusOK
	}
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(res.Body)
}

// resolveTtsProvider splits a "provider/model[/voice]" string into its parts.
// For a bare model (no "/" or a first segment that is not a known TTS
// provider — e.g. "gpt-4o-mini-tts/alloy" where the first segment is a model,
// not a provider), it falls back to openai (the canonical OpenAI-TTS
// provider). The bare model is returned verbatim in that case so the usecase
// can apply the provider's DefaultModel/DefaultVoice via parseModelVoice.
func resolveTtsProvider(modelStr string) (providerID, bareModel string) {
	if !strings.Contains(modelStr, "/") {
		return openaiOrDefault(modelStr)
	}
	parts := strings.SplitN(modelStr, "/", 2)
	first := parts[0]
	// Only treat the first segment as a provider prefix if it is actually a
	// known TTS provider. Otherwise the whole string is a bare "model/voice"
	// (e.g. OpenAI "gpt-4o-mini-tts/alloy") and we fall back to openai.
	if _, ok := ttsprov.Lookup(first); ok {
		return first, parts[1]
	}
	return openaiOrDefault(modelStr)
}

// openaiOrDefault returns the openai provider id (or a fallback) with the
// model passed through verbatim, mirroring the bare-model fallback in the
// STT handler.
func openaiOrDefault(bareModel string) (string, string) {
	if cfg, ok := ttsprov.Lookup("openai"); ok && cfg.Format != "" {
		return "openai", bareModel
	}
	for _, id := range ttsKnownProviders {
		if cfg, ok := ttsprov.Lookup(id); ok && cfg.Format != "" {
			return id, bareModel
		}
	}
	return "", bareModel
}

// ttsKnownProviders is the static set of provider ids with a TTS config, used
// for the bare-model fallback scan. It mirrors the tts package registry.
var ttsKnownProviders = []string{
	"openai", "gemini", "elevenlabs", "minimax", "minimax-cn",
	"inworld", "cartesia", "playht", "nvidia", "deepgram",
}