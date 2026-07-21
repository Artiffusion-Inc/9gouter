package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/usecase/managedashboard"
)

// RegisterModels mounts model alias / custom / disabled / list routes.
func RegisterModels(mux *http.ServeMux, deps Deps) {
	h := &modelsHandler{
		deps: deps,
		svc:  &managedashboard.ModelService{AliasRepo: deps.Alias, DisabledRepo: deps.DisabledModels},
	}
	mux.HandleFunc("GET /api/models", h.list)
	mux.HandleFunc("PUT /api/models", h.setAlias)

	mux.HandleFunc("GET /api/models/alias", h.listAliases)
	mux.HandleFunc("PUT /api/models/alias", h.setAlias)
	mux.HandleFunc("DELETE /api/models/alias", h.deleteAlias)

	mux.HandleFunc("GET /api/models/custom", h.listCustom)
	mux.HandleFunc("POST /api/models/custom", h.addCustom)
	mux.HandleFunc("DELETE /api/models/custom", h.deleteCustom)

	mux.HandleFunc("GET /api/models/disabled", h.listDisabled)
	mux.HandleFunc("POST /api/models/disabled", h.disable)
	mux.HandleFunc("DELETE /api/models/disabled", h.enable)

	mux.HandleFunc("GET /api/models/availability", h.availability)
	mux.HandleFunc("POST /api/models/availability", h.clearCooldown)
	mux.HandleFunc("POST /api/models/test", h.test)
}

type modelsHandler struct {
	deps Deps
	svc  *managedashboard.ModelService
}

func (h *modelsHandler) list(w http.ResponseWriter, r *http.Request) {
	aliases, err := h.svc.Aliases(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch aliases")
		return
	}
	disabled, err := h.svc.Disabled(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch disabled models")
		return
	}
	// Static model list is not ported; return empty list with aliases/disabled.
	writeJSON(w, http.StatusOK, map[string]any{
		"models":   []any{},
		"aliases":  aliases,
		"disabled": disabled,
	})
}

func (h *modelsHandler) listAliases(w http.ResponseWriter, r *http.Request) {
	aliases, err := h.svc.Aliases(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch aliases")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"aliases": aliases})
}

type aliasRequest struct {
	Model string `json:"model"`
	Alias string `json:"alias"`
}

func (h *modelsHandler) setAlias(w http.ResponseWriter, r *http.Request) {
	var req aliasRequest
	if err := parseJSON(r, &req); err != nil || req.Model == "" || req.Alias == "" {
		writeError(w, http.StatusBadRequest, "Model and alias required")
		return
	}
	aliases, err := h.svc.Aliases(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch aliases")
		return
	}
	for existingModel, existingAlias := range aliases {
		if existingAlias == req.Alias && existingModel != req.Model {
			writeError(w, http.StatusBadRequest, "Alias already in use")
			return
		}
	}
	if err := h.svc.SetAlias(r.Context(), req.Model, req.Alias); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update alias")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "model": req.Model, "alias": req.Alias})
}

func (h *modelsHandler) deleteAlias(w http.ResponseWriter, r *http.Request) {
	alias := r.URL.Query().Get("alias")
	if alias == "" {
		writeError(w, http.StatusBadRequest, "Alias required")
		return
	}
	if err := h.svc.DeleteAlias(r.Context(), alias); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete alias")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *modelsHandler) listCustom(w http.ResponseWriter, r *http.Request) {
	models, err := h.svc.CustomModels(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch custom models")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

type customModelRequest struct {
	ProviderAlias string `json:"providerAlias"`
	ID            string `json:"id"`
	Type          string `json:"type"`
	Name          string `json:"name"`
}

func (h *modelsHandler) addCustom(w http.ResponseWriter, r *http.Request) {
	var req customModelRequest
	if err := parseJSON(r, &req); err != nil || req.ProviderAlias == "" || req.ID == "" {
		writeError(w, http.StatusBadRequest, "providerAlias and id required")
		return
	}
	added, err := h.svc.AddCustom(r.Context(), req.ProviderAlias, req.ID, req.Type, req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to add custom model")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "added": added})
}

func (h *modelsHandler) deleteCustom(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	providerAlias := q.Get("providerAlias")
	id := q.Get("id")
	typ := q.Get("type")
	if providerAlias == "" || id == "" {
		writeError(w, http.StatusBadRequest, "providerAlias and id required")
		return
	}
	if err := h.svc.DeleteCustom(r.Context(), providerAlias, id, typ); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete custom model")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *modelsHandler) listDisabled(w http.ResponseWriter, r *http.Request) {
	providerAlias := r.URL.Query().Get("providerAlias")
	if providerAlias != "" {
		ids, err := h.svc.DisabledByProvider(r.Context(), providerAlias)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to fetch disabled models")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ids": ids})
		return
	}
	all, err := h.svc.Disabled(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch disabled models")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disabled": all})
}

type disableRequest struct {
	ProviderAlias string   `json:"providerAlias"`
	IDs           []string `json:"ids"`
}

func (h *modelsHandler) disable(w http.ResponseWriter, r *http.Request) {
	var req disableRequest
	if err := parseJSON(r, &req); err != nil || req.ProviderAlias == "" || len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "providerAlias and ids[] required")
		return
	}
	if err := h.svc.Disable(r.Context(), req.ProviderAlias, req.IDs); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to disable models")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *modelsHandler) enable(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	providerAlias := q.Get("providerAlias")
	id := q.Get("id")
	if providerAlias == "" {
		writeError(w, http.StatusBadRequest, "providerAlias required")
		return
	}
	var ids []string
	if id != "" {
		ids = []string{id}
	}
	if err := h.svc.Enable(r.Context(), providerAlias, ids); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to enable models")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *modelsHandler) availability(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"models":           []any{},
		"unavailableCount": 0,
	})
}

