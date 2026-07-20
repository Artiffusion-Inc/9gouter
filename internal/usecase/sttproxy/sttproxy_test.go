package sttproxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/stt"
	domainProv "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

type captureLogger struct{}

func (captureLogger) Infof(string, ...any)  {}
func (captureLogger) Warnf(string, ...any)  {}
func (captureLogger) Debugf(string, ...any) {}

func creds(apiKey string) domainProv.Credentials {
	return domainProv.Credentials{APIKey: apiKey, ProviderSpecificData: map[string]any{"_connectionId": "conn-1"}}
}

// === Lookup / dispatch ===

func TestHandle_UnsupportedProvider(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "nope", File: []byte("x")})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
	if res.Err == nil {
		t.Error("want error for unsupported provider")
	}
}

func TestHandle_MissingFile(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "openai"})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing file)", res.StatusCode)
	}
}

func TestHandle_NoCredentials(t *testing.T) {
	h := New(Dependencies{Logger: captureLogger{}, Config: config.Config{}})
	res := h.Handle(context.Background(), Request{ProviderID: "openai", File: []byte("x")})
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (no creds)", res.StatusCode)
	}
}

// === OpenAI-compatible ===

func TestHandle_OpenAICompatible_RawPassthrough(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"hello world"}`)
	}))
	defer srv.Close()

	h := New(Dependencies{
		HTTPClient: srv.Client(),
		Logger:     captureLogger{},
		Config:     config.Config{},
	})
	// Override the openai base URL by pointing the config map via a custom
	// handler: we exercise transcribeOpenAICompatible directly to avoid
	// mutating the static registry.
	cfg := sttCfg("https://api.openai.com/v1/audio/transcriptions", "bearer", "openai")
	cfg.BaseURL = srv.URL
	res := h.transcribeOpenAICompatible(context.Background(), cfg, Request{
		ProviderID:  "openai",
		Model:       "whisper-1",
		File:        []byte("AUDIOBYTES"),
		Filename:    "clip.wav",
		FormFields:  map[string]string{"language": "en", "response_format": "json"},
		Credentials: creds("k-openai"),
	}, "k-openai")
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q", gotMethod)
	}
	if gotPath != "/" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer k-openai" {
		t.Errorf("auth = %q", gotAuth)
	}
	if !bytes.HasPrefix([]byte(gotCT), []byte("multipart/form-data")) {
		t.Errorf("Content-Type = %q, want multipart/form-data", gotCT)
	}
	// Forwarded multipart must carry the audio bytes + model + language.
	if !bytes.Contains(gotBody, []byte("AUDIOBYTES")) {
		t.Error("audio bytes not forwarded")
	}
	if !bytes.Contains(gotBody, []byte("whisper-1")) {
		t.Error("model field not forwarded")
	}
	if !bytes.Contains(gotBody, []byte("en")) {
		t.Error("language field not forwarded")
	}
	if res.ContentType != "application/json" {
		t.Errorf("response CT = %q", res.ContentType)
	}
	if !bytes.Contains(res.Body, []byte(`"text":"hello world"`)) {
		t.Errorf("response body = %q", res.Body)
	}
}

func TestHandle_OpenAICompatible_ResponseFormatTextPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "transcribed text plain")
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := sttCfg(srv.URL, "bearer", "openai")
	res := h.transcribeOpenAICompatible(context.Background(), cfg, Request{
		ProviderID: "groq", Model: "whisper-large-v3", File: []byte("x"), Credentials: creds("k"),
	}, "k")
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.ContentType != "text/plain" {
		t.Errorf("CT = %q, want text/plain (passthrough)", res.ContentType)
	}
	if string(res.Body) != "transcribed text plain" {
		t.Errorf("body = %q", res.Body)
	}
}

func TestHandle_OpenAICompatible_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := sttCfg(srv.URL, "bearer", "openai")
	res := h.transcribeOpenAICompatible(context.Background(), cfg, Request{
		ProviderID: "openai", Model: "whisper-1", File: []byte("x"), Credentials: creds("k"),
	}, "k")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 passthrough", res.StatusCode)
	}
	if res.Err == nil {
		t.Error("want upstream error")
	}
}

// === Deepgram ===

func TestHandle_Deepgram_Reshape(t *testing.T) {
	var gotAuth, gotCT string
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":{"channels":[{"alternatives":[{"transcript":"hi there"}]}]}}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := sttCfg(srv.URL, "token", "deepgram")
	res := h.transcribeDeepgram(context.Background(), cfg, Request{
		ProviderID: "deepgram", Model: "nova-3", File: []byte("AUD"),
		Filename: "clip.mp3", FormFields: map[string]string{"language": "en"}, Credentials: creds("k-dg"),
	}, "k-dg")
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if gotAuth != "Token k-dg" {
		t.Errorf("auth = %q, want Token k-dg", gotAuth)
	}
	if gotCT != "audio/mpeg" {
		t.Errorf("CT = %q, want audio/mpeg (from .mp3 ext)", gotCT)
	}
	if !contains(gotQuery, "model=nova-3") || !contains(gotQuery, "smart_format=true") || !contains(gotQuery, "language=en") {
		t.Errorf("query = %q", gotQuery)
	}
	if res.ContentType != "application/json" {
		t.Errorf("response CT = %q", res.ContentType)
	}
	if !bytes.Contains(res.Body, []byte(`"text":"hi there"`)) {
		t.Errorf("reshaped body = %q", res.Body)
	}
}

func TestHandle_Deepgram_DetectLanguage(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":{"channels":[{"alternatives":[{"transcript":""}]}}]}`)
	}))
	defer srv.Close()
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := sttCfg(srv.URL, "token", "deepgram")
	_ = h.transcribeDeepgram(context.Background(), cfg, Request{
		ProviderID: "deepgram", Model: "nova-3", File: []byte("AUD"), Filename: "c.wav", Credentials: creds("k"),
	}, "k")
	if !contains(gotQuery, "detect_language=true") {
		t.Errorf("query should set detect_language when no language given: %q", gotQuery)
	}
}

// === Gemini ===

func TestHandle_Gemini_Reshape(t *testing.T) {
	var gotQuery string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"gemini transcript"}]}}]}`)
	}))
	defer srv.Close()
	// Gemini URL is {BaseURL}/{model}:generateContent?key={token}. Point BaseURL
	// at the mock server root.
	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := sttCfg(srv.URL, "key", "gemini")
	res := h.transcribeGemini(context.Background(), cfg, Request{
		ProviderID: "gemini", Model: "gemini-2.0-flash", File: []byte("AUD"),
		Filename: "c.wav", FormFields: map[string]string{"language": "fr", "prompt": "transcribe"},
		Credentials: creds("k-gem"),
	}, "k-gem")
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !contains(gotQuery, "key=k-gem") {
		t.Errorf("query = %q, want key=k-gem", gotQuery)
	}
	// inline_data + language-appended prompt + base64 audio present.
	if !contains(gotBody, "inline_data") {
		t.Errorf("body missing inline_data: %s", gotBody)
	}
	if !contains(gotBody, "transcribe") || !contains(gotBody, "Language: fr") {
		t.Errorf("body missing prompt/language: %s", gotBody)
	}
	if !bytes.Contains(res.Body, []byte(`"text":"gemini transcript"`)) {
		t.Errorf("reshaped body = %q", res.Body)
	}
}

// === AssemblyAI (upload → submit → poll) ===

func TestHandle_AssemblyAI_Reshape(t *testing.T) {
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[r.URL.Path]++
		switch r.URL.Path {
		case "/v2/upload":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"upload_url":"https://up.test/audio"}`)
		case "/v2/transcript":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"tx-123"}`)
		case "/v2/transcript/tx-123":
			w.Header().Set("Content-Type", "application/json")
			// First poll: queued; second: completed. The handler polls every 2s,
			// so this would be slow — instead return completed immediately.
			_, _ = io.WriteString(w, `{"status":"completed","text":"assembly result"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	h := New(Dependencies{HTTPClient: srv.Client(), Logger: captureLogger{}, Config: config.Config{}})
	cfg := sttCfg(srv.URL+"/v2/transcript", "authorization", "assemblyai")
	cfg.UploadURL = srv.URL + "/v2/upload"
	res := h.transcribeAssemblyAI(context.Background(), cfg, Request{
		ProviderID: "assemblyai", Model: "best", File: []byte("AUD"),
		Filename: "c.wav", Credentials: creds("k-aai"),
	}, "k-aai")
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if calls["/v2/upload"] != 1 || calls["/v2/transcript"] != 1 || calls["/v2/transcript/tx-123"] < 1 {
		t.Errorf("call counts = %v", calls)
	}
	if !bytes.Contains(res.Body, []byte(`"text":"assembly result"`)) {
		t.Errorf("reshaped body = %q", res.Body)
	}
}

// === Auth header schemes ===

func TestSetAuthHeader(t *testing.T) {
	cases := []struct{ header, expect string }{
		{"bearer", "Bearer tok"},
		{"token", "Token tok"},
		{"x-api-key", ""}, // x-api-key goes to its own header
		{"key", "Key tok"},
		{"authorization", "tok"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "http://x", nil)
		cfg := sttCfg("http://x", c.header, "openai")
		setAuthHeader(r, cfg, "tok")
		if c.header == "x-api-key" {
			if r.Header.Get("x-api-key") != "tok" {
				t.Errorf("x-api-key: got %q", r.Header.Get("x-api-key"))
			}
			continue
		}
		if r.Header.Get("Authorization") != c.expect {
			t.Errorf("header %q: got %q, want %q", c.header, r.Header.Get("Authorization"), c.expect)
		}
	}
}

// === resolveAudioContentType ===

func TestResolveAudioContentType(t *testing.T) {
	cases := []struct{ mime, name, want string }{
		{"audio/wav", "x", "audio/wav"},
		{"", "song.mp3", "audio/mpeg"},
		{"", "clip.m4a", "audio/mp4"},
		{"", "clip.wav", "audio/wav"},
		{"", "clip.flac", "audio/flac"},
		{"", "clip.webm", "audio/webm"},
		{"", "clip.opus", "audio/opus"},
		{"", "clip.bin", "application/octet-stream"},
	}
	for _, c := range cases {
		if got := resolveAudioContentType(c.mime, c.name); got != c.want {
			t.Errorf("resolveAudioContentType(%q,%q) = %q, want %q", c.mime, c.name, got, c.want)
		}
	}
}

// === upstreamError ===

func TestUpstreamError(t *testing.T) {
	if err := upstreamError(500, []byte(`{"error":{"message":"boom"}}`)); err == nil || err.Error() != "boom" {
		t.Errorf("want 'boom', got %v", err)
	}
	// Unknown JSON shape → fallback to the raw body text.
	if err := upstreamError(500, []byte(`{"msg":"x"}`)); err == nil || err.Error() != `{"msg":"x"}` {
		t.Errorf("want raw body fallback, got %v", err)
	}
	if err := upstreamError(500, nil); err == nil {
		t.Error("want generic upstream error")
	}
}

// === Helpers ===

// sttCfg builds a stt.Config with the given base URL + auth header + format so
// tests can point at httptest servers without mutating the package registry.
func sttCfg(baseURL, authHeader, format string) stt.Config {
	return stt.Config{BaseURL: baseURL, AuthHeader: stt.AuthHeader(authHeader), Format: stt.Format(format), AuthType: stt.AuthTypeAPIKey}
}

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }
