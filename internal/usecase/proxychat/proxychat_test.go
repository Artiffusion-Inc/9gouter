package proxychat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9router/internal/adapter/config"
	httpstream "github.com/Artiffusion-Inc/9router/internal/adapter/transport/http"
	"github.com/Artiffusion-Inc/9router/internal/domain/format"
	"github.com/Artiffusion-Inc/9router/internal/domain/provider"
	"github.com/Artiffusion-Inc/9router/internal/domain/usage"
)

// inMemoryUsageRepo is a minimal usage.Repo for tests.
type inMemoryUsageRepo struct {
	records []usage.UsageRecord
}

func (r *inMemoryUsageRepo) Save(ctx context.Context, rec usage.UsageRecord) error {
	r.records = append(r.records, rec)
	return nil
}

func (r *inMemoryUsageRepo) Query(ctx context.Context, q usage.Query) ([]usage.UsageRecord, error) {
	return r.records, nil
}

func (r *inMemoryUsageRepo) Aggregates(ctx context.Context, period string) (usage.Aggregates, error) {
	return usage.Aggregates{}, nil
}

// stubExecutor returns a fixed response.
type stubExecutor struct {
	resp *http.Response
	err  error
}

func (s *stubExecutor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	return "http://localhost"
}

func (s *stubExecutor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	return http.Header{}
}

func (s *stubExecutor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	return body, nil
}

func (s *stubExecutor) Execute(ctx context.Context, req provider.ExecRequest) (provider.Resp, error) {
	return provider.Resp{Response: s.resp}, s.err
}

type stubProvider struct {
	id   string
	exec provider.Executor
}

func (s *stubProvider) ID() string             { return s.id }
func (s *stubProvider) Executor() provider.Executor { return s.exec }

// fakeStreamPiper just copies upstream to the SSE writer without the real stall logic.
type fakeStreamPiper struct{}

func (fakeStreamPiper) Pipe(ctx context.Context, upstream io.Reader, w *httpstream.Writer, opts httpstream.PipeOpts) error {
	for {
		buf := make([]byte, 4096)
		n, err := upstream.Read(buf)
		if n > 0 {
			if err := w.WriteRaw(buf[:n]); err != nil {
				return err
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

type fakeJSONToSSE struct{}

func (fakeJSONToSSE) Synthesize(body []byte) (string, error) { return "", nil }

func makeSSEUpstream(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestHandle_StreamingSSE(t *testing.T) {
	repo := &inMemoryUsageRepo{}
	upstreamBody := "data: {\"id\":\"cmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	exec := &stubExecutor{resp: makeSSEUpstream(upstreamBody)}

	h := New(Dependencies{
		Registry:  func(id string) (DomainProvider, error) { return &stubProvider{id: "openai", exec: exec}, nil },
		UsageRepo: repo,
		StreamPipe: fakeStreamPiper{},
		JSONToSSE: fakeJSONToSSE{},
		Config:    config.Config{StreamStallTimeout: config.DurationMs(180 * time.Second), StreamStallTimeoutReasoning: config.DurationMs(600 * time.Second), StreamReadinessMaxTimeout: config.DurationMs(900 * time.Second)},
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:        context.Background(),
		Endpoint:   "/v1/chat/completions",
		Body:       json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"stream":true}`),
		ProviderID: "openai",
		Model:      "gpt-4",
		Stream:     true,
		ResponseWriter: rec,
	}

	res, err := h.Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if !res.Streamed {
		t.Fatalf("expected streamed result")
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "hi") {
		t.Fatalf("response missing content: %q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("response missing [DONE]: %q", body)
	}
	if len(repo.records) != 1 {
		t.Fatalf("expected 1 usage row, got %d", len(repo.records))
	}
	if repo.records[0].Status != "success" {
		t.Fatalf("expected success status, got %q", repo.records[0].Status)
	}
}

func TestHandle_UpstreamStall_ErrorSSEAndDoneAndUsage(t *testing.T) {
	repo := &inMemoryUsageRepo{}
	// Upstream that never writes data.
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(blockReader{}),
	}
	exec := &stubExecutor{resp: resp}

	h := New(Dependencies{
		Registry:  func(id string) (DomainProvider, error) { return &stubProvider{id: "openai", exec: exec}, nil },
		UsageRepo: repo,
		StreamPipe: pipeAdapter{}, // use real pipe for stall detection
		JSONToSSE: fakeJSONToSSE{},
		Config:    config.Config{StreamStallTimeout: config.DurationMs(50 * time.Millisecond), StreamStallTimeoutReasoning: config.DurationMs(100 * time.Millisecond), StreamReadinessMaxTimeout: config.DurationMs(900 * time.Second)},
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:        context.Background(),
		Endpoint:   "/v1/chat/completions",
		Body:       json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"stream":true}`),
		ProviderID: "openai",
		Model:      "gpt-4",
		Stream:     true,
		ResponseWriter: rec,
	}

	res, err := h.Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if !res.Streamed {
		t.Fatalf("expected streamed result")
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"code":"stream_stall_timeout"`) {
		t.Fatalf("response missing stall error SSE: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("response missing [DONE]: %q", body)
	}
	if len(repo.records) != 1 {
		t.Fatalf("expected 1 usage row, got %d", len(repo.records))
	}
	if repo.records[0].Status != "success" {
		t.Fatalf("expected success status for stalled stream, got %q", repo.records[0].Status)
	}
}

type blockReader struct{}

func (blockReader) Read([]byte) (int, error) {
	time.Sleep(time.Hour)
	return 0, nil
}

func TestHandle_JsonToSseSynthesis(t *testing.T) {
	repo := &inMemoryUsageRepo{}
	jsonBody := `{"id":"cmpl-x","object":"chat.completion","created":1,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(jsonBody))),
	}
	exec := &stubExecutor{resp: resp}

	h := New(Dependencies{
		Registry:  func(id string) (DomainProvider, error) { return &stubProvider{id: "openai", exec: exec}, nil },
		UsageRepo: repo,
		StreamPipe: fakeStreamPiper{},
		JSONToSSE: synthesizerFunc(func(body []byte) (string, error) {
			return "data: {\"id\":\"cmpl-x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", nil
		}),
		Config:    config.Config{StreamStallTimeout: config.DurationMs(180 * time.Second), StreamStallTimeoutReasoning: config.DurationMs(600 * time.Second), StreamReadinessMaxTimeout: config.DurationMs(900 * time.Second)},
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:        context.Background(),
		Endpoint:   "/v1/chat/completions",
		Body:       json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"stream":true}`),
		ProviderID: "openai",
		Model:      "gpt-4",
		Stream:     true,
		ResponseWriter: rec,
	}

	_, err := h.Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hello") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected synthesized SSE, got %q", body)
	}
	if len(repo.records) != 1 {
		t.Fatalf("expected 1 usage row, got %d", len(repo.records))
	}
}

func TestHandle_TokenSaverHeaderOff(t *testing.T) {
	repo := &inMemoryUsageRepo{}
	upstreamBody := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n"
	exec := &stubExecutor{resp: makeSSEUpstream(upstreamBody)}

	h := New(Dependencies{
		Registry:  func(id string) (DomainProvider, error) { return &stubProvider{id: "openai", exec: exec}, nil },
		UsageRepo: repo,
		StreamPipe: fakeStreamPiper{},
		JSONToSSE: fakeJSONToSSE{},
		Config:    config.Config{StreamStallTimeout: config.DurationMs(180 * time.Second), StreamStallTimeoutReasoning: config.DurationMs(600 * time.Second), StreamReadinessMaxTimeout: config.DurationMs(900 * time.Second)},
	})

	headers := http.Header{}
	headers.Set("x-9router-token-saver", "off")
	rec := httptest.NewRecorder()
	req := Request{
		Ctx:        context.Background(),
		Endpoint:   "/v1/chat/completions",
		Body:       json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"stream":true}`),
		ProviderID: "openai",
		Model:      "gpt-4",
		Stream:     true,
		Headers:    headers,
		ResponseWriter: rec,
	}

	_, err := h.Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	// Just verify no panic and usage saved.
	if len(repo.records) != 1 {
		t.Fatalf("expected 1 usage row, got %d", len(repo.records))
	}
}

func TestHandle_NonStreaming(t *testing.T) {
	repo := &inMemoryUsageRepo{}
	jsonBody := `{"id":"cmpl-x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(jsonBody))),
	}
	exec := &stubExecutor{resp: resp}

	h := New(Dependencies{
		Registry:  func(id string) (DomainProvider, error) { return &stubProvider{id: "openai", exec: exec}, nil },
		UsageRepo: repo,
		StreamPipe: fakeStreamPiper{},
		JSONToSSE: fakeJSONToSSE{},
		Config:    config.Config{StreamStallTimeout: config.DurationMs(180 * time.Second), StreamStallTimeoutReasoning: config.DurationMs(600 * time.Second), StreamReadinessMaxTimeout: config.DurationMs(900 * time.Second)},
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:        context.Background(),
		Endpoint:   "/v1/chat/completions",
		Body:       json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		ProviderID: "openai",
		Model:      "gpt-4",
		Stream:     false,
		ResponseWriter: rec,
	}

	res, err := h.Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if res.Streamed {
		t.Fatalf("expected non-streamed result")
	}
	if rec.Body.String() == "" {
		t.Fatalf("expected response body")
	}
	if repo.records[0].PromptTokens != 5 || repo.records[0].CompletionTokens != 2 {
		t.Fatalf("unexpected token counts: %+v", repo.records[0])
	}
}

func TestIsThinkingEnabled(t *testing.T) {
	if !isThinkingEnabled(json.RawMessage(`{"thinking":{"type":"enabled"}}`), nil, "") {
		t.Fatal("expected thinking enabled")
	}
	if !isThinkingEnabled(json.RawMessage(`{"reasoning_effort":"high"}`), nil, "") {
		t.Fatal("expected reasoning effort")
	}
	h := http.Header{}
	h.Set("Anthropic-Beta", "claude-thinking-1234")
	if !isThinkingEnabled(nil, h, "") {
		t.Fatal("expected Anthropic-Beta thinking")
	}
	if !isThinkingEnabled(nil, nil, "o3-reason") {
		t.Fatal("expected -reason model")
	}
	if isThinkingEnabled(json.RawMessage(`{}`), nil, "gpt-4") {
		t.Fatal("expected no reasoning")
	}
}

func TestDetectSourceFormat(t *testing.T) {
	if detectSourceFormat("/v1/responses", nil) != format.OpenaiResponses {
		t.Fatal("expected responses format")
	}
	if detectSourceFormat("/v1/messages", nil) != format.Claude {
		t.Fatal("expected claude format")
	}
	if detectSourceFormat("/v1/chat/completions", json.RawMessage(`{"input":[]}`)) != format.Openai {
		t.Fatal("expected openai format for chat with input array")
	}
}
