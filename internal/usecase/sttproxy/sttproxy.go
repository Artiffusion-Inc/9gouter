// Package sttproxy implements the /v1/audio/transcriptions pipeline for the Go
// rewrite. It ports src/sse/handlers/stt.js (handleStt) + open-sse/handlers/sttCore.js
// (handleSttCore): parse the multipart form, resolve the provider's static STT
// config, dispatch by format (deepgram raw-binary, assemblyai upload→submit→poll,
// gemini generateContent, openai-compatible multipart passthrough), and return
// either the reshaped { "text": "..." } JSON or the raw upstream body for the
// OpenAI-compatible case (which forwards response_format unchanged).
//
// NOT in this MVP slice (separate slices, mirroring the video/fetch scope):
// the multi-account fallback rotation loop (excludeConnectionIds), usage
// persistence (the legacy STT path never persisted usage), and on-401 token
// refresh. x-9router-connection-id is NOT returned by STT (the JS handler does
// not echo it — unlike video). The provider is resolved at the handler from
// body.model (provider/model prefix) and passed in as ProviderID.
package sttproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/stt"
	domainProv "github.com/Artiffusion-Inc/9router/internal/domain/provider"
)

// Logger is a minimal log sink.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

// noopLogger is the default when none is provided.
type noopLogger struct{}

func (noopLogger) Infof(string, ...any)  {}
func (noopLogger) Warnf(string, ...any)  {}
func (noopLogger) Debugf(string, ...any) {}

// Dependencies wires the sttproxy Handler.
type Dependencies struct {
	// HTTPClient performs upstream calls. Defaults to a 300s-timeout client
	// (matching the JS maxDuration=300 for large audio uploads).
	HTTPClient *http.Client
	Logger     Logger
	Config     config.Config
}

// Handler runs the STT pipeline.
type Handler struct {
	deps Dependencies
}

// New constructs a Handler with sane defaults.
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
	Ctx        context.Context
	ProviderID string
	Model      string
	// File is the uploaded audio bytes.
	File []byte
	// Filename is the upload filename (may be empty; defaults to "audio.wav").
	Filename string
	// FileMIME is the browser-provided MIME (may be empty; resolved from ext).
	FileMIME string
	// FormFields are the non-file multipart fields (model, language, prompt,
	// response_format, temperature, timestamp_granularities, ...).
	FormFields  map[string]string
	Credentials domainProv.Credentials
	UserAgent   string
}

// Result is the output of Handle.
type Result struct {
	StatusCode int
	Err        error
	// Body is the response body bytes.
	Body []byte
	// ContentType is the response Content-Type to forward to the client.
	ContentType string
}

// Handle dispatches the STT upstream call by the provider's static config.
func (h *Handler) Handle(ctx context.Context, req Request) Result {
	cfg, ok := stt.Lookup(req.ProviderID)
	if !ok {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("provider '%s' does not support STT", req.ProviderID)}
	}
	if len(req.File) == 0 {
		return Result{StatusCode: http.StatusBadRequest, Err: errors.New("Missing required field: file")}
	}
	token := ""
	if cfg.AuthType != stt.AuthTypeNone {
		token = credentialToken(req.Credentials)
		if token == "" {
			return Result{StatusCode: http.StatusUnauthorized, Err: fmt.Errorf("No credentials for STT provider: %s", req.ProviderID)}
		}
	}
	switch cfg.Format {
	case stt.FormatDeepgram:
		return h.transcribeDeepgram(ctx, cfg, req, token)
	case stt.FormatAssemblyAI:
		return h.transcribeAssemblyAI(ctx, cfg, req, token)
	case stt.FormatGemini:
		return h.transcribeGemini(ctx, cfg, req, token)
	default:
		return h.transcribeOpenAICompatible(ctx, cfg, req, token)
	}
}

// transcribeOpenAICompatible forwards the multipart to the upstream and returns
// the raw upstream body + Content-Type (so response_format text/srt/vtt/verbose_json
// are preserved verbatim).
func (h *Handler) transcribeOpenAICompatible(ctx context.Context, cfg stt.Config, req Request, token string) Result {
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	fh, err := w.CreateFormFile("file", orDefault(req.Filename, "audio.wav"))
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	if _, err := fh.Write(req.File); err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	if err := w.WriteField("model", req.Model); err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	for _, k := range []string{"language", "prompt", "response_format", "temperature"} {
		if v, ok := req.FormFields[k]; ok && v != "" {
			_ = w.WriteField(k, v)
		}
	}
	if err := w.Close(); err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, body)
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	httpReq.Header.Set("Content-Type", w.FormDataContentType())
	setAuthHeader(httpReq, cfg, token)
	resp, err := h.deps.HTTPClient.Do(httpReq)
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		return Result{StatusCode: resp.StatusCode, Err: upstreamError(resp.StatusCode, b)}
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	return Result{StatusCode: resp.StatusCode, Body: b, ContentType: ct}
}

// transcribeDeepgram POSTs the raw audio binary with model/language query
// params and reshapes the response to { "text": transcript }.
func (h *Handler) transcribeDeepgram(ctx context.Context, cfg stt.Config, req Request, token string) Result {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	q := u.Query()
	q.Set("model", req.Model)
	q.Set("smart_format", "true")
	q.Set("punctuate", "true")
	if lang := strings.TrimSpace(req.FormFields["language"]); lang != "" {
		q.Set("language", lang)
	} else {
		q.Set("detect_language", "true")
	}
	u.RawQuery = q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(req.File))
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	httpReq.Header.Set("Content-Type", resolveAudioContentType(req.FileMIME, req.Filename))
	setAuthHeader(httpReq, cfg, token)
	resp, err := h.deps.HTTPClient.Do(httpReq)
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		return Result{StatusCode: resp.StatusCode, Err: upstreamError(resp.StatusCode, b)}
	}
	var dg struct {
		Results struct {
			Channels []struct {
				Alternatives []struct {
					Transcript string `json:"transcript"`
				} `json:"alternatives"`
			} `json:"channels"`
		} `json:"results"`
	}
	text := ""
	if err := json.Unmarshal(b, &dg); err == nil {
		if len(dg.Results.Channels) > 0 && len(dg.Results.Channels[0].Alternatives) > 0 {
			text = dg.Results.Channels[0].Alternatives[0].Transcript
		}
	}
	return jsonResponse(text)
}

// transcribeAssemblyAI: upload audio → submit transcript → poll until completed
// (or 120s timeout), reshape to { "text": transcript }.
func (h *Handler) transcribeAssemblyAI(ctx context.Context, cfg stt.Config, req Request, token string) Result {
	// 1. Upload the raw audio to the AssemblyAI upload endpoint.
	uploadURL := cfg.UploadURL
	if uploadURL == "" {
		uploadURL = "https://api.assemblyai.com/v2/upload"
	}
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(req.File))
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	upReq.Header.Set("Content-Type", "application/octet-stream")
	setAuthHeader(upReq, cfg, token)
	upResp, err := h.deps.HTTPClient.Do(upReq)
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	upBody, _ := io.ReadAll(io.LimitReader(upResp.Body, 1<<20))
	upResp.Body.Close()
	if upResp.StatusCode >= 400 {
		return Result{StatusCode: upResp.StatusCode, Err: upstreamError(upResp.StatusCode, upBody)}
	}
	var up struct {
		UploadURL string `json:"upload_url"`
	}
	if err := json.Unmarshal(upBody, &up); err != nil || up.UploadURL == "" {
		return Result{StatusCode: http.StatusBadGateway, Err: fmt.Errorf("AssemblyAI upload: missing upload_url")}
	}

	// 2. Submit the transcript request.
	submitBody, _ := json.Marshal(map[string]any{
		"audio_url":          up.UploadURL,
		"speech_models":      []string{req.Model},
		"language_detection": true,
	})
	subReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(submitBody))
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	subReq.Header.Set("Content-Type", "application/json")
	setAuthHeader(subReq, cfg, token)
	subResp, err := h.deps.HTTPClient.Do(subReq)
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	subBody, _ := io.ReadAll(io.LimitReader(subResp.Body, 1<<20))
	subResp.Body.Close()
	if subResp.StatusCode >= 400 {
		return Result{StatusCode: subResp.StatusCode, Err: upstreamError(subResp.StatusCode, subBody)}
	}
	var sub struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(subBody, &sub); err != nil || sub.ID == "" {
		return Result{StatusCode: http.StatusBadGateway, Err: fmt.Errorf("AssemblyAI submit: missing id")}
	}

	// 3. Poll until completed or 120s timeout (2s interval, mirroring JS).
	deadline := time.Now().Add(120 * time.Second)
	pollURL := cfg.BaseURL + "/" + sub.ID
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return Result{StatusCode: http.StatusBadGateway, Err: ctx.Err()}
		case <-time.After(2 * time.Second):
		}
		pReq, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
		if err != nil {
			return Result{StatusCode: http.StatusBadGateway, Err: err}
		}
		setAuthHeader(pReq, cfg, token)
		pResp, err := h.deps.HTTPClient.Do(pReq)
		if err != nil {
			continue
		}
		pBody, _ := io.ReadAll(io.LimitReader(pResp.Body, 1<<20))
		pResp.Body.Close()
		if pResp.StatusCode >= 400 {
			continue
		}
		var r struct {
			Status string `json:"status"`
			Text   string `json:"text"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(pBody, &r); err != nil {
			continue
		}
		switch r.Status {
		case "completed":
			return jsonResponse(r.Text)
		case "error":
			return Result{StatusCode: http.StatusInternalServerError, Err: errors.New(orDefault(r.Error, "AssemblyAI failed"))}
		}
	}
	return Result{StatusCode: http.StatusGatewayTimeout, Err: errors.New("AssemblyAI timeout after 120s")}
}

// transcribeGemini sends the audio as inline_data base64 to generateContent
// and reshapes the candidate text to { "text": ... }.
func (h *Handler) transcribeGemini(ctx context.Context, cfg stt.Config, req Request, token string) Result {
	b64 := base64.StdEncoding.EncodeToString(req.File)
	mime := resolveAudioContentType(req.FileMIME, req.Filename)
	promptText := strings.TrimSpace(req.FormFields["prompt"])
	if promptText == "" {
		promptText = "Generate a transcript of the speech. Return only the transcribed text, no commentary."
	}
	if lang := strings.TrimSpace(req.FormFields["language"]); lang != "" {
		promptText += " Language: " + lang + "."
	}
	gemURL := strings.TrimRight(cfg.BaseURL, "/") + "/" + req.Model + ":generateContent?key=" + token
	body, _ := json.Marshal(map[string]any{
		"contents": []map[string]any{
			{
				"parts": []map[string]any{
					{"text": promptText},
					{"inline_data": map[string]any{"mime_type": mime, "data": b64}},
				},
			},
		},
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, gemURL, bytes.NewReader(body))
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := h.deps.HTTPClient.Do(httpReq)
	if err != nil {
		return Result{StatusCode: http.StatusBadGateway, Err: err}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		return Result{StatusCode: resp.StatusCode, Err: upstreamError(resp.StatusCode, b)}
	}
	var g struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	text := ""
	if err := json.Unmarshal(b, &g); err == nil {
		var sb strings.Builder
		for _, c := range g.Candidates {
			for _, p := range c.Content.Parts {
				if p.Text != "" {
					sb.WriteString(p.Text)
				}
			}
		}
		text = sb.String()
	}
	return jsonResponse(text)
}

// jsonResponse reshapes the transcript to the OpenAI-compatible { "text": ... }
// form, matching the JS jsonResponse helper.
func jsonResponse(text string) Result {
	b, _ := json.Marshal(map[string]string{"text": text})
	return Result{StatusCode: http.StatusOK, Body: b, ContentType: "application/json"}
}

// credentialToken extracts the apiKey/accessToken from credentials, mirroring
// the JS `credentials?.apiKey || credentials?.accessToken`.
func credentialToken(creds domainProv.Credentials) string {
	if creds.APIKey != "" {
		return creds.APIKey
	}
	if v, ok := creds.ProviderSpecificData["apiKey"].(string); ok && v != "" {
		return v
	}
	if creds.AccessToken != "" {
		return creds.AccessToken
	}
	return ""
}

// setAuthHeader applies the provider's auth-header scheme to the request,
// mirroring the JS buildAuthHeaders.
func setAuthHeader(r *http.Request, cfg stt.Config, token string) {
	if token == "" {
		return
	}
	switch cfg.AuthHeader {
	case stt.AuthBearer:
		r.Header.Set("Authorization", "Bearer "+token)
	case stt.AuthToken:
		r.Header.Set("Authorization", "Token "+token)
	case stt.AuthXAPIKey:
		r.Header.Set("x-api-key", token)
	case stt.AuthKey:
		r.Header.Set("Authorization", "Key "+token)
	case stt.AuthAuthorization:
		// AssemblyAI: the JS registry uses authHeader "authorization" and the
		// SDK sends the API key in the Authorization header directly.
		r.Header.Set("Authorization", token)
	default:
		r.Header.Set("Authorization", "Bearer "+token)
	}
}

// resolveAudioContentType maps a browser file MIME or filename extension to an
// audio MIME, mirroring the JS resolveAudioContentType.
func resolveAudioContentType(mime, filename string) string {
	t := strings.ToLower(mime)
	if strings.HasPrefix(t, "audio/") {
		return t
	}
	name := strings.ToLower(filename)
	ext := ""
	if i := strings.LastIndex(name, "."); i >= 0 {
		ext = name[i+1:]
	}
	switch ext {
	case "mp3":
		return "audio/mpeg"
	case "mp4", "m4a":
		return "audio/mp4"
	case "wav":
		return "audio/wav"
	case "ogg":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	case "webm":
		return "audio/webm"
	case "aac":
		return "audio/aac"
	case "opus":
		return "audio/opus"
	}
	return "application/octet-stream"
}

// upstreamError parses an upstream error body into an error, mirroring the JS
// upstreamError helper.
func upstreamError(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return fmt.Errorf("Upstream error (%d)", status)
	}
	var j struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &j); err == nil {
		// error may be an object {"message": ...} or a bare string.
		if len(j.Error) > 0 {
			var em struct {
				Message string `json:"message"`
			}
			if e := json.Unmarshal(j.Error, &em); e == nil && em.Message != "" {
				return errors.New(em.Message)
			}
			var es string
			if e := json.Unmarshal(j.Error, &es); e == nil && es != "" {
				return errors.New(es)
			}
		}
		if j.Message != "" {
			return errors.New(j.Message)
		}
	}
	return errors.New(msg)
}

// orDefault returns s when non-empty, else dflt.
func orDefault(s, dflt string) string {
	if s != "" {
		return s
	}
	return dflt
}
