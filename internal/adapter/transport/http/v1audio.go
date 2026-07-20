package http

import (
	"io"
	"net/http"
	"strings"

	sttprov "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/stt"
)

// sttMaxUploadBytes caps the in-memory multipart parse budget and the file
// read. It mirrors the legacy NINEROUTER_PROXY_CLIENT_MAX_BODY_SIZE default of
// 128mb; the config-driven body limit is enforced by the proxy layer upstream
// of the handler.
const sttMaxUploadBytes int64 = 128 << 20

// handleAudioTranscriptions implements POST /v1/audio/transcriptions — the
// OpenAI Whisper-compatible STT endpoint. It ports src/sse/handlers/stt.js
// (handleStt) + open-sse/handlers/sttCore.js: parse the multipart form,
// validate the API key, resolve the provider from the `model` form field
// (provider/model prefix → strip; bare model → first provider with an STT
// config + active connection), then dispatch to the sttproxy usecase.
//
// Unlike video, STT does not echo an x-9gouter-connection-id header (the JS
// handler does not). The body limit is governed by the proxy
// ProxyClientMaxBodySize (128mb default), matching the legacy
// NINEROUTER_PROXY_CLIENT_MAX_BODY_SIZE.
func (h *v1Handler) handleAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
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

	// Parse the multipart form. The max in-memory parse is bounded by the
	// same ProxyClientMaxBodySize as the rest of the proxy; the file part is
	// read into memory (STT files are small-to-medium, well under 128mb).
	maxBytes := sttMaxUploadBytes
	if err := r.ParseMultipartForm(maxBytes); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid multipart form data")
		return
	}

	modelStr := strings.TrimSpace(r.FormValue("model"))
	if modelStr == "" {
		h.writeError(w, http.StatusBadRequest, "Missing required field: model")
		return
	}

	// Resolve provider from model. "provider/model" → provider prefix +
	// bare model; bare model → first provider with an STT config.
	providerID, bareModel := resolveSttProvider(modelStr)
	if providerID == "" {
		h.writeError(w, http.StatusBadRequest, "Could not resolve STT provider from model: "+modelStr)
		return
	}

	// Read the uploaded file.
	file, header, err := r.FormFile("file")
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Missing required field: file")
		return
	}
	defer file.Close()
	audioBytes, err := io.ReadAll(io.LimitReader(file, maxBytes))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Failed to read audio file")
		return
	}

	// Collect the non-file form fields the usecase cares about (and pass the
	// rest through under their original keys for the openai-compatible shape).
	formFields := make(map[string]string, len(r.PostForm))
	for k, v := range r.PostForm {
		if len(v) > 0 {
			formFields[k] = v[0]
		}
	}

	creds, err := h.resolveCredentials(ctx, providerID, "")
	if err != nil {
		h.writeError(w, http.StatusNotFound, "No active credentials for provider: "+providerID)
		return
	}

	if h.deps.Stt == nil {
		h.writeError(w, http.StatusNotImplemented, "STT pipeline not wired")
		return
	}

	res, err := h.deps.Stt.Handle(ctx, SttRequest{
		Ctx:         ctx,
		ProviderID:  providerID,
		Model:       bareModel,
		File:        audioBytes,
		Filename:    header.Filename,
		FileMIME:    header.Header.Get("Content-Type"),
		FormFields:  formFields,
		Credentials: creds,
		UserAgent:   r.UserAgent(),
	})
	if err != nil && res.Err == nil {
		res.Err = err
	}
	h.writeSttResult(w, res)
}

// writeSttResult writes the upstream transcription response to the client with
// the upstream Content-Type and CORS, mirroring the JS jsonResponse / passthrough
// helpers. STT does not emit x-9gouter-connection-id.
func (h *v1Handler) writeSttResult(w http.ResponseWriter, res SttResult) {
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

// resolveSttProvider splits a "provider/model" string into its parts. For a
// bare model (no "/"), it scans the static STT provider registry and returns
// the first provider id whose STT config exists (the caller will then fail
// the credential lookup if that provider has no active connection). The bare
// model is returned verbatim in that case.
func resolveSttProvider(modelStr string) (providerID, bareModel string) {
	if !strings.Contains(modelStr, "/") {
		// Bare model: pick the first STT-capable provider. The static registry
		// is small (openai/groq/deepgram/gemini/assemblyai); openai is the
		// canonical default for Whisper-style models.
		if cfg, ok := sttprov.Lookup("openai"); ok && cfg.Format != "" {
			return "openai", modelStr
		}
		// Fallback: any provider with an STT config.
		for _, id := range sttKnownProviders {
			if cfg, ok := sttprov.Lookup(id); ok && cfg.Format != "" {
				return id, modelStr
			}
		}
		return "", modelStr
	}
	parts := strings.SplitN(modelStr, "/", 2)
	return parts[0], parts[1]
}

// sttKnownProviders is the static set of provider ids with an STT config, used
// for the bare-model fallback scan. It mirrors the stt package registry.
var sttKnownProviders = []string{"openai", "groq", "deepgram", "gemini", "assemblyai"}
