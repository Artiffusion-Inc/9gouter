package http

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	"github.com/Artiffusion-Inc/9router/internal/adapter/db/repo"
)

// --- parseV1BetaModelAction ---

func TestParseV1BetaModelAction_Streaming(t *testing.T) {
	m, stream, ok := parseV1BetaModelAction("gemini-2.0-flash:streamGenerateContent")
	if !ok || !stream || m != "gemini-2.0-flash" {
		t.Fatalf("got (%q,%v,%v) want (gemini-2.0-flash,true,true)", m, stream, ok)
	}
}

func TestParseV1BetaModelAction_NonStreaming(t *testing.T) {
	m, stream, ok := parseV1BetaModelAction("gemini-2.0-flash:generateContent")
	if !ok || stream || m != "gemini-2.0-flash" {
		t.Fatalf("got (%q,%v,%v) want (gemini-2.0-flash,false,true)", m, stream, ok)
	}
}

func TestParseV1BetaModelAction_ProviderSlashModel(t *testing.T) {
	m, stream, ok := parseV1BetaModelAction("gemini/gemini-2.0-flash:streamGenerateContent")
	if !ok || !stream || m != "gemini/gemini-2.0-flash" {
		t.Fatalf("got (%q,%v,%v) want (gemini/gemini-2.0-flash,true,true)", m, stream, ok)
	}
}

func TestParseV1BetaModelAction_NoAction(t *testing.T) {
	_, _, ok := parseV1BetaModelAction("gemini-2.0-flash")
	if ok {
		t.Fatal("expected ok=false for path without action suffix")
	}
}

// --- convertGeminiToInternal ---

func TestConvertGeminiToInternal_SystemAndContents(t *testing.T) {
	body := `{
		"systemInstruction": {"parts": [{"text": "be brief"}]},
		"contents": [
			{"role": "user", "parts": [{"text": "hi"}]},
			{"role": "model", "parts": [{"text": "hello"}]},
			{"role": "user", "parts": [{"text": "bye"}]}
		],
		"generationConfig": {"maxOutputTokens": 128, "temperature": 0.5, "topP": 0.9}
	}`
	out, err := convertGeminiToInternal([]byte(body), "gemini-2.0-flash", true)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "gemini-2.0-flash" {
		t.Errorf("model = %v", got["model"])
	}
	if got["stream"] != true {
		t.Errorf("stream = %v", got["stream"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d, want 4", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Errorf("first msg = %v", first)
	}
	// messages = [system, user(hi), assistant(hello), user(bye)] — the
	// "model" role maps to "assistant" and lands at index 2 (after system+user).
	third := msgs[2].(map[string]any)
	if third["role"] != "assistant" {
		t.Errorf("model role should map to assistant, got %v", third["role"])
	}
	if got["max_tokens"].(float64) != 128 {
		t.Errorf("max_tokens = %v", got["max_tokens"])
	}
	if got["temperature"].(float64) != 0.5 {
		t.Errorf("temperature = %v", got["temperature"])
	}
	if got["top_p"].(float64) != 0.9 {
		t.Errorf("top_p = %v", got["top_p"])
	}
}

func TestConvertGeminiToInternal_NoSystemNoGenConfig(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	out, err := convertGeminiToInternal([]byte(body), "m", false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if _, ok := got["max_tokens"]; ok {
		t.Error("max_tokens should be absent")
	}
	if _, ok := got["temperature"]; ok {
		t.Error("temperature should be absent")
	}
	if got["stream"] != false {
		t.Errorf("stream = %v", got["stream"])
	}
}

// --- isV1BetaGeminiNativeTTS ---

func TestIsV1BetaGeminiNativeTTS_AudioModality(t *testing.T) {
	body := []byte(`{"generationConfig":{"responseModalities":["TEXT","AUDIO"]}}`)
	if !isV1BetaGeminiNativeTTS("gemini-2.5-flash-preview-tts", body) {
		t.Fatal("expected TTS detection for AUDIO modality")
	}
}

func TestIsV1BetaGeminiNativeTTS_TextOnly(t *testing.T) {
	body := []byte(`{"generationConfig":{"responseModalities":["TEXT"]}}`)
	if isV1BetaGeminiNativeTTS("gemini-2.0-flash", body) {
		t.Fatal("expected no TTS for TEXT-only modality")
	}
}

// --- buildV1BetaUsageMetadata ---

func TestBuildV1BetaUsageMetadata_WithReasoning(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":     10.0,
		"completion_tokens": 20.0,
		"total_tokens":      30.0,
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": 5.0,
		},
	}
	meta := buildV1BetaUsageMetadata(usage)
	if meta["promptTokenCount"] != 10 {
		t.Errorf("promptTokenCount = %v", meta["promptTokenCount"])
	}
	if meta["candidatesTokenCount"] != 20 {
		t.Errorf("candidatesTokenCount = %v", meta["candidatesTokenCount"])
	}
	if meta["totalTokenCount"] != 30 {
		t.Errorf("totalTokenCount = %v", meta["totalTokenCount"])
	}
	if meta["thoughtsTokenCount"] != 5 {
		t.Errorf("thoughtsTokenCount = %v", meta["thoughtsTokenCount"])
	}
}

func TestBuildV1BetaUsageMetadata_NoReasoning(t *testing.T) {
	meta := buildV1BetaUsageMetadata(map[string]any{"prompt_tokens": 1.0, "completion_tokens": 2.0, "total_tokens": 3.0})
	if _, ok := meta["thoughtsTokenCount"]; ok {
		t.Error("thoughtsTokenCount should be absent when reasoning_tokens missing/0")
	}
}

// --- convertOpenAISSEToGeminiSSE ---

func TestConvertOpenAISSEToGeminiSSE_ContentAndFinish(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5},\"model\":\"gemini-2.0-flash\"}\n\n" +
		"data: [DONE]\n\n"
	rw := httptest.NewRecorder()
	if err := convertOpenAISSEToGeminiSSE(rw, bytes.NewReader([]byte(sse)), "fallback-model"); err != nil {
		t.Fatal(err)
	}
	out := rw.Body.String()
	if !strings.Contains(out, "\"text\":\"Hi\"") {
		t.Errorf("missing Hi chunk: %s", out)
	}
	if !strings.Contains(out, "\"text\":\" there\"") {
		t.Errorf("missing 'there' chunk: %s", out)
	}
	if !strings.Contains(out, "\"finishReason\":\"STOP\"") {
		t.Errorf("missing finishReason: %s", out)
	}
	if !strings.Contains(out, "\"modelVersion\":\"gemini-2.0-flash\"") {
		t.Errorf("missing modelVersion: %s", out)
	}
	if !strings.Contains(out, "\"totalTokenCount\":5") {
		t.Errorf("missing usageMetadata: %s", out)
	}
	if strings.Contains(out, "[DONE]") {
		t.Errorf("[DONE] sentinel must not leak into Gemini SSE: %s", out)
	}
}

func TestConvertOpenAISSEToGeminiSSE_ReasoningContent(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking...\"}}]}\n\n"
	rw := httptest.NewRecorder()
	if err := convertOpenAISSEToGeminiSSE(rw, bytes.NewReader([]byte(sse)), "m"); err != nil {
		t.Fatal(err)
	}
	out := rw.Body.String()
	if !strings.Contains(out, "\"thought\":true") {
		t.Errorf("reasoning_content should become a thought part: %s", out)
	}
	if !strings.Contains(out, "\"text\":\"thinking...\"") {
		t.Errorf("missing thought text: %s", out)
	}
}

// --- convertOpenAIJSONToGemini ---

func TestConvertOpenAIJSONToGemini_Basic(t *testing.T) {
	openai := `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"model":"gemini-2.0-flash","usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	rw := httptest.NewRecorder()
	if err := convertOpenAIJSONToGemini(rw, bytes.NewReader([]byte(openai)), "fallback"); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	cands, _ := got["candidates"].([]any)
	if len(cands) != 1 {
		t.Fatalf("candidates len = %d", len(cands))
	}
	cand := cands[0].(map[string]any)
	if cand["finishReason"] != "STOP" {
		t.Errorf("finishReason = %v", cand["finishReason"])
	}
	if got["modelVersion"] != "gemini-2.0-flash" {
		t.Errorf("modelVersion = %v", got["modelVersion"])
	}
	if got["usageMetadata"].(map[string]any)["totalTokenCount"] != float64(3) {
		t.Errorf("usageMetadata wrong: %v", got["usageMetadata"])
	}
}

func TestConvertOpenAIJSONToGemini_AlreadyGeminiPassthrough(t *testing.T) {
	gemini := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}]}`
	rw := httptest.NewRecorder()
	if err := convertOpenAIJSONToGemini(rw, bytes.NewReader([]byte(gemini)), "m"); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(rw.Body.Bytes(), &got)
	if _, ok := got["candidates"]; !ok {
		t.Errorf("candidates passthrough lost: %s", rw.Body.String())
	}
}

func TestConvertOpenAIJSONToGemini_ReasoningContent(t *testing.T) {
	openai := `{"choices":[{"message":{"role":"assistant","reasoning_content":"thought","content":"answer"},"finish_reason":"stop"}]}`
	rw := httptest.NewRecorder()
	if err := convertOpenAIJSONToGemini(rw, bytes.NewReader([]byte(openai)), "m"); err != nil {
		t.Fatal(err)
	}
	out := rw.Body.String()
	if !strings.Contains(out, "\"thought\":true") {
		t.Errorf("reasoning_content should become thought part: %s", out)
	}
}

// --- HTTP handler: GET /v1beta/models list ---

// newV1BetaDeps builds a minimal V1Deps wired against a temp DB. Mirrors the
// field set the existing v1_test.go helpers construct manually.
func newV1BetaDeps(t *testing.T, db *sql.DB, chat ChatHandler) V1Deps {
	t.Helper()
	return V1Deps{
		APIKeysRepo:   repo.NewAPIKeyRepo(db),
		SettingsRepo:  repo.NewSettingsRepo(db),
		ConnectionRepo: repo.NewConnectionRepo(db),
		ComboRepo:     repo.NewComboRepo(db),
		AliasRepo:     repo.NewAliasRepo(db),
		NodeRepo:      repo.NewNodeRepo(db),
		ProxyPoolRepo: repo.NewProxyPoolRepo(db),
		Chat:          chat,
		Config:        config.Config{ProxyClientMaxBodySize: "128mb"},
		Logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

func newV1BetaMux(t *testing.T) *http.ServeMux {
	t.Helper()
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	deps := newV1BetaDeps(t, db, &stubChatHandler{})
	mux := http.NewServeMux()
	RegisterV1(mux, deps)
	return mux
}

func TestV1BetaModels_List_NonEmpty(t *testing.T) {
	mux := newV1BetaMux(t)
	req := httptest.NewRequest("GET", "/v1beta/models", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	var got struct {
		Models []v1betaModelEntry `json:"models"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) == 0 {
		t.Fatal("model list is empty")
	}
	// Gemini provider emits bare models/<id> entries advertising both
	// generateContent and streamGenerateContent.
	var hasStreamMethod bool
	for _, m := range got.Models {
		for _, meth := range m.SupportedGenerationMethods {
			if meth == "streamGenerateContent" {
				hasStreamMethod = true
			}
		}
	}
	if !hasStreamMethod {
		t.Errorf("no entry advertises streamGenerateContent")
	}
	if !strings.Contains(rw.Body.String(), "inputTokenLimit") {
		t.Errorf("entries missing inputTokenLimit")
	}
}

// TestV1BetaModelsPath_TTSNoConnections_503 verifies the TTS forward returns
// 503 (exhausted) when no active gemini connection exists, instead of the
// previous honest-501 stub.
func TestV1BetaModelsPath_TTSNoConnections_503(t *testing.T) {
	mux := newV1BetaMux(t) // no gemini connection created
	body := `{"contents":[{"role":"user","parts":[{"text":"say hi"}]}],"generationConfig":{"responseModalities":["AUDIO"]}}`
	req := httptest.NewRequest("POST", "/v1beta/models/gemini-2.5-flash-preview-tts:generateContent", bytes.NewReader([]byte(body)))
	req.RemoteAddr = "127.0.0.1:12345"
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (exhausted, no gemini connection); body=%s", rw.Code, rw.Body.String())
	}
}

// TestV1BetaModelsPath_TTSBadModel_400 verifies the TTS forward rejects a
// model id that fails the charset guard (path-traversal block).
func TestV1BetaModelsPath_TTSBadModel_400(t *testing.T) {
	mux := newV1BetaMux(t)
	body := `{"generationConfig":{"responseModalities":["AUDIO"]}}`
	// "../escape" fails v1betaGeminiModelPattern.
	req := httptest.NewRequest("POST", "/v1beta/models/..%2Fescape:generateContent", bytes.NewReader([]byte(body)))
	req.RemoteAddr = "127.0.0.1:12345"
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (invalid model charset); body=%s", rw.Code, rw.Body.String())
	}
}

// TestV1BetaModelsPath_TTSForward_Success verifies the raw-byte TTS forward
// proxies a successful upstream response with CORS + stripped compression
// headers, using a local httptest server as the upstream.
func TestV1BetaModelsPath_TTSForward_Success(t *testing.T) {
	// Spin up a mock upstream that returns 200 + a binary body.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "gem-key" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "audio/pcm")
		w.Header().Set("Content-Length", "9")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("PCM-BYTES"))
	}))
	t.Cleanup(upstream.Close)

	prevBase := v1betaGeminiNativeBaseURL
	v1betaGeminiNativeBaseURL = upstream.URL
	t.Cleanup(func() { v1betaGeminiNativeBaseURL = prevBase })

	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	mustCreateConnection(t, db, "gemini", `{"apiKey":"gem-key","providerSpecificData":{"connectionProxyEnabled":false}}`)
	deps := newV1BetaDeps(t, db, &stubChatHandler{})
	mux := http.NewServeMux()
	RegisterV1(mux, deps)

	body := `{"contents":[{"role":"user","parts":[{"text":"say hi"}]}],"generationConfig":{"responseModalities":["AUDIO"]}}`
	req := httptest.NewRequest("POST", "/v1beta/models/gemini-2.5-flash-preview-tts:generateContent", bytes.NewReader([]byte(body)))
	req.RemoteAddr = "127.0.0.1:12345"
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	if rw.Body.String() != "PCM-BYTES" {
		t.Errorf("body = %q, want PCM-BYTES", rw.Body.String())
	}
	if rw.Header().Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding must be stripped, got %q", rw.Header().Get("Content-Encoding"))
	}
	if rw.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("missing CORS header")
	}
}

// TestV1BetaModelsPath_TTSForward_5xxRotates verifies a 5xx upstream rotates
// to the next active gemini connection.
func TestV1BetaModelsPath_TTSForward_5xxRotates(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		key := r.Header.Get("x-goog-api-key")
		if key == "bad-key" {
			http.Error(w, "upstream 502", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "audio/pcm")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK-BYTES"))
	}))
	t.Cleanup(upstream.Close)

	prevBase := v1betaGeminiNativeBaseURL
	v1betaGeminiNativeBaseURL = upstream.URL
	t.Cleanup(func() { v1betaGeminiNativeBaseURL = prevBase })

	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	// Two gemini connections with distinct ids: the first (bad-key) returns
	// 502 and rotates; the second (good-key) succeeds.
	mustCreateConnectionWithID(t, db, "gemini-bad", "gemini", `{"apiKey":"bad-key","providerSpecificData":{"connectionProxyEnabled":false}}`)
	mustCreateConnectionWithID(t, db, "gemini-good", "gemini", `{"apiKey":"good-key","providerSpecificData":{"connectionProxyEnabled":false}}`)
	deps := newV1BetaDeps(t, db, &stubChatHandler{})
	mux := http.NewServeMux()
	RegisterV1(mux, deps)

	body := `{"generationConfig":{"responseModalities":["AUDIO"]}}`
	req := httptest.NewRequest("POST", "/v1beta/models/gemini-2.5-flash-preview-tts:generateContent", bytes.NewReader([]byte(body)))
	req.RemoteAddr = "127.0.0.1:12345"
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (after rotation); body=%s", rw.Code, rw.Body.String())
	}
	if rw.Body.String() != "OK-BYTES" {
		t.Errorf("body = %q, want OK-BYTES", rw.Body.String())
	}
	if callCount < 2 {
		t.Errorf("upstream called %d time(s), want >=2 (rotation)", callCount)
	}
}

func TestV1BetaModelsPath_BadPath_400(t *testing.T) {
	mux := newV1BetaMux(t)
	req := httptest.NewRequest("POST", "/v1beta/models/gemini-2.0-flash", bytes.NewReader([]byte(`{}`)))
	req.RemoteAddr = "127.0.0.1:12345"
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for path without action", rw.Code)
	}
}

// v1BetaFixedChat is a ChatHandler that writes a fixed response body to the
// http.ResponseWriter (bypassing the sse.Writer) and returns the given
// Streamed flag. It lets the v1beta response converters be exercised end-to-end
// against a known OpenAI JSON or SSE payload without a live upstream.
type v1BetaFixedChat struct {
	body     string
	streamed bool
}

func (s *v1BetaFixedChat) Handle(ctx context.Context, req ChatRequest, w http.ResponseWriter, sse *Writer) (ChatResult, error) {
	if s.streamed {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, s.body)
	return ChatResult{StatusCode: http.StatusOK, Streamed: s.streamed}, nil
}

func TestV1BetaModelsPath_NonStreaming_DispatchesChat(t *testing.T) {
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	mustCreateConnection(t, db, "gemini", `{"apiKey":"gem-key","providerSpecificData":{"connectionProxyEnabled":false}}`)
	chat := &v1BetaFixedChat{
		streamed: false,
		body:     `{"choices":[{"message":{"role":"assistant","content":"hello back"},"finish_reason":"stop"}],"model":"gemini-2.0-flash","usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
	}
	deps := newV1BetaDeps(t, db, chat)
	mux := http.NewServeMux()
	RegisterV1(mux, deps)

	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	req := httptest.NewRequest("POST", "/v1beta/models/gemini/gemini-2.0-flash:generateContent", bytes.NewReader([]byte(body)))
	req.RemoteAddr = "127.0.0.1:12345"
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	out := rw.Body.String()
	if !strings.Contains(out, "\"finishReason\":\"STOP\"") {
		t.Errorf("missing finishReason STOP: %s", out)
	}
	if !strings.Contains(out, "\"modelVersion\":\"gemini-2.0-flash\"") {
		t.Errorf("missing modelVersion: %s", out)
	}
	if !strings.Contains(out, "\"text\":\"hello back\"") {
		t.Errorf("missing content text: %s", out)
	}
}

func TestV1BetaModelsPath_Streaming_DispatchesChat(t *testing.T) {
	db := mustOpenDB(t)
	t.Cleanup(func() { db.Close() })
	mustCreateConnection(t, db, "gemini", `{"apiKey":"gem-key","providerSpecificData":{"connectionProxyEnabled":false}}`)
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3},\"model\":\"gemini-2.0-flash\"}\n\n" +
		"data: [DONE]\n\n"
	chat := &v1BetaFixedChat{streamed: true, body: sse}
	deps := newV1BetaDeps(t, db, chat)
	mux := http.NewServeMux()
	RegisterV1(mux, deps)

	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	req := httptest.NewRequest("POST", "/v1beta/models/gemini/gemini-2.0-flash:streamGenerateContent", bytes.NewReader([]byte(body)))
	req.RemoteAddr = "127.0.0.1:12345"
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	out := rw.Body.String()
	if !strings.Contains(out, "\"text\":\"Hi\"") {
		t.Errorf("missing streamed content: %s", out)
	}
	if !strings.Contains(out, "\"finishReason\":\"STOP\"") {
		t.Errorf("missing finishReason: %s", out)
	}
	if strings.Contains(out, "[DONE]") {
		t.Errorf("[DONE] leaked: %s", out)
	}
	if !strings.Contains(rw.Header().Get("Content-Type"), "text/event-stream") {
		t.Errorf("content-type = %q", rw.Header().Get("Content-Type"))
	}
}