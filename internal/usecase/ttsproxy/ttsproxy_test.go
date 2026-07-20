package ttsproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/tts"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

type captureLogger struct{}

func (captureLogger) Infof(string, ...any)  {}
func (captureLogger) Warnf(string, ...any)  {}
func (captureLogger) Debugf(string, ...any) {}

func creds(apiKey string) domainProv.Credentials {
	return domainProv.Credentials{APIKey: apiKey, ProviderSpecificData: map[string]any{"_connectionId": "c1"}}
}

// === Dispatch / validation ===

func TestHandle_UnsupportedProvider(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "nope", Input: "hi"})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

func TestHandle_DeferredProviderReturns501(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "edge-tts", Input: "hi"})
	if res.StatusCode != http.StatusNotImplemented {
		t.Errorf("edge-tts should 501 in Go build, got %d", res.StatusCode)
	}
}

func TestHandle_MissingInput(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "openai", Input: "  "})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing input)", res.StatusCode)
	}
}

func TestHandle_NoCredentials(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "openai", Input: "hi"})
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
}

// === OpenAI ===

func TestHandle_OpenAI_RawMP3(t *testing.T) {
	var gotAuth string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("MP3BYTES"))
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := ttsCfg(srv.URL, "bearer", "openai")
	audio, fmtStr, err := h.synthOpenAI(context.Background(), cfg, Request{
		ProviderID: "openai", Model: "gpt-4o-mini-tts/alloy", Input: "hello",
		Credentials: creds("k"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("auth = %q", gotAuth)
	}
	if !contains(gotBody, `"model":"gpt-4o-mini-tts"`) || !contains(gotBody, `"voice":"alloy"`) || !contains(gotBody, `"input":"hello"`) {
		t.Errorf("body = %q", gotBody)
	}
	if string(audio) != "MP3BYTES" || fmtStr != "mp3" {
		t.Errorf("audio = %q fmt = %q", audio, fmtStr)
	}
}

// === Gemini PCM → WAV ===

func TestHandle_Gemini_PCMToWAV(t *testing.T) {
	pcm := bytes.Repeat([]byte{0xAB, 0xCD}, 100) // 200 bytes of PCM
	b64 := base64.StdEncoding.EncodeToString(pcm)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !contains(r.URL.RawQuery, "key=k-gem") {
			t.Errorf("query = %q, want key=k-gem", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/L16","data":"`+b64+`"}}]}}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := ttsCfg(srv.URL, "key", "gemini-tts")
	audio, fmtStr, err := h.synthGemini(context.Background(), cfg, Request{
		ProviderID: "gemini", Model: "gemini-2.5-flash-preview-tts/Charlize", Input: "hi",
		Credentials: creds("k-gem"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmtStr != "wav" {
		t.Errorf("format = %q, want wav", fmtStr)
	}
	if !bytes.HasPrefix(audio, []byte("RIFF")) || !bytes.Contains(audio[:16], []byte("WAVE")) {
		t.Errorf("not a WAV: %x...", audio[:16])
	}
	if !bytes.Contains(audio, pcm) {
		t.Error("PCM payload not present in WAV")
	}
}

// === ElevenLabs ===

func TestHandle_ElevenLabs(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("xi-api-key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte("MP3"))
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := ttsCfg(srv.URL, "xi-api-key", "elevenlabs")
	audio, fmtStr, err := h.synthElevenLabs(context.Background(), cfg, Request{
		ProviderID: "elevenlabs", Model: "eleven_multilingual_v2/voiceX", Input: "hi", Credentials: creds("k-el"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/voiceX" {
		t.Errorf("path = %q, want /voiceX", gotPath)
	}
	if gotAuth != "k-el" {
		t.Errorf("xi-api-key = %q", gotAuth)
	}
	if !contains(gotBody, `"model_id":"eleven_multilingual_v2"`) {
		t.Errorf("body = %q", gotBody)
	}
	if string(audio) != "MP3" || fmtStr != "mp3" {
		t.Errorf("audio=%q fmt=%q", audio, fmtStr)
	}
}

// === MiniMax hex → bytes ===

func TestHandle_MiniMax_HexDecode(t *testing.T) {
	audioHex := hex.EncodeToString([]byte("AUDIODATA"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"audio":"`+audioHex+`"},"extra_info":{"audio_format":"mp3"}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := ttsCfg(srv.URL, "bearer", "minimax-tts")
	audio, fmtStr, err := h.synthMiniMax(context.Background(), cfg, Request{
		ProviderID: "minimax", Model: "speech-02-hd/voiceY", Input: "hi", Credentials: creds("k-mm"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(audio) != "AUDIODATA" {
		t.Errorf("audio = %q, want AUDIODATA", audio)
	}
	if fmtStr != "mp3" {
		t.Errorf("format = %q", fmtStr)
	}
}

// === Inworld base64 ===

func TestHandle_Inworld_Base64(t *testing.T) {
	audioB64 := base64.StdEncoding.EncodeToString([]byte("INAUDIO"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic k-inw" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"audioContent":"`+audioB64+`"}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := ttsCfg(srv.URL, "basic", "inworld")
	audio, _, err := h.synthInworld(context.Background(), cfg, Request{
		ProviderID: "inworld", Model: "inworld-tts-1.5-mini/v1", Input: "hi", Credentials: creds("k-inw"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(audio) != "INAUDIO" {
		t.Errorf("audio = %q", audio)
	}
}

// === Cartesia ===

func TestHandle_Cartesia(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-API-Key")
		_, _ = w.Write([]byte("CARTBYTES"))
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := ttsCfg(srv.URL, "x-api-key", "cartesia")
	audio, _, err := h.synthCartesia(context.Background(), cfg, Request{
		ProviderID: "cartesia", Model: "sonic-2/voiceZ", Input: "hi", Credentials: creds("k-cart"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "k-cart" {
		t.Errorf("X-API-Key = %q", gotAuth)
	}
	if string(audio) != "CARTBYTES" {
		t.Errorf("audio = %q", audio)
	}
}

// === PlayHT userId:apiKey split ===

func TestHandle_PlayHT_SplitAuth(t *testing.T) {
	var gotUser, gotBearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("X-USER-ID")
		gotBearer = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("PLAYHTBYTES"))
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := ttsCfg(srv.URL, "playht", "playht")
	audio, _, err := h.synthPlayHT(context.Background(), cfg, Request{
		ProviderID: "playht", Model: "PlayHT2.0-turbo/voiceW", Input: "hi",
		Credentials: creds("user123:apikey456"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotUser != "user123" {
		t.Errorf("X-USER-ID = %q, want user123", gotUser)
	}
	if gotBearer != "Bearer apikey456" {
		t.Errorf("Authorization = %q, want Bearer apikey456", gotBearer)
	}
	_ = audio
}

// === NVIDIA / Deepgram ===

func TestHandle_Nvidia(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("WAVBYTES"))
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := ttsCfg(srv.URL, "bearer", "nvidia-tts")
	_, fmtStr, err := h.synthNvidia(context.Background(), cfg, Request{
		ProviderID: "nvidia", Model: "tts-en-us-cmu-arctic-flash/v1", Input: "hi", Credentials: creds("k"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmtStr != "wav" {
		t.Errorf("format = %q, want wav", fmtStr)
	}
}

func TestHandle_Deepgram_ModelQueryParam(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		_, _ = w.Write([]byte("DGMP3"))
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := ttsCfg(srv.URL, "token", "deepgram")
	_, _, err := h.synthDeepgram(context.Background(), cfg, Request{
		ProviderID: "deepgram", Model: "aura-2-thalia-en/v1", Input: "hi", Credentials: creds("k-dg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(gotPath, "model=aura-2-thalia-en") {
		t.Errorf("path = %q", gotPath)
	}
}

// === Envelope (mp3 binary vs json wrapper) ===

func TestEnvelope_RawBinary(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.envelope([]byte("AUDIO"), "mp3", "mp3")
	if res.ContentType != "audio/mp3" {
		t.Errorf("CT = %q", res.ContentType)
	}
	if string(res.Body) != "AUDIO" {
		t.Errorf("body = %q", res.Body)
	}
}

func TestEnvelope_JSONWrapper(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.envelope([]byte("AUDIO"), "wav", "json")
	if res.ContentType != "application/json" {
		t.Errorf("CT = %q", res.ContentType)
	}
	if !bytes.Contains(res.Body, []byte(`"format":"wav"`)) {
		t.Errorf("body missing format: %q", res.Body)
	}
	if !bytes.Contains(res.Body, []byte(base64.StdEncoding.EncodeToString([]byte("AUDIO")))) {
		t.Errorf("body missing base64 audio: %q", res.Body)
	}
}

// === pcmToWav ===

func TestPcmToWav_Structure(t *testing.T) {
	pcm := []byte{0x01, 0x02, 0x03, 0x04}
	wav := pcmToWav(pcm, 24000, 16, 1)
	if !bytes.HasPrefix(wav, []byte("RIFF")) {
		t.Error("missing RIFF")
	}
	if !bytes.Contains(wav, []byte("WAVE")) {
		t.Error("missing WAVE")
	}
	if !bytes.Contains(wav, []byte("fmt ")) {
		t.Error("missing fmt chunk")
	}
	if !bytes.Contains(wav, []byte("data")) {
		t.Error("missing data chunk")
	}
	if !bytes.HasSuffix(wav, pcm) {
		t.Error("PCM payload should be the trailing bytes")
	}
}

// === Helpers ===

func ttsCfg(baseURL, authHeader, format string) tts.Config {
	return tts.Config{BaseURL: baseURL, AuthHeader: tts.AuthHeader(authHeader), Format: tts.Format(format), AuthType: tts.AuthTypeAPIKey}
}

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }