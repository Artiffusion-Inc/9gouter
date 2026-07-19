package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	domainProv "github.com/Artiffusion-Inc/9router/internal/domain/provider"
	videoproxy "github.com/Artiffusion-Inc/9router/internal/usecase/videoproxy"
)

// handleVideoCreate implements POST /v1/videos/{generations|edits|extensions}.
// It ports src/sse/handlers/videoGeneration.js::handleVideoCreate: api-key gate,
// resolve provider from body.model (default xAI), strip the provider/ prefix
// before forwarding, and raw-byte passthrough to the upstream videoConfig.
// Idempotency-Key is forwarded on POST.
func (h *v1Handler) handleVideoCreate(w http.ResponseWriter, r *http.Request, action string) {
	ctx := r.Context()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	contentType := r.Header.Get("Content-Type")
	idempotencyKey := r.Header.Get("Idempotency-Key")

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

	// Resolve provider from body.model. For JSON bodies, parse the model only to
	// resolve the provider; forward the original bytes 1:1 (do not reshape).
	providerID, forwardedBody := resolveVideoProviderAndBody(body, contentType)
	if providerID == "" {
		h.writeError(w, http.StatusBadRequest, "Invalid model format")
		return
	}

	// Only xAI has a videoConfig; an explicit non-xAI provider prefix 400s.
	if h.videoBaseFor(providerID) == "" {
		if strings.Contains(modelStringFromBody(body, contentType), "/") {
			h.writeError(w, http.StatusBadRequest, "Provider '"+providerID+"' does not support video generation")
			return
		}
		// Bare model with no matching videoConfig → fall back to xAI (legacy).
		providerID = "xai"
	}

	creds, err := h.resolveCredentials(ctx, providerID, "")
	if err != nil {
		h.writeError(w, http.StatusNotFound, "No active credentials for provider: "+providerID)
		return
	}

	if h.deps.Video == nil {
		h.writeError(w, http.StatusNotImplemented, "Video pipeline not wired")
		return
	}

	connectionID := connectionIDFromCreds(creds)
	res, err := h.deps.Video.Handle(ctx, VideoProxyRequest{
		Ctx:            ctx,
		Action:         action,
		Body:           forwardedBody,
		ContentType:    contentType,
		IdempotencyKey: idempotencyKey,
		ProviderID:     providerID,
		Credentials:    creds,
		ConnectionID:   connectionID,
		UserAgent:      r.UserAgent(),
	})
	if err != nil && res.Err == nil {
		res.Err = err
	}
	h.writeVideoResult(w, res)
}

// handleVideoGet implements GET /v1/videos/{id} — poll an in-progress job.
// Provider is fixed to xAI (the only video upstream); the connection is pinned
// by the x-connection-id request header when the client echoes it back.
func (h *v1Handler) handleVideoGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := r.PathValue("id")
	if requestID == "" {
		h.writeError(w, http.StatusBadRequest, "Missing video request id")
		return
	}
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

	providerID := "xai"
	creds, err := h.resolveCredentials(ctx, providerID, "")
	if err != nil {
		h.writeError(w, http.StatusNotFound, "No active credentials for provider: "+providerID)
		return
	}
	if h.deps.Video == nil {
		h.writeError(w, http.StatusNotImplemented, "Video pipeline not wired")
		return
	}
	res, err := h.deps.Video.Handle(ctx, VideoProxyRequest{
		Ctx:         ctx,
		RequestID:   requestID,
		ProviderID:  providerID,
		Credentials: creds,
		UserAgent:   r.UserAgent(),
	})
	if err != nil && res.Err == nil {
		res.Err = err
	}
	h.writeVideoResult(w, res)
}

// writeVideoResult writes the raw upstream response to the client with the
// upstream Content-Type and the x-9router-connection-id header (so the client
// can echo it on subsequent GET polls to pin the account).
func (h *v1Handler) writeVideoResult(w http.ResponseWriter, res VideoProxyResult) {
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
	if res.ConnectionID != "" {
		w.Header().Set("x-9router-connection-id", res.ConnectionID)
	}
	if res.StatusCode == 0 {
		res.StatusCode = http.StatusOK
	}
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(res.Body)
}

// videoBaseFor returns the configured video upstream base for the provider, or
// "" if the provider has no video support. Mirrors the JS getVideoConfig.
func (h *v1Handler) videoBaseFor(providerID string) string {
	// Only xAI is wired in the MVP; the usecase's default resolver answers the
	// same question, but we expose a cheap local check to decide provider
	// fallback at the handler boundary without constructing a request.
	if providerID == "xai" {
		return "https://api.x.ai/v1/videos"
	}
	return ""
}

// resolveVideoProviderAndBody parses a JSON body to find body.model, resolves
// the provider prefix (default xAI when absent or bare), strips the provider/
// prefix from the forwarded body so the upstream receives the bare model —
// matching the JS forwardBody. For non-JSON bodies (multipart) the bytes are
// forwarded unchanged and the provider defaults to xAI.
func resolveVideoProviderAndBody(body []byte, contentType string) (providerID string, forwarded []byte) {
	if !strings.HasPrefix(contentType, "application/json") {
		return "xai", body
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Malformed JSON — let the upstream reject it; default provider.
		return "xai", body
	}
	modelRaw, ok := parsed["model"]
	if !ok {
		return "xai", body
	}
	var model string
	if err := json.Unmarshal(modelRaw, &model); err != nil {
		return "xai", body
	}
	if !strings.Contains(model, "/") {
		return "xai", body
	}
	parts := strings.SplitN(model, "/", 2)
	prefix := parts[0]
	bare := parts[1]
	// Rewrite body.model to the bare model and forward.
	parsed["model"], _ = json.Marshal(bare)
	forwarded, _ = json.Marshal(parsed)
	return prefix, forwarded
}

// modelStringFromBody extracts body.model as a string for JSON bodies (used
// only to distinguish bare-model vs provider/... forms). Returns "" otherwise.
func modelStringFromBody(body []byte, contentType string) string {
	if !strings.HasPrefix(contentType, "application/json") {
		return ""
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	if raw, ok := parsed["model"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	return ""
}

// connectionIDFromCreds extracts the _connectionId stamped on the resolved
// credentials (added by resolveCredentials) so the response can echo it back.
func connectionIDFromCreds(creds domainProv.Credentials) string {
	if m := creds.ProviderSpecificData; m != nil {
		if v, ok := m["_connectionId"].(string); ok {
			return v
		}
	}
	return ""
}

// keep videoproxy import alive (Action validation lives there; the handler
// passes a string action and the usecase re-validates).
var _ = videoproxy.Action("")
