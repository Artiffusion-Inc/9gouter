package http

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/settings"
)

// v1beta implements the Gemini-native /v1beta/models surface, porting legacy
// JS src/app/api/v1beta/models/route.js (GET list) and
// src/app/api/v1beta/models/[...path]/route.js (POST generateContent /
// streamGenerateContent). The @google/genai SDK talks this surface directly
// (models/<id>:generateContent, models/<id>:streamGenerateContent?alt=sse).
//
// Two routes are registered:
//
//   GET  /v1beta/models                 — Gemini-shaped model catalog, built
//                                         from provider.AllCatalogs().
//   POST /v1beta/models/{path...}        — proxy: convert the Gemini request
//                                         body to the internal/OpenAI shape,
//                                         re-dispatch through handleChat, and
//                                         convert the OpenAI SSE or JSON
//                                         response back to Gemini shape.
//
// Streaming intent is read from the URL action suffix (:streamGenerateContent
// => SSE, :generateContent => JSON), NOT a body field — mirroring the JS
// route and the canonical Gemini API convention.
//
// The Gemini-native TTS forward branch (raw-byte proxy to
// generativelanguage.googleapis.com with the credential fallback loop) is a
// separate slice; for now those requests get an honest 501 pointing at the
// future task. The text-chat branch is the client-surface gap #38 closes.

const (
	// v1betaGeminiModelPattern mirrors the JS GEMINI_NATIVE_MODEL_PATTERN /
	// sanitizeGeminiFunctionName charset; blocks path traversal in the
	// upstream model segment.
	v1betaGeminiModelPattern = "^[a-zA-Z0-9_.:-]+$"
)

// v1betaGeminiNativeBaseURL is the upstream Gemini endpoint the TTS-forward
// branch proxies to. It is a var (not a const) so tests can repoint it at a
// local httptest server; production code never reassigns it.
var v1betaGeminiNativeBaseURL = "https://generativelanguage.googleapis.com/v1beta/models"

// v1betaModelEntry is one entry in the Gemini /v1beta/models list shape.
type v1betaModelEntry struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName"`
	Description                string   `json:"description"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	InputTokenLimit            int      `json:"inputTokenLimit"`
	OutputTokenLimit           int      `json:"outputTokenLimit"`
}

// handleV1BetaModels implements GET /v1beta/models — the Gemini-compatible
// model catalog. It mirrors JS route.js GET: iterate provider.AllCatalogs,
// emit `models/<alias>/<modelId>` for every model and a bare
// `models/<modelId>` (with generateContent + streamGenerateContent methods)
// for gemini-provider models.
func (h *v1Handler) handleV1BetaModels(w http.ResponseWriter, r *http.Request) {
	seen := make(map[string]struct{})
	models := make([]v1betaModelEntry, 0, 256)

	add := func(name, displayName, description string, methods []string) {
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		if methods == nil {
			methods = []string{"generateContent"}
		}
		models = append(models, v1betaModelEntry{
			Name:                        name,
			DisplayName:                 displayName,
			Description:                 description,
			SupportedGenerationMethods:  methods,
			InputTokenLimit:             128000,
			OutputTokenLimit:            8192,
		})
	}

	for _, cat := range provider.AllCatalogs() {
		prov := cat.Alias
		if prov == "" {
			prov = cat.ID
		}
		for _, m := range cat.Models {
			display := m.Name
			if display == "" {
				display = m.ID
			}
			add(
				"models/"+prov+"/"+m.ID,
				display,
				prov+" model: "+display,
				nil,
			)
			if cat.ID == "gemini" || cat.Alias == "gemini" {
				add(
					"models/"+m.ID,
					display,
					"Gemini model: "+display,
					[]string{"generateContent", "streamGenerateContent"},
				)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
}

// v1betaRequest is the parsed Gemini request body (the subset we forward to
// the chat pipeline). Fields are optional; missing fields are omitted.
type v1betaRequest struct {
	SystemInstruction *v1betaContent   `json:"systemInstruction,omitempty"`
	Contents           []v1betaContent  `json:"contents,omitempty"`
	GenerationConfig  *v1betaGenConfig  `json:"generationConfig,omitempty"`
}

type v1betaContent struct {
	Role  string         `json:"role,omitempty"`
	Parts []v1betaPart   `json:"parts,omitempty"`
}

type v1betaPart struct {
	Text string `json:"text,omitempty"`
}

type v1betaGenConfig struct {
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"topP,omitempty"`
}

// handleV1BetaModelsPath implements POST /v1beta/models/{path...}. path is
// either "<model>:<action>" or "<provider>/<model>:<action>". The action
// suffix determines streaming (:streamGenerateContent => SSE,
// :generateContent => JSON). The Gemini request body is converted to the
// internal/OpenAI shape and re-dispatched through handleChat; the OpenAI
// response is converted back to Gemini shape (SSE or JSON).
func (h *v1Handler) handleV1BetaModelsPath(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	path := r.PathValue("path")
	model, stream, ok := parseV1BetaModelAction(path)
	if !ok {
		h.writeError(w, http.StatusBadRequest, "Invalid model path")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Gemini-native TTS forward (audio response modality or a gemini tts
	// model id) is a raw-byte proxy to the upstream Gemini endpoint
	// (generativelanguage.googleapis.com/v1beta/models/<id>:<action>) with a
	// credential fallback rotation: on an upstream failure we exclude the
	// connection and retry the next active gemini connection. Ports legacy
	// JS forwardGeminiNativeRequest in
	// src/app/api/v1beta/models/[...path]/route.js.
	//
	// MVP scope: the raw-byte forward + exclude-set rotation over active
	// connections. The JS path also called markAccountUnavailable to persist
	// a per-account error state (decolua/9router #2703 Fix 3 structured
	// failures) — that DB-level account-marking is a follow-up slice tracked
	// under #2703; here we rotate on every retriable failure and return the
	// last error without persisting account state.
	if isV1BetaGeminiNativeTTS(model, body) {
		h.handleV1BetaTTSForward(w, r, model, stream, body)
		return
	}

	internalBody, err := convertGeminiToInternal(body, model, stream)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid Gemini request body")
		return
	}

	// Re-dispatch through the chat pipeline as /v1/chat/completions.
	r2 := r.Clone(ctx)
	r2.Body = io.NopCloser(bytes.NewReader(internalBody))
	r2.URL.Path = "/v1/chat/completions"
	r2.RequestURI = "POST /v1/chat/completions HTTP/1.1"

	if stream {
		// Pipe: chat handler writes OpenAI SSE into the pipe; a goroutine
		// converts OpenAI SSE -> Gemini SSE and writes to the client.
		pr, pw := io.Pipe()
		defer pw.Close()
		capture := &pipeResponseWriter{header: http.Header{}, pw: pw}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		convErr := make(chan error, 1)
		go func() {
			err := convertOpenAISSEToGeminiSSE(w, pr, model)
			pr.Close()
			convErr <- err
		}()

		h.handleChat(capture, r2)
		pw.Close()
		<-convErr
		return
	}

	// Non-streaming: capture the OpenAI JSON the chat handler writes, then
	// convert to a Gemini GenerateContentResponse.
	pr, pw := io.Pipe()
	defer pw.Close()
	capture := &pipeResponseWriter{header: http.Header{}, pw: pw}

	convErr := make(chan error, 1)
	go func() {
		err := convertOpenAIJSONToGemini(w, pr, model)
		pr.Close()
		convErr <- err
	}()

	h.handleChat(capture, r2)
	pw.Close()
	<-convErr
}

// parseV1BetaModelAction parses the {path...} value into (model, stream, ok).
// path is one of:
//   <provider>/<model>:<action>
//   <model>:<action>
// action is :generateContent (stream=false) or :streamGenerateContent
// (stream=true). Returns ok=false if the action suffix is missing/unknown.
func parseV1BetaModelAction(path string) (model string, stream bool, ok bool) {
	var action string
	if strings.Contains(path, ":streamGenerateContent") {
		action = ":streamGenerateContent"
		stream = true
	} else if strings.Contains(path, ":generateContent") {
		action = ":generateContent"
	} else {
		return "", false, false
	}
	trimmed := strings.Replace(path, action, "", 1)
	return trimmed, stream, true
}

// isV1BetaGeminiNativeTTS reports whether the request targets the
// Gemini-native TTS path (audio response modality). Mirrors the JS
// isGeminiNativeTtsRequest hasAudioResponseModality branch; the tts-model-id
// set check is folded into the future TTS-forward slice.
func isV1BetaGeminiNativeTTS(model string, body []byte) bool {
	var probe struct {
		GenerationConfig struct {
			ResponseModalities []string `json:"responseModalities"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	for _, m := range probe.GenerationConfig.ResponseModalities {
		if strings.EqualFold(m, "AUDIO") {
			return true
		}
	}
	return false
}

// convertGeminiToInternal mirrors the JS convertGeminiToInternal: a minimal
// Gemini -> OpenAI/internal chat-completions body (systemInstruction + contents
// -> messages, generationConfig -> max_tokens/temperature/top_p). Tools,
// images, and function calls are out of scope for the MVP text-chat path.
func convertGeminiToInternal(geminiBody []byte, model string, stream bool) ([]byte, error) {
	var g v1betaRequest
	if err := json.Unmarshal(geminiBody, &g); err != nil {
		return nil, err
	}

	messages := make([]map[string]any, 0, len(g.Contents)+1)

	if g.SystemInstruction != nil {
		var sysText strings.Builder
		for _, p := range g.SystemInstruction.Parts {
			if p.Text != "" {
				if sysText.Len() > 0 {
					sysText.WriteByte('\n')
				}
				sysText.WriteString(p.Text)
			}
		}
		if sysText.Len() > 0 {
			messages = append(messages, map[string]any{
				"role":    "system",
				"content": sysText.String(),
			})
		}
	}

	for _, c := range g.Contents {
		role := "user"
		if c.Role == "model" {
			role = "assistant"
		}
		var text strings.Builder
		for _, p := range c.Parts {
			if p.Text != "" {
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString(p.Text)
			}
		}
		messages = append(messages, map[string]any{
			"role":    role,
			"content": text.String(),
		})
	}

	out := map[string]any{
		"model":    model,
		"messages": messages,
		"stream":   stream,
	}
	if g.GenerationConfig != nil {
		if g.GenerationConfig.MaxOutputTokens != nil {
			out["max_tokens"] = *g.GenerationConfig.MaxOutputTokens
		}
		if g.GenerationConfig.Temperature != nil {
			out["temperature"] = *g.GenerationConfig.Temperature
		}
		if g.GenerationConfig.TopP != nil {
			out["top_p"] = *g.GenerationConfig.TopP
		}
	}
	return json.Marshal(out)
}

// v1betaFinishReasonMap maps OpenAI finish_reason to Gemini finishReason,
// mirroring the JS FINISH_REASON_MAP.
var v1betaFinishReasonMap = map[string]string{
	"stop":           "STOP",
	"length":         "MAX_TOKENS",
	"tool_calls":     "STOP",
	"content_filter": "SAFETY",
}

// convertOpenAISSEToGeminiSSE reads an OpenAI SSE stream from `src` and writes
// a Gemini SSE stream to `w`. Each OpenAI `data: {...}` chunk is reshaped into
// a Gemini candidates chunk; the OpenAI [DONE] sentinel is dropped (Gemini
// SSE ends by stream close). Usage metadata is attached on the final
// (finish_reason-bearing) chunk. Ports JS transformOpenAISSEToGeminiSSE.
func convertOpenAISSEToGeminiSSE(w http.ResponseWriter, src io.Reader, model string) error {
	reader := bufio.NewReader(src)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			continue
		}
		choices, _ := parsed["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, ok := choices[0].(map[string]any)
		if !ok {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)

		var parts []map[string]any
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			parts = append(parts, map[string]any{"text": rc, "thought": true})
		}
		if c, ok := delta["content"].(string); ok && c != "" {
			parts = append(parts, map[string]any{"text": c})
		}

		finishReason, _ := choice["finish_reason"].(string)
		if len(parts) == 0 && finishReason == "" {
			continue
		}

		candidate := map[string]any{
			"content": map[string]any{
				"role":  "model",
				"parts": parts,
			},
			"index": 0,
		}
		if len(parts) == 0 {
			candidate["content"].(map[string]any)["parts"] = []map[string]any{{"text": ""}}
		}
		if finishReason != "" {
			fr, ok := v1betaFinishReasonMap[finishReason]
			if !ok {
				fr = "STOP"
			}
			candidate["finishReason"] = fr
		}

		chunk := map[string]any{"candidates": []any{candidate}}

		if finishReason != "" {
			if usage, ok := parsed["usage"].(map[string]any); ok {
				chunk["usageMetadata"] = buildV1BetaUsageMetadata(usage)
			}
			if m, ok := parsed["model"].(string); ok && m != "" {
				chunk["modelVersion"] = m
			} else {
				chunk["modelVersion"] = model
			}
		}

		b, _ := json.Marshal(chunk)
		if _, err := io.WriteString(w, "data: "+string(b)+"\r\n\r\n"); err != nil {
			return err
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// convertOpenAIJSONToGemini reads a non-streaming OpenAI chat.completion JSON
// response from `src` and writes a Gemini GenerateContentResponse to `w`.
// Ports JS convertOpenAIResponseToGemini.
func convertOpenAIJSONToGemini(w http.ResponseWriter, src io.Reader, model string) error {
	raw, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		// Not JSON — pass through unchanged.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write(raw)
		return nil
	}

	// Already a Gemini-shaped response (candidates present) — passthrough.
	if _, ok := body["candidates"]; ok {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(w).Encode(body)
		return nil
	}
	if _, ok := body["error"]; ok {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(w).Encode(body)
		return nil
	}

	choices, _ := body["choices"].([]any)
	if len(choices) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(w).Encode(body)
		return nil
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(w).Encode(body)
		return nil
	}

	message, _ := choice["message"].(map[string]any)
	finishReason, _ := choice["finish_reason"].(string)

	var parts []map[string]any
	if message != nil {
		if rc, ok := message["reasoning_content"].(string); ok && rc != "" {
			parts = append(parts, map[string]any{"text": rc, "thought": true})
		}
		content, _ := message["content"].(string)
		parts = append(parts, map[string]any{"text": content})
	}
	if len(parts) == 0 {
		parts = []map[string]any{{"text": ""}}
	}

	fr, ok := v1betaFinishReasonMap[finishReason]
	if !ok {
		fr = "STOP"
	}

	geminiResp := map[string]any{
		"candidates": []any{
			map[string]any{
				"content":      map[string]any{"role": "model", "parts": parts},
				"finishReason": fr,
				"index":        0,
			},
		},
	}
	if m, ok := body["model"].(string); ok && m != "" {
		geminiResp["modelVersion"] = m
	} else {
		geminiResp["modelVersion"] = model
	}
	if usage, ok := body["usage"].(map[string]any); ok {
		geminiResp["usageMetadata"] = buildV1BetaUsageMetadata(usage)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(geminiResp)
	return nil
}

// buildV1BetaUsageMetadata converts an OpenAI usage object to a Gemini
// usageMetadata object, including thoughtsTokenCount when the OpenAI
// completion_tokens_details.reasoning_tokens is present.
func buildV1BetaUsageMetadata(usage map[string]any) map[string]any {
	meta := map[string]any{
		"promptTokenCount":     numberToInt(usage["prompt_tokens"]),
		"candidatesTokenCount": numberToInt(usage["completion_tokens"]),
		"totalTokenCount":      numberToInt(usage["total_tokens"]),
	}
	if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		if rt := numberToInt(details["reasoning_tokens"]); rt > 0 {
			meta["thoughtsTokenCount"] = rt
		}
	}
	return meta
}

// numberToInt coerces a JSON numeric value (float64/int/int64) to int.
func numberToInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// v1betaGeminiModelRe validates the bare model id segment used to build the
// upstream Gemini URL. Mirrors the JS GEMINI_NATIVE_MODEL_PATTERN — blocks
// path traversal in the upstream path.
var v1betaGeminiModelRe = regexp.MustCompile(v1betaGeminiModelPattern)

// v1BetaTTSFetchTimeout is the per-attempt upstream timeout for the
// Gemini-native TTS forward. Mirrors the JS GEMINI_NATIVE_TTS_FETCH_TIMEOUT_MS
// default (60s). Tunable via V1BETA_TTS_FETCH_TIMEOUT_MS env in a future slice.
var v1BetaTTSFetchTimeout = 60 * time.Second

// handleV1BetaTTSForward proxies a Gemini-native TTS request (audio
// responseModality) raw-byte to the upstream Gemini endpoint, rotating across
// active gemini connections on retriable failures (exclude-set loop). Ports
// legacy JS forwardGeminiNativeRequest.
//
// The upstream URL is built as v1betaGeminiNativeBaseURL/<modelId><action>
// with the incoming query string forwarded (minus the client `key=` param —
// auth comes from the resolved connection). Auth headers are derived from the
// connection's apiKey (x-goog-api-key) or accessToken (Authorization: Bearer),
// mirroring JS buildGeminiNativeAuthHeaders.
//
// On an upstream failure (network error, 5xx/502/504, or a 401/403 that does
// not recover on a fresh connection) we add the connection to the exclude set
// and retry the next active gemini connection. The last error is surfaced as
// the response when no connections remain. DB-level account-marking
// (markAccountUnavailable, #2703 Fix 3) is a follow-up — this MVP rotates
// without persisting per-account error state.
func (h *v1Handler) handleV1BetaTTSForward(w http.ResponseWriter, r *http.Request, model string, stream bool, body []byte) {
	ctx := r.Context()

	// API-key gate, identical to the chat surface (the JS path validated the
	// client key via isValidApiKey when settings.requireApiKey was set).
	if err := h.requireV1BetaClientKey(ctx, w, r); err != nil {
		return
	}

	modelID := normalizeV1BetaModel(model)
	if !v1betaGeminiModelRe.MatchString(modelID) {
		h.writeV1BetaError(w, http.StatusBadRequest, "Invalid model")
		return
	}

	action := ":generateContent"
	if stream {
		action = ":streamGenerateContent"
	}
	upstreamURL := buildV1BetaTTSUpstreamURL(r.URL, modelID, action)

	connections, err := h.deps.ConnectionRepo.List(ctx, repo.ConnectionFilter{Provider: "gemini", IsActive: boolPtr(true)})
	if err != nil {
		h.writeV1BetaError(w, http.StatusInternalServerError, fmt.Sprintf("list connections: %v", err))
		return
	}

	excluded := make(map[string]struct{})
	var lastErr string
	var lastStatus int

	for _, conn := range connections {
		if _, skip := excluded[conn.ID]; skip {
			continue
		}
		creds := buildV1BetaConnCredentials(conn)
		authHdrs := buildV1BetaAuthHeaders(creds)
		if authHdrs == nil {
			excluded[conn.ID] = struct{}{}
			lastErr = "No Gemini API key configured"
			lastStatus = http.StatusNotFound
			continue
		}

		status, errText, upstreamOK := h.doV1BetaTTSUpstream(ctx, w, r, upstreamURL, authHdrs, body)
		if upstreamOK {
			return
		}

		// Retriable: rotate. Non-retriable client errors (400/401/403/404/429)
		// surface immediately on the first connection — the JS path's
		// shouldFallback gate decides this; 5xx/502/504 and network errors
		// rotate to the next connection.
		if isV1BetaRetriableStatus(status) {
			excluded[conn.ID] = struct{}{}
			if errText != "" {
				lastErr = errText
			}
			lastStatus = status
			continue
		}
		// Non-retriable: write the upstream error to the client directly.
		h.writeV1BetaError(w, status, errText)
		return
	}

	// Exhausted every connection.
	status := lastStatus
	if status == 0 {
		status = http.StatusServiceUnavailable
	}
	msg := lastErr
	if msg == "" {
		msg = "No active credentials for provider: gemini"
	}
	h.writeV1BetaError(w, status, msg)
}

// buildV1BetaConnCredentials extracts the auth material from a stored gemini
// connection row into domain.Credentials. Mirrors the field extraction in
// resolveCredentials but for a single chosen connection (the TTS forward
// rotates connections, so it cannot reuse the "first connection" path).
func buildV1BetaConnCredentials(conn settings.ProviderConnection) domainProv.Credentials {
	var data map[string]any
	_ = json.Unmarshal(conn.Data, &data)
	creds := domainProv.Credentials{
		ProviderSpecificData: map[string]any{"_connectionId": conn.ID},
	}
	if v, ok := data["apiKey"].(string); ok {
		creds.APIKey = v
	}
	if v, ok := data["accessToken"].(string); ok {
		creds.AccessToken = v
	}
	if v, ok := data["providerSpecificData"].(map[string]any); ok {
		for k, val := range v {
			creds.ProviderSpecificData[k] = val
		}
	}
	return creds
}

// doV1BetaTTSUpstream performs a single upstream attempt. On success it
// streams the upstream body to w with CORS + stripped compression headers
// and returns upstreamOK=true. On failure it returns the status + error text
// and upstreamOK=false; the caller decides rotation vs surfacing.
func (h *v1Handler) doV1BetaTTSUpstream(ctx context.Context, w http.ResponseWriter, r *http.Request, upstreamURL string, authHdrs map[string]string, body []byte) (status int, errText string, upstreamOK bool) {
	client := &http.Client{Timeout: v1BetaTTSFetchTimeout}
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return http.StatusBadGateway, fmt.Sprintf("build upstream request: %v", err), false
	}
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	upReq.Header.Set("Content-Type", ct)
	for k, v := range authHdrs {
		upReq.Header.Set(k, v)
	}

	resp, err := client.Do(upReq)
	if err != nil {
		// Network/timeout errors are retriable → rotate.
		code := classifyV1BetaNetError(err)
		return code, fmt.Sprintf("%s (%d)", err.Error(), code), false
	}
	defer resp.Body.Close()

	if resp.StatusCode < 400 {
		// Success: stream the upstream body with CORS + stripped compression
		// headers (mirrors JS corsHeadersFrom — forwarding content-encoding
		// would make clients decompress plain bytes again).
		hdr := w.Header()
		for k, vs := range resp.Header {
			if k == "Content-Encoding" || k == "Content-Length" || k == "Transfer-Encoding" {
				continue
			}
			for _, v := range vs {
				hdr.Add(k, v)
			}
		}
		hdr.Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return resp.StatusCode, "", true
	}

	// Upstream error: read the body for the error text and surface the
	// upstream status.
	errBytes, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(errBytes), false
}

// requireV1BetaClientKey validates the client-supplied API key when
// settings.requireApiKey is set. Mirrors JS validateGeminiNativeClientKey:
// the key is read from Authorization: Bearer, x-goog-api-key, or ?key=.
func (h *v1Handler) requireV1BetaClientKey(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	requireKey, err := h.requireAPIKey(ctx)
	if err != nil {
		h.writeV1BetaError(w, http.StatusInternalServerError, "Auth check failed")
		return err
	}
	if !requireKey {
		return nil
	}
	apiKey := extractV1BetaClientKey(r)
	if apiKey == "" {
		h.writeV1BetaError(w, http.StatusUnauthorized, "Missing API key")
		return fmt.Errorf("missing api key")
	}
	valid, err := h.deps.APIKeysRepo.Validate(ctx, apiKey)
	if err != nil {
		h.writeV1BetaError(w, http.StatusInternalServerError, "Auth check failed")
		return err
	}
	if !valid {
		h.writeV1BetaError(w, http.StatusUnauthorized, "Invalid API key")
		return fmt.Errorf("invalid api key")
	}
	return nil
}

// extractV1BetaClientKey reads the client API key from Authorization: Bearer,
// x-goog-api-key, or the ?key= query param — the three Gemini-SDK auth spots.
func extractV1BetaClientKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	if k := r.Header.Get("x-goog-api-key"); k != "" {
		return k
	}
	return r.URL.Query().Get("key")
}

// buildV1BetaAuthHeaders returns the upstream auth headers for a resolved
// gemini credential: x-goog-api-key when an apiKey is present, else
// Authorization: Bearer <accessToken>. Returns nil when neither is set.
func buildV1BetaAuthHeaders(creds domainProv.Credentials) map[string]string {
	if creds.APIKey != "" {
		return map[string]string{"x-goog-api-key": creds.APIKey}
	}
	if creds.AccessToken != "" {
		return map[string]string{"Authorization": "Bearer " + creds.AccessToken}
	}
	return nil
}

// buildV1BetaTTSUpstreamURL builds the upstream Gemini endpoint URL, copying
// the incoming query string minus the client `key=` param. Mirrors JS
// buildGeminiNativeUrl.
func buildV1BetaTTSUpstreamURL(sourceURL *url.URL, modelID, action string) string {
	upstream, _ := url.Parse(v1betaGeminiNativeBaseURL + "/" + modelID + action)
	q := upstream.Query()
	for k, vs := range sourceURL.Query() {
		if k == "key" {
			continue
		}
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	upstream.RawQuery = q.Encode()
	return upstream.String()
}

// normalizeV1BetaModel strips the leading models/ or gemini/ prefix, mirroring
// JS normalizeGeminiNativeModel.
func normalizeV1BetaModel(model string) string {
	out := strings.TrimPrefix(model, "models/")
	out = strings.TrimPrefix(out, "gemini/")
	return out
}

// isV1BetaRetriableStatus reports whether a status warrants rotating to the
// next connection (mirrors the JS shouldFallback gate on retriable upstream
// failures). 5xx/502/504 rotate; 4xx surface immediately.
func isV1BetaRetriableStatus(status int) bool {
	switch status {
	case 500, 502, 503, 504:
		return true
	}
	return false
}

// classifyV1BetaNetError maps a network/timeout error to an HTTP status,
// mirroring the JS isGeminiNativeTimeoutError heuristic: timeouts → 504,
// other fetch failures → 502.
func classifyV1BetaNetError(err error) int {
	msg := err.Error()
	if strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "timeout") {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

// writeV1BetaError writes a Gemini-shaped error response.
func (h *v1Handler) writeV1BetaError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"code":    status,
		},
	})
}

// v1BetaConn (removed): the TTS forward now iterates []settings.ProviderConnection
// directly from ConnectionRepo.List, so no separate view type is needed.