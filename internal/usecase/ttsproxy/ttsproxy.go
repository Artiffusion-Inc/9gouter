// Package ttsproxy implements the /v1/audio/speech pipeline for the Go
// rewrite. It ports src/sse/handlers/tts.js (handleTts) + open-sse/handlers/
// ttsCore.js (handleTtsCore) + the per-provider ttsProviders adapters:
// synthesize the input text to audio via the provider's static TTS config,
// then wrap the audio bytes in the client-requested envelope (raw binary when
// response_format=mp3, or {"audio":base64,"format"} JSON when response_format=json).
//
// Supported in this MVP slice: openai-compatible, gemini (PCM→WAV), elevenlabs,
// minimax (hex→base64), inworld, cartesia, playht, nvidia, deepgram.
// Deferred (501): edge-tts / google-tts (web-scrape), local-device (OS exec),
// coqui/tortoise (local no-auth), aws-polly (sigv4), openrouter
// (SSE audio-chunk accumulate). The handler resolves the provider from
// body.model (provider/voice prefix or bare → first TTS-capable provider).
//
// NOT in this slice (separate slices): account fallback rotation, on-401
// token refresh, combo expansion, usage persistence. x-9gouter-connection-id
// is NOT returned by TTS (the JS handler does not).
package ttsproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/tts"
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

// Dependencies wires the ttsproxy Handler.
type Dependencies struct {
	HTTPClient *http.Client
	Logger     Logger
	Config     config.Config
}

// Handler runs the TTS pipeline.
type Handler struct {
	deps Dependencies
}

// New constructs a Handler with sane defaults (600s body timeout, matching
// FETCH_BODY_TIMEOUT_MS).
func New(deps Dependencies) *Handler {
	if deps.HTTPClient == nil {
		deps.HTTPClient = &http.Client{Timeout: 600 * time.Second}
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
	Model          string // "modelId/voiceId" or bare — usecase parses it
	Input          string
	Language       string // optional, Gemini hint
	ResponseFormat string // "mp3" (default, raw binary) | "json" (base64 wrapper)
	Credentials    domainProv.Credentials
	UserAgent      string
}

// Result is the output of Handle.
type Result struct {
	StatusCode int
	Err        error
	// Body is the response body bytes (raw audio when ResponseFormat=mp3, or
	// the JSON wrapper when ResponseFormat=json).
	Body []byte
	// ContentType is the response Content-Type to forward to the client.
	ContentType string
	// AudioFormat is the audio container ("mp3", "wav", ...) used by the
	// JSON envelope's "format" field.
	AudioFormat string
}

// Handle dispatches the TTS upstream call by the provider's static config.
func (h *Handler) Handle(ctx context.Context, req Request) Result {
	cfg, ok := tts.Lookup(req.ProviderID)
	if !ok {
		return Result{StatusCode: http.StatusBadRequest, Err: fmt.Errorf("provider '%s' does not support TTS", req.ProviderID)}
	}
	if cfg.Unsupported {
		return Result{StatusCode: http.StatusNotImplemented, Err: fmt.Errorf("TTS provider '%s' not supported in Go build", req.ProviderID)}
	}
	if strings.TrimSpace(req.Input) == "" {
		return Result{StatusCode: http.StatusBadRequest, Err: errors.New("missing required field: input")}
	}
	rf := req.ResponseFormat
	if rf == "" {
		rf = "mp3"
	}

	// Resolve credentials unless the provider is noAuth.
	if cfg.AuthType != tts.AuthTypeNone {
		if credentialToken(req.Credentials) == "" {
			return Result{StatusCode: http.StatusUnauthorized, Err: fmt.Errorf("no credentials for TTS provider: %s", req.ProviderID)}
		}
	}

	audio, audioFormat, err := h.synthesize(ctx, cfg, req)
	if err != nil {
		return errResult(err)
	}

	return h.envelope(audio, audioFormat, rf)
}

// synthesize calls the provider upstream and returns the raw audio bytes plus
// the container format.
func (h *Handler) synthesize(ctx context.Context, cfg tts.Config, req Request) ([]byte, string, error) {
	switch cfg.Format {
	case tts.FormatOpenAI:
		return h.synthOpenAI(ctx, cfg, req)
	case tts.FormatGemini:
		return h.synthGemini(ctx, cfg, req)
	case tts.FormatElevenLabs:
		return h.synthElevenLabs(ctx, cfg, req)
	case tts.FormatMiniMax:
		return h.synthMiniMax(ctx, cfg, req)
	case tts.FormatInworld:
		return h.synthInworld(ctx, cfg, req)
	case tts.FormatCartesia:
		return h.synthCartesia(ctx, cfg, req)
	case tts.FormatPlayHT:
		return h.synthPlayHT(ctx, cfg, req)
	case tts.FormatNvidia:
		return h.synthNvidia(ctx, cfg, req)
	case tts.FormatDeepgram:
		return h.synthDeepgram(ctx, cfg, req)
	default:
		return nil, "", fmt.Errorf("TTS format '%s' not implemented", cfg.Format)
	}
}

// parseModelVoice splits "modelId/voiceId" (or bare) into its parts, mirroring
// the JS parseModelVoice. The longest known-model-prefix wins; without a slash
// the config defaultModel/defaultVoice apply.
func parseModelVoice(modelStr string, cfg tts.Config) (modelID, voiceID string) {
	if !strings.Contains(modelStr, "/") {
		modelID = cfg.DefaultModel
		if modelID == "" {
			modelID = modelStr
		}
		voiceID = cfg.DefaultVoice
		if voiceID == "" {
			voiceID = modelStr
		}
		return
	}
	parts := strings.SplitN(modelStr, "/", 2)
	return parts[0], parts[1]
}

// === OpenAI-compatible ===

func (h *Handler) synthOpenAI(ctx context.Context, cfg tts.Config, req Request) ([]byte, string, error) {
	modelID, voiceID := parseModelVoice(req.Model, cfg)
	body, _ := json.Marshal(map[string]any{
		"model":           modelID,
		"voice":           voiceID,
		"input":           req.Input,
		"response_format": "mp3",
		"speed":           1.0,
	})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	r.Header.Set("Content-Type", "application/json")
	setAuthHeader(r, cfg, req.Credentials)
	resp, err := h.deps.HTTPClient.Do(r)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		return nil, "", upstreamError(resp.StatusCode, b)
	}
	return b, "mp3", nil
}

// === Gemini (base64 PCM → WAV) ===

func (h *Handler) synthGemini(ctx context.Context, cfg tts.Config, req Request) ([]byte, string, error) {
	modelID, voiceID := parseModelVoice(req.Model, cfg)
	tok := credentialToken(req.Credentials)
	gemURL := strings.TrimRight(cfg.BaseURL, "/") + "/" + modelID + ":generateContent?key=" + tok
	body, _ := json.Marshal(map[string]any{
		"contents": []map[string]any{
			{"parts": []map[string]any{{"text": req.Input}}},
		},
		"generationConfig": map[string]any{
			"responseModalities": []string{"AUDIO"},
			"speechConfig": map[string]any{
				"voiceConfig": map[string]any{
					"prebuiltVoiceConfig": map[string]any{"voiceName": voiceID},
				},
			},
		},
	})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, gemURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := h.deps.HTTPClient.Do(r)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		return nil, "", upstreamError(resp.StatusCode, b)
	}
	var g struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, "", fmt.Errorf("gemini tts: decode: %w", err)
	}
	var b64 string
	for _, c := range g.Candidates {
		for _, p := range c.Content.Parts {
			if p.InlineData.Data != "" {
				b64 = p.InlineData.Data
			}
		}
	}
	if b64 == "" {
		return nil, "", errors.New("gemini tts: no audio data in response")
	}
	pcm, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, "", fmt.Errorf("gemini tts: base64 decode: %w", err)
	}
	// Gemini native TTS returns 24kHz 16-bit mono PCM. Wrap into a WAV
	// container so standard players handle it.
	return pcmToWav(pcm, 24000, 16, 1), "wav", nil
}

// === ElevenLabs ===

func (h *Handler) synthElevenLabs(ctx context.Context, cfg tts.Config, req Request) ([]byte, string, error) {
	_, voiceID := parseModelVoice(req.Model, cfg)
	// ElevenLabs default model id (JS uses "eleven_multilingual_v2").
	modelID := "eleven_multilingual_v2"
	body, _ := json.Marshal(map[string]any{
		"text":     req.Input,
		"model_id": modelID,
		"voice_settings": map[string]any{
			"stability":        0.5,
			"similarity_boost": 0.75,
		},
	})
	url := strings.TrimRight(cfg.BaseURL, "/") + "/" + voiceID
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	r.Header.Set("Content-Type", "application/json")
	setAuthHeader(r, cfg, req.Credentials)
	r.Header.Set("Accept", "audio/mpeg")
	resp, err := h.deps.HTTPClient.Do(r)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		return nil, "", upstreamError(resp.StatusCode, b)
	}
	return b, "mp3", nil
}

// === MiniMax (hex → base64) ===

func (h *Handler) synthMiniMax(ctx context.Context, cfg tts.Config, req Request) ([]byte, string, error) {
	modelID, voiceID := parseModelVoice(req.Model, cfg)
	body, _ := json.Marshal(map[string]any{
		"model":          modelID,
		"text":           req.Input,
		"stream":         false,
		"language_boost": "auto",
		"output_format":  "hex",
		"voice_setting":  map[string]any{"voice_id": voiceID, "speed": 1.0, "vol": 1.0, "pitch": 0},
		"audio_setting":  map[string]any{"sample_rate": 32000, "bitrate": 128000, "format": "mp3", "channel": 1},
	})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	r.Header.Set("Content-Type", "application/json")
	setAuthHeader(r, cfg, req.Credentials)
	resp, err := h.deps.HTTPClient.Do(r)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		return nil, "", upstreamError(resp.StatusCode, b)
	}
	var mm struct {
		Data struct {
			Audio string `json:"audio"`
		} `json:"data"`
		ExtraInfo struct {
			AudioFormat string `json:"audio_format"`
		} `json:"extra_info"`
	}
	if err := json.Unmarshal(b, &mm); err != nil {
		return nil, "", fmt.Errorf("minimax tts: decode: %w", err)
	}
	audioBytes, err := hex.DecodeString(mm.Data.Audio)
	if err != nil {
		return nil, "", fmt.Errorf("minimax tts: hex decode: %w", err)
	}
	fmtStr := mm.ExtraInfo.AudioFormat
	if fmtStr == "" {
		fmtStr = "mp3"
	}
	return audioBytes, fmtStr, nil
}

// === Inworld (JSON {audioContent} base64) ===

func (h *Handler) synthInworld(ctx context.Context, cfg tts.Config, req Request) ([]byte, string, error) {
	modelID, voiceID := parseModelVoice(req.Model, cfg)
	body, _ := json.Marshal(map[string]any{
		"text":    req.Input,
		"voiceId": voiceID,
		"modelId": modelID,
		"audioConfig": map[string]any{
			"audioEncoding": "MP3",
		},
	})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	r.Header.Set("Content-Type", "application/json")
	setAuthHeader(r, cfg, req.Credentials)
	resp, err := h.deps.HTTPClient.Do(r)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		return nil, "", upstreamError(resp.StatusCode, b)
	}
	var iw struct {
		AudioContent string `json:"audioContent"`
	}
	if err := json.Unmarshal(b, &iw); err != nil {
		return nil, "", fmt.Errorf("inworld tts: decode: %w", err)
	}
	if iw.AudioContent == "" {
		return nil, "", errors.New("inworld tts: no audioContent")
	}
	audioBytes, err := base64.StdEncoding.DecodeString(iw.AudioContent)
	if err != nil {
		return nil, "", fmt.Errorf("inworld tts: base64 decode: %w", err)
	}
	return audioBytes, "mp3", nil
}

// === Cartesia (raw binary) ===

func (h *Handler) synthCartesia(ctx context.Context, cfg tts.Config, req Request) ([]byte, string, error) {
	modelID, voiceID := parseModelVoice(req.Model, cfg)
	body, _ := json.Marshal(map[string]any{
		"model_id":   modelID,
		"transcript": req.Input,
		"voice":      map[string]any{"mode": "id", "id": voiceID},
		"output_format": map[string]any{
			"container":   "mp3",
			"bit_rate":    128000,
			"sample_rate": 44100,
		},
	})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	r.Header.Set("Content-Type", "application/json")
	setAuthHeader(r, cfg, req.Credentials)
	r.Header.Set("Cartesia-Version", "2024-06-10")
	resp, err := h.deps.HTTPClient.Do(r)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		return nil, "", upstreamError(resp.StatusCode, b)
	}
	return b, "mp3", nil
}

// === PlayHT (userId:apiKey split → raw binary) ===

func (h *Handler) synthPlayHT(ctx context.Context, cfg tts.Config, req Request) ([]byte, string, error) {
	_, voiceID := parseModelVoice(req.Model, cfg)
	tok := credentialToken(req.Credentials)
	// PlayHT credential shape: "userId:apiKey" (the JS split on ":").
	userID := tok
	apiKey := tok
	if i := strings.Index(tok, ":"); i >= 0 {
		userID = tok[:i]
		apiKey = tok[i+1:]
	}
	body, _ := json.Marshal(map[string]any{
		"text":          req.Input,
		"voice":         voiceID,
		"voice_engine":  "PlayHT2.0-turbo",
		"output_format": "mp3",
		"speed":         1.0,
	})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-USER-ID", userID)
	r.Header.Set("Authorization", "Bearer "+apiKey)
	r.Header.Set("Accept", "audio/mpeg")
	resp, err := h.deps.HTTPClient.Do(r)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		return nil, "", upstreamError(resp.StatusCode, b)
	}
	return b, "mp3", nil
}

// === NVIDIA (raw binary) ===

func (h *Handler) synthNvidia(ctx context.Context, cfg tts.Config, req Request) ([]byte, string, error) {
	modelID, voiceID := parseModelVoice(req.Model, cfg)
	body, _ := json.Marshal(map[string]any{
		"input": map[string]any{"text": req.Input},
		"voice": voiceID,
		"model": modelID,
	})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	r.Header.Set("Content-Type", "application/json")
	setAuthHeader(r, cfg, req.Credentials)
	r.Header.Set("Accept", "audio/wav")
	resp, err := h.deps.HTTPClient.Do(r)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		return nil, "", upstreamError(resp.StatusCode, b)
	}
	return b, "wav", nil
}

// === Deepgram (raw binary) ===

func (h *Handler) synthDeepgram(ctx context.Context, cfg tts.Config, req Request) ([]byte, string, error) {
	modelID, _ := parseModelVoice(req.Model, cfg)
	u := cfg.BaseURL + "?model=" + modelID
	body, _ := json.Marshal(map[string]any{"text": req.Input})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	r.Header.Set("Content-Type", "application/json")
	setAuthHeader(r, cfg, req.Credentials)
	resp, err := h.deps.HTTPClient.Do(r)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		return nil, "", upstreamError(resp.StatusCode, b)
	}
	return b, "mp3", nil
}

// === Envelope ===

// envelope wraps the raw audio bytes into the client-requested response
// format: raw binary (mp3) or {"audio":base64,"format"} JSON.
func (h *Handler) envelope(audio []byte, audioFormat, responseFormat string) Result {
	if responseFormat == "json" {
		b, _ := json.Marshal(map[string]string{
			"audio":  base64.StdEncoding.EncodeToString(audio),
			"format": audioFormat,
		})
		return Result{StatusCode: http.StatusOK, Body: b, ContentType: "application/json", AudioFormat: audioFormat}
	}
	return Result{StatusCode: http.StatusOK, Body: audio, ContentType: "audio/" + audioFormat, AudioFormat: audioFormat}
}

// === Auth ===

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

func setAuthHeader(r *http.Request, cfg tts.Config, creds domainProv.Credentials) {
	if cfg.AuthType == tts.AuthTypeNone {
		return
	}
	tok := credentialToken(creds)
	if tok == "" {
		return
	}
	switch cfg.AuthHeader {
	case tts.AuthBearer:
		r.Header.Set("Authorization", "Bearer "+tok)
	case tts.AuthXiAPIKey:
		r.Header.Set("xi-api-key", tok)
	case tts.AuthXAPIKey:
		r.Header.Set("X-API-Key", tok)
	case tts.AuthBasic:
		r.Header.Set("Authorization", "Basic "+tok)
	case tts.AuthToken:
		r.Header.Set("Authorization", "Token "+tok)
	// "key" (Gemini) is applied as a query param at the URL level, not here.
	case tts.AuthPlayHT:
		// Handled in synthPlayHT (needs the userId split).
	}
}

// errResult maps an error to a Result, preserving a sensible status code.
func errResult(err error) Result {
	// upstreamError returns an error whose string is the message; the caller
	// surfaces it. Default to 502 Bad Gateway for transport errors.
	return Result{StatusCode: http.StatusBadGateway, Err: err}
}

// upstreamError parses an upstream error body into an error.
func upstreamError(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return fmt.Errorf("upstream error (%d)", status)
	}
	var j struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &j); err == nil {
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

// pcmToWav wraps raw PCM bytes into a minimal WAV container with the given
// sample rate, bits-per-sample, and channel count. Gemini native TTS returns
// 24kHz 16-bit mono PCM; the JS adapter wraps it identically.
func pcmToWav(pcm []byte, sampleRate, bitsPerSample, channels int) []byte {
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	dataLen := len(pcm)
	var buf bytes.Buffer
	// RIFF header
	buf.WriteString("RIFF")
	writeUint32(&buf, uint32(36+dataLen))
	buf.WriteString("WAVE")
	// fmt chunk
	buf.WriteString("fmt ")
	writeUint32(&buf, 16)
	writeUint16(&buf, 1) // PCM
	writeUint16(&buf, uint16(channels))
	writeUint32(&buf, uint32(sampleRate))
	writeUint32(&buf, uint32(byteRate))
	writeUint16(&buf, uint16(blockAlign))
	writeUint16(&buf, uint16(bitsPerSample))
	// data chunk
	buf.WriteString("data")
	writeUint32(&buf, uint32(dataLen))
	buf.Write(pcm)
	return buf.Bytes()
}

func writeUint32(buf *bytes.Buffer, v uint32) {
	buf.Write([]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
}

func writeUint16(buf *bytes.Buffer, v uint16) {
	buf.Write([]byte{byte(v), byte(v >> 8)})
}