func (h *modelsHandler) clearCooldown(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action   string `json:"action"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := parseJSON(r, &body); err != nil || body.Action != "clearCooldown" || body.Provider == "" || body.Model == "" {
		writeError(w, http.StatusBadRequest, "Invalid request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *modelsHandler) test(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
		Kind  string `json:"kind"`
	}
	if err := parseJSON(r, &body); err != nil || body.Model == "" {
		writeError(w, http.StatusBadRequest, "Model required")
		return
	}
	// Ping the model for real: dispatch a probe request through the own /v1/*
	// surface (chat completions / embeddings / images / audio transcriptions)
	// with an active dashboard API key, mirroring the legacy
	// src/app/api/models/test/ping.js pingModelByKind. This exercises the full
	// proxy + translator + provider stack, so a made-up model name fails
	// instead of always reporting success.
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	base := scheme + "://" + r.Host
	key := h.activeAPIKey(r.Context())
	result := pingModelByKind(r.Context(), base, key, body.Model, body.Kind)
	writeJSON(w, http.StatusOK, result)
}

// activeAPIKey returns the first active dashboard API key, or "" if none.
func (h *modelsHandler) activeAPIKey(ctx context.Context) string {
	if h.deps.APIKeys == nil {
		return ""
	}
	keys, err := h.deps.APIKeys.List(ctx)
	if err != nil {
		return ""
	}
	for _, k := range keys {
		if k.IsActive && k.Key != "" {
			return k.Key
		}
	}
	return ""
}

// pingModelByKind issues a probe request to the own /v1/* surface and reports
// reachability. Mirrors pingModelByKind in the legacy JS backend.
func pingModelByKind(ctx context.Context, base, apiKey, model, kind string) map[string]any {
	client := &http.Client{Timeout: 15 * time.Second}
	start := time.Now()

	do := func(path string, hdr http.Header, body io.Reader) (*http.Response, []byte, time.Duration) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, body)
		if err != nil {
			return nil, nil, 0
		}
		for k, vs := range hdr {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := client.Do(req)
		lat := time.Since(start)
		if err != nil {
			return nil, nil, lat
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, raw, lat
	}

	parse := func(raw []byte) map[string]any {
		var p map[string]any
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &p)
		}
		return p
	}

	fail := func(lat time.Duration, status int, msg string) map[string]any {
		return map[string]any{"ok": false, "latencyMs": lat.Milliseconds(), "status": status, "error": truncate(msg, 240)}
	}

	var path string
	var hdr http.Header
	var rd io.Reader

	switch kind {
	case "embedding":
		path = "/v1/embeddings"
		hdr = http.Header{"Content-Type": []string{"application/json"}}
		rd = strings.NewReader(`{"model":` + jsonStringValue(model) + `,"input":"test"}`)
	case "image":
		path = "/v1/images/generations"
		hdr = http.Header{"Content-Type": []string{"application/json"}}
		rd = strings.NewReader(`{"model":` + jsonStringValue(model) + `,"prompt":"test"}`)
	case "stt":
		path = "/v1/audio/transcriptions"
		// multipart audio transcription: build a minimal silent WAV.
		hdr = http.Header{} // Content-Type set by multipart writer
		body, ct := silentWavMultipart(model)
		hdr.Set("Content-Type", ct)
		rd = body
	default: // "llm" and unknown kinds -> chat completions
		path = "/v1/chat/completions"
		hdr = http.Header{"Content-Type": []string{"application/json"}}
		rd = strings.NewReader(`{"model":` + jsonStringValue(model) + `,"max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	}

	resp, raw, lat := do(path, hdr, rd)
	if resp == nil {
		return fail(lat, 0, "Network error: request failed")
	}
	p := parse(raw)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := jsonErrMessage(p)
		if detail == "" {
			detail = string(raw)
		}
		return fail(lat, resp.StatusCode, "HTTP "+strconv.Itoa(resp.StatusCode)+appendDetail(detail))
	}

	// Per-kind success validation: confirm the provider actually returned data
	// for this model, not just an empty 200 body.
	switch kind {
	case "embedding":
		if data, _ := p["data"].([]any); len(data) == 0 {
			return fail(lat, resp.StatusCode, "Provider returned no embedding data")
		}
	case "image":
		if data, _ := p["data"].([]any); len(data) == 0 {
			return fail(lat, resp.StatusCode, "Provider returned no image data for this model")
		}
	case "stt":
		if t, _ := p["text"].(string); strings.TrimSpace(t) == "" {
			return fail(lat, resp.StatusCode, "Provider returned no transcription text for this model")
		}
	default:
		if status, hasStatus := p["status"]; hasStatus {
			ps := fmt.Sprint(status)
			if ps != "" && ps != "200" && ps != "0" {
				if msg := jsonMsg(p); msg != "" {
					return fail(lat, resp.StatusCode, "Provider status "+ps+": "+msg)
				}
			}
		}
		if e, ok := p["error"].(map[string]any); ok {
			if m, _ := e["message"].(string); m != "" {
				return fail(lat, resp.StatusCode, m)
			}
		} else if e, ok := p["error"].(string); ok && e != "" {
			return fail(lat, resp.StatusCode, e)
		}
		if choices, _ := p["choices"].([]any); len(choices) == 0 {
			return fail(lat, resp.StatusCode, "Provider returned no completion choices for this model")
		}
	}

	return map[string]any{"ok": true, "latencyMs": lat.Milliseconds(), "error": nil, "status": resp.StatusCode}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func appendDetail(d string) string {
	if d == "" {
		return ""
	}
	return ": " + truncate(d, 240)
}

func jsonStringValue(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func jsonErrMessage(p map[string]any) string {
	if e, ok := p["error"].(map[string]any); ok {
		if m, _ := e["message"].(string); m != "" {
			return m
		}
	}
	for _, k := range []string{"msg", "message"} {
		if s, ok := p[k].(string); ok && s != "" {
			return s
		}
	}
	if s, ok := p["error"].(string); ok {
		return s
	}
	return ""
}

func jsonMsg(p map[string]any) string {
	for _, k := range []string{"msg", "message"} {
		if s, ok := p[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// silentWavMultipart builds a minimal silent WAV file wrapped in a multipart
// form body for the /v1/audio/transcriptions probe, mirroring the legacy
// createSilentWavFile() + FormData approach.
func silentWavMultipart(model string) (io.Reader, string) {
	const sampleRate = 16000
	const channels = 1
	const bitsPerSample = 16
	const durationMs = 250
	sampleCount := 1
	if sampleRate*durationMs/1000 > 0 {
		sampleCount = sampleRate * durationMs / 1000
	}
	dataSize := sampleCount * channels * (bitsPerSample / 8)
	var wav bytes.Buffer
	wav.Grow(44 + dataSize)
	wavb := &wav
	// Writes to a *bytes.Buffer never fail, but errcheck cannot prove it.
	// Discard the error explicitly so the write helpers stay unchecked-clean.
	_ = binary.Write(wavb, binary.LittleEndian, []byte("RIFF"))
	_ = binary.Write(wavb, binary.LittleEndian, uint32(36+dataSize))
	_, _ = wavb.Write([]byte("WAVE"))
	_, _ = wavb.Write([]byte("fmt "))
	_ = binary.Write(wavb, binary.LittleEndian, uint32(16))
	_ = binary.Write(wavb, binary.LittleEndian, uint16(1))
	_ = binary.Write(wavb, binary.LittleEndian, uint16(channels))
	_ = binary.Write(wavb, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(wavb, binary.LittleEndian, uint32(sampleRate*channels*(bitsPerSample/8)))
	_ = binary.Write(wavb, binary.LittleEndian, uint16(channels*(bitsPerSample/8)))
	_ = binary.Write(wavb, binary.LittleEndian, uint16(bitsPerSample))
	_, _ = wavb.Write([]byte("data"))
	_ = binary.Write(wavb, binary.LittleEndian, uint32(dataSize))
	_, _ = wavb.Write(make([]byte, dataSize))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "test.wav")
	if err == nil {
		_, _ = fw.Write(wav.Bytes())
	}
	_ = mw.WriteField("model", model)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

var _ = json.Marshal
