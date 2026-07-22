package proxychat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/sqlite"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator/shared"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/usage"
)

// realUsageRepo opens a fresh on-disk SQLite database, runs SyncSchema, and
// returns a real *repo.UsageRepo backed by it — no in-memory fakes, no mocks.
// This is the same setup the production binary uses (sqlite.Open + db.SyncSchema
// + repo.NewUsageRepo), so the test exercises the exact persistence path that
// recorded promptTokens=0 for translated non-stream responses.
func realUsageRepo(t *testing.T) *repo.UsageRepo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage-test.db")
	conn, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	if err := db.SyncSchema(conn); err != nil {
		t.Fatalf("SyncSchema: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return repo.NewUsageRepo(conn)
}

// configForTest returns a Dependencies config with the stall/readiness timeouts
// the Handler requires.
func configForTest() config.Config {
	return config.Config{
		StreamStallTimeout:          config.DurationMs(180 * time.Second),
		StreamStallTimeoutReasoning: config.DurationMs(600 * time.Second),
		StreamReadinessMaxTimeout:   config.DurationMs(900 * time.Second),
	}
}

// TestTokenCount_NumericTypes is the direct regression test for the usage=0
// bug. tokenCount previously matched ONLY float64 (the type json.Unmarshal
// produces). BuildUsage (shared) emits int values, so any non-stream response
// translated in-process from ollama/claude/gemini→openai produced a clientBody
// whose usage.prompt_tokens was an int — which the old tokenCount silently
// dropped to 0. This test pins that tokenCount now accepts every numeric type a
// usage field can carry: int (BuildUsage), float64 (raw json.Unmarshal), int64,
// and json.Number.
func TestTokenCount_NumericTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		val  any
		want int
	}{
		{"int (BuildUsage output)", 177, 177},
		{"float64 (raw json.Unmarshal)", float64(177), 177},
		{"int64", int64(177), 177},
		{"json.Number", json.Number("177"), 177},
		{"zero int", 0, 0},
		{"missing (nil)", nil, 0},
		{"non-numeric (string)", "177", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			body := map[string]any{
				"usage": map[string]any{"prompt_tokens": c.val},
			}
			if got := tokenCount(body, "prompt_tokens", "input_tokens"); got != c.want {
				t.Fatalf("tokenCount(prompt_tokens=%T %v) = %d, want %d", c.val, c.val, got, c.want)
			}
		})
	}
}

// TestNumericInt is the unit test for the numeric coercion helper that backs
// tokenCount. It asserts the full type matrix and that non-numeric values
// return ok=false (so callers fall through to the next usage key).
func TestNumericInt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		val    any
		want   int
		wantOk bool
	}{
		{float64(42.9), 42, true}, // truncates toward zero, like int()
		{int(42), 42, true},
		{int64(42), 42, true},
		{json.Number("42"), 42, true},
		{json.Number("not-a-number"), 0, false},
		{nil, 0, false},
		{"42", 0, false},
		{true, 0, false},
		{[]any{1}, 0, false},
	}
	for _, c := range cases {
		got, ok := numericInt(c.val)
		if got != c.want || ok != c.wantOk {
			t.Errorf("numericInt(%T %v) = (%d, %v), want (%d, %v)", c.val, c.val, got, ok, c.want, c.wantOk)
		}
	}
}

// TestTokenCount_FallsThroughToSecondKey verifies that when the first key is
// missing/zero, tokenCount tries the next key (e.g. input_tokens fallback for
// Claude-shaped usage that uses input_tokens instead of prompt_tokens).
func TestTokenCount_FallsThroughToSecondKey(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"usage": map[string]any{"input_tokens": int(99)}, // no prompt_tokens
	}
	if got := tokenCount(body, "prompt_tokens", "input_tokens"); got != 99 {
		t.Fatalf("tokenCount fallback = %d, want 99", got)
	}
}

// TestTokenCount_NoUsageField verifies a body without a usage object returns 0
// without panicking (guards the map type-assertion).
func TestTokenCount_NoUsageField(t *testing.T) {
	t.Parallel()
	if got := tokenCount(map[string]any{"choices": "x"}, "prompt_tokens"); got != 0 {
		t.Fatalf("tokenCount without usage = %d, want 0", got)
	}
}

// ollamaStubExecutor returns a fixed Ollama-shaped non-stream chat response
// (message.content + eval_count/prompt_eval_count). It is a test double for the
// upstream HTTP provider, NOT a mock of any repository — the usage repository
// under test is the real on-disk SQLite UsageRepo.
type ollamaStubExecutor struct{}

func (ollamaStubExecutor) BuildURL(string, bool, int, provider.Credentials) string { return "http://upstream" }
func (ollamaStubExecutor) BuildHeaders(provider.Credentials, bool) http.Header     { return http.Header{} }
func (ollamaStubExecutor) TransformRequest(string, json.RawMessage, bool, provider.Credentials) (json.RawMessage, error) {
	return json.RawMessage(`{"model":"minimax-m3","messages":[{"role":"user","content":"hi"}]}`), nil
}
func (ollamaStubExecutor) Execute(context.Context, provider.ExecRequest) (provider.Resp, error) {
	body := `{"model":"minimax-m3","created_at":"2026-07-22T06:13:17Z","message":{"role":"assistant","content":"Hi there!","thinking":"greeting"},"done":true,"done_reason":"stop","prompt_eval_count":177,"eval_count":42}`
	return provider.Resp{
		Response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		},
	}, nil
}

// TestHandle_NonStreamOllamaToOpenAI_RecordsRealTokens is the end-to-end
// regression test for the usage=0 bug. It drives the full non-stream Handle
// path with a real SQLite UsageRepo: ollama upstream → OpenAI client
// (targetFormat=Ollama, sourceFormat=Openai → translateNonStreamingResponse →
// ollamaBodyToOpenAI → shared.ToOpenAIUsage → BuildUsage which emits int
// token values). Before the fix, the resulting usageHistory row had
// promptTokens=0/completionTokens=0 because tokenCount matched only float64.
// After the fix, the row records 177/42.
func TestHandle_NonStreamOllamaToOpenAI_RecordsRealTokens(t *testing.T) {
	usageRepo := realUsageRepo(t)

	// Sanity: confirm the ollama→openai translation produces an int-typed
	// usage field — this is the exact condition that triggered the bug.
	var upstreamBody map[string]any
	_ = json.Unmarshal([]byte(`{"model":"m","message":{"content":"x"},"done":true,"prompt_eval_count":177,"eval_count":42}`), &upstreamBody)
	translated := ollamaBodyToOpenAI(upstreamBody)
	usageObj, _ := translated["usage"].(map[string]any)
	if _, isInt := usageObj["prompt_tokens"].(int); !isInt {
		t.Fatalf("precondition: BuildUsage prompt_tokens must be int, got %T", usageObj["prompt_tokens"])
	}
	if got := tokenCount(translated, "prompt_tokens", "input_tokens"); got != 177 {
		t.Fatalf("precondition: tokenCount on translated body = %d, want 177", got)
	}

	h := New(Dependencies{
		Registry:  func(id string) (DomainProvider, error) { return &stubProvider{id: "ollama", exec: ollamaStubExecutor{}}, nil },
		UsageRepo: usageRepo,
		StreamPipe: fakeStreamPiper{},
		JSONToSSE:  fakeJSONToSSE{},
		Config:     configForTest(),
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:            context.Background(),
		Endpoint:       "/v1/chat/completions",
		Body:           json.RawMessage(`{"model":"minimax-m3","messages":[{"role":"user","content":"hi"}],"stream":false}`),
		ProviderID:     "ollama",
		Model:          "minimax-m3",
		Stream:         false,
		APIKey:         "sk-test",
		ConnectionID:   "conn-test",
		ResponseWriter: rec,
	}

	res, err := h.Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	// The client response must carry the translated OpenAI usage with the
	// real token counts (proves translation produced int values the client
	// sees).
	var clientResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &clientResp); err != nil {
		t.Fatalf("unmarshal client response: %v (body=%s)", err, rec.Body.String())
	}
	if got := tokenCount(clientResp, "prompt_tokens", "input_tokens"); got != 177 {
		t.Fatalf("client usage prompt_tokens = %v, want 177", clientResp["usage"])
	}
	if got := tokenCount(clientResp, "completion_tokens", "output_tokens"); got != 42 {
		t.Fatalf("client usage completion_tokens = %v, want 42", clientResp["usage"])
	}

	// Read the row back from the REAL SQLite database and assert the persisted
	// token counts — this is the regression assertion. Before the fix both
	// were 0.
	rows, err := usageRepo.Query(context.Background(), usage.Query{Limit: 100})
	if err != nil {
		t.Fatalf("usage Query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.PromptTokens != 177 {
		t.Errorf("persisted promptTokens = %d, want 177 (regression: was 0 before tokenCount fix)", r.PromptTokens)
	}
	if r.CompletionTokens != 42 {
		t.Errorf("persisted completionTokens = %d, want 42 (regression: was 0 before tokenCount fix)", r.CompletionTokens)
	}
	if r.Status != "success" {
		t.Errorf("persisted status = %q, want success", r.Status)
	}
	if r.Provider != "ollama" {
		t.Errorf("persisted provider = %q, want ollama", r.Provider)
	}
}

// TestHandle_NonStreamOpenAIPassthrough_RecordsFloat64Tokens verifies the path
// that ALREADY worked (OpenAI→OpenAI passthrough, no in-process translation, so
// usage values stay float64 from json.Unmarshal) still records tokens — guards
// against the fix regressing the float64 path while fixing the int path.
func TestHandle_NonStreamOpenAIPassthrough_RecordsFloat64Tokens(t *testing.T) {
	usageRepo := realUsageRepo(t)
	exec := &openAIPassthroughExecutor{}

	h := New(Dependencies{
		Registry:  func(id string) (DomainProvider, error) { return &stubProvider{id: "openai", exec: exec}, nil },
		UsageRepo: usageRepo,
		StreamPipe: fakeStreamPiper{},
		JSONToSSE:  fakeJSONToSSE{},
		Config:     configForTest(),
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:            context.Background(),
		Endpoint:       "/v1/chat/completions",
		Body:           json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":false}`),
		ProviderID:     "openai",
		Model:          "gpt-4",
		Stream:         false,
		APIKey:         "sk-test",
		ConnectionID:   "conn-passthrough",
		ResponseWriter: rec,
	}
	if _, err := h.Handle(context.Background(), req); err != nil {
		t.Fatalf("Handle error: %v", err)
	}

	rows, err := usageRepo.Query(context.Background(), usage.Query{Limit: 100})
	if err != nil {
		t.Fatalf("usage Query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	if rows[0].PromptTokens != 100 {
		t.Errorf("passthrough promptTokens = %d, want 100", rows[0].PromptTokens)
	}
	if rows[0].CompletionTokens != 25 {
		t.Errorf("passthrough completionTokens = %d, want 25", rows[0].CompletionTokens)
	}
}

// openAIPassthroughExecutor returns an already-OpenAI-shaped non-stream
// response so sourceFormat==targetFormat (no translation) and usage values
// stay float64 from json.Unmarshal.
type openAIPassthroughExecutor struct{}

func (openAIPassthroughExecutor) BuildURL(string, bool, int, provider.Credentials) string { return "http://upstream" }
func (openAIPassthroughExecutor) BuildHeaders(provider.Credentials, bool) http.Header     { return http.Header{} }
func (openAIPassthroughExecutor) TransformRequest(string, json.RawMessage, bool, provider.Credentials) (json.RawMessage, error) {
	return json.RawMessage(`{"model":"gpt-4","messages":[]}`), nil
}
func (openAIPassthroughExecutor) Execute(context.Context, provider.ExecRequest) (provider.Resp, error) {
	body := `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":25,"total_tokens":125}}`
	return provider.Resp{
		Response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		},
	}, nil
}

// TestSharedBuildUsage_EmitsIntTypes is a cross-package pin: BuildUsage is the
// upstream cause of the int-typed usage values that triggered the bug. If a
// future refactor switches BuildUsage to emit float64, this test flips and
// flags that the tokenCount int-branch would become dead (but still correct).
// It documents the contract tokenCount relies on.
func TestSharedBuildUsage_EmitsIntTypes(t *testing.T) {
	t.Parallel()
	u := shared.BuildUsage(177, 42, 219, 0, 0, 0)
	if _, ok := u["prompt_tokens"].(int); !ok {
		t.Fatalf("BuildUsage prompt_tokens type = %T, want int (tokenCount relies on int branch)", u["prompt_tokens"])
	}
	if _, ok := u["completion_tokens"].(int); !ok {
		t.Fatalf("BuildUsage completion_tokens type = %T, want int", u["completion_tokens"])
	}
}

// TestTranslateNonStreamingResponse_OllamaPreservesUsage verifies the
// translation step itself keeps the usage object (and that it is int-typed) —
// an intermediate-level regression guard between the unit tokenCount test and
// the full Handle integration test.
func TestTranslateNonStreamingResponse_OllamaPreservesUsage(t *testing.T) {
	t.Parallel()
	var upstream map[string]any
	_ = json.Unmarshal([]byte(`{"model":"m","message":{"content":"x"},"done":true,"done_reason":"stop","prompt_eval_count":177,"eval_count":42}`), &upstream)
	out := translateNonStreamingResponse(upstream, format.Openai, format.Ollama)
	if out == nil {
		t.Fatal("nil translated body")
	}
	if got := tokenCount(out, "prompt_tokens", "input_tokens"); got != 177 {
		t.Fatalf("translated prompt_tokens = %d, want 177", got)
	}
	if got := tokenCount(out, "completion_tokens", "output_tokens"); got != 42 {
		t.Fatalf("translated completion_tokens = %d, want 42", got)
	}
}