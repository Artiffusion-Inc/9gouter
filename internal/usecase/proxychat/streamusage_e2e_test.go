package proxychat

// streamusage_e2e_test.go pins the end-to-end streaming-usage path (#162):
// a real SSE upstream piped through pipeAdapter (the production
// httpstream.Pipe) with OnFrame/OnFirstByte wired, recording real
// prompt/completion/cached/cost + streamMs/tps in the usage repo. This is the
// fix for the dashboard "Usage & Analytics not tracking cost/tokens/tps" —
// before, streaming requests recorded zeros.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/pricing"
)

// TestHandle_StreamingRecordsRealUsageAndTiming drives a real OpenAI-style
// SSE stream (delta chunk + terminal chunk carrying usage) through the
// production pipeAdapter and asserts the usage row holds the upstream-reported
// prompt/completion/cached, a non-zero USD cost, and a measured streamMs/tps.
func TestHandle_StreamingRecordsRealUsageAndTiming(t *testing.T) {
	repo := &inMemoryUsageRepo{}
	// Delta chunk then terminal chunk with full usage (cached + reasoning).
	upstreamBody := strings.Join([]string{
		`data: {"id":"cmpl-1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hello"}}]}`,
		`data: {"id":"cmpl-1","object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":45,"total_tokens":165,"cached_tokens":80,"completion_tokens_details":{"reasoning_tokens":12}}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	exec := &stubExecutor{resp: makeSSEUpstream(upstreamBody)}

	// gpt-4 is in the hard-coded MODEL_PRICING table
	// (pricing.go:65: Input 2.50, Output 10.00, Cached 1.25, Reasoning 15.00),
	// so NewResolver(nil) yields a deterministic non-zero cost for real tokens.
	resolver := pricing.NewResolver(nil)

	h := New(Dependencies{
		Registry:   func(id string) (DomainProvider, error) { return &stubProvider{id: "openai", exec: exec}, nil },
		UsageRepo:  repo,
		StreamPipe: pipeAdapter{}, // real httpstream.Pipe → OnFrame/OnFirstByte fire
		JSONToSSE:  fakeJSONToSSE{},
		Pricing:    resolver,
		Config:     config.Config{StreamStallTimeout: config.DurationMs(180 * time.Second), StreamStallTimeoutReasoning: config.DurationMs(600 * time.Second), StreamReadinessMaxTimeout: config.DurationMs(900 * time.Second)},
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:            context.Background(),
		Endpoint:       "/v1/chat/completions",
		Body:           json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"stream":true}`),
		ProviderID:     "openai",
		Model:          "gpt-4",
		Stream:         true,
		ResponseWriter: rec,
	}

	res, err := h.Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if !res.Streamed {
		t.Fatalf("expected streamed result")
	}
	if len(repo.records) != 1 {
		t.Fatalf("expected 1 usage row, got %d", len(repo.records))
	}
	r := repo.records[0]
	if r.PromptTokens != 120 {
		t.Errorf("PromptTokens=%d want 120", r.PromptTokens)
	}
	if r.CompletionTokens != 45 {
		t.Errorf("CompletionTokens=%d want 45", r.CompletionTokens)
	}
	if r.Cost <= 0 {
		t.Errorf("Cost=%v want >0 (pricing applied to real tokens)", r.Cost)
	}
	// tokens blob must carry cached + reasoning.
	var tok map[string]int
	if err := json.Unmarshal(r.Tokens, &tok); err != nil {
		t.Fatalf("unmarshal tokens blob: %v", err)
	}
	if tok["cached_tokens"] != 80 {
		t.Errorf("tokens.cached_tokens=%d want 80", tok["cached_tokens"])
	}
	if tok["reasoning_tokens"] != 12 {
		t.Errorf("tokens.reasoning_tokens=%d want 12", tok["reasoning_tokens"])
	}
	// streamMs + tps must be measured (non-nil, non-zero). The stream is
	// instantaneous, so values may be tiny but must be present.
	if r.StreamMs == nil || *r.StreamMs <= 0 {
		t.Errorf("StreamMs=%v want non-nil positive (OnFirstByte fired)", r.StreamMs)
	}
	if r.TPS == nil || *r.TPS <= 0 {
		t.Errorf("TPS=%v want non-nil positive", r.TPS)
	}
}

// TestHandle_StreamingNoUsageFallsBackToHeadroom: when the upstream stream
// carries NO usage frame, the collector yields nil tokens and prompt/completion
// stay 0 (no false invention). The usage row records zeros, matching the
// honest "no upstream usage reported" state.
func TestHandle_StreamingNoUsageFallsBackToZero(t *testing.T) {
	repo := &inMemoryUsageRepo{}
	upstreamBody := strings.Join([]string{
		`data: {"id":"cmpl-1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"id":"cmpl-1","object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	exec := &stubExecutor{resp: makeSSEUpstream(upstreamBody)}

	h := New(Dependencies{
		Registry:   func(id string) (DomainProvider, error) { return &stubProvider{id: "openai", exec: exec}, nil },
		UsageRepo:  repo,
		StreamPipe: pipeAdapter{},
		JSONToSSE:  fakeJSONToSSE{},
		Config:     config.Config{StreamStallTimeout: config.DurationMs(180 * time.Second), StreamStallTimeoutReasoning: config.DurationMs(600 * time.Second), StreamReadinessMaxTimeout: config.DurationMs(900 * time.Second)},
	})

	rec := httptest.NewRecorder()
	req := Request{
		Ctx:            context.Background(),
		Endpoint:       "/v1/chat/completions",
		Body:           json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"stream":true}`),
		ProviderID:     "openai",
		Model:          "gpt-4",
		Stream:         true,
		ResponseWriter: rec,
	}
	if _, err := h.Handle(context.Background(), req); err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if len(repo.records) != 1 {
		t.Fatalf("expected 1 usage row, got %d", len(repo.records))
	}
	r := repo.records[0]
	if r.PromptTokens != 0 || r.CompletionTokens != 0 {
		t.Errorf("tokens=%d/%d, want 0/0 (no upstream usage → no invention)", r.PromptTokens, r.CompletionTokens)
	}
	if r.Tokens != nil {
		t.Errorf("Tokens blob=%s, want nil when no usage collected", string(r.Tokens))
	}
}
