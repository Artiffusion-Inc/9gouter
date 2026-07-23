package proxychat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/config"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// thinking_suffix_test.go ports the b10b8070 regression: the UI appends a
// "(level)" suffix to copied model names (e.g. "claude-opus-4-8(high)"); the
// server must strip it from the upstream body.model so providers do not reject
// the request for an unknown model id. The strip happens at proxychat.go:186
// (the chatCore.js:151 analogue) before the body reaches the executor.

// capturingExecutor records the upstream body it is asked to send, then returns
// a fixed non-streaming JSON response so Handle completes.
type capturingExecutor struct {
	resp        *http.Response
	gotBody     json.RawMessage
	gotModel    string
	transformed bool
}

func (s *capturingExecutor) BuildURL(model string, stream bool, urlIndex int, creds provider.Credentials) string {
	return "http://localhost"
}

func (s *capturingExecutor) BuildHeaders(creds provider.Credentials, stream bool) http.Header {
	return http.Header{}
}

func (s *capturingExecutor) TransformRequest(model string, body json.RawMessage, stream bool, creds provider.Credentials) (json.RawMessage, error) {
	s.transformed = true
	return body, nil
}

func (s *capturingExecutor) Execute(ctx context.Context, req provider.ExecRequest) (provider.Resp, error) {
	s.gotBody = req.Body
	s.gotModel = req.Model
	return provider.Resp{Response: s.resp}, nil
}

type capturingProvider struct {
	id   string
	exec provider.Executor
}

func (s *capturingProvider) ID() string                  { return s.id }
func (s *capturingProvider) Executor() provider.Executor { return s.exec }

// TestHandle_StripsThinkingSuffixFromUpstreamModel verifies the real chat path
// strips a "(level)" suffix from the upstream body.model.
func TestHandle_StripsThinkingSuffixFromUpstreamModel(t *testing.T) {
	jsonBody := `{"id":"cmpl-x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	exec := &capturingExecutor{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader([]byte(jsonBody))),
		},
	}

	h := New(Dependencies{
		Registry:   func(id string) (DomainProvider, error) { return &capturingProvider{id: "openai", exec: exec}, nil },
		UsageRepo:  &inMemoryUsageRepo{},
		StreamPipe: fakeStreamPiper{},
		JSONToSSE:  fakeJSONToSSE{},
		Config:     config.Config{StreamStallTimeout: config.DurationMs(180 * time.Second), StreamStallTimeoutReasoning: config.DurationMs(600 * time.Second), StreamReadinessMaxTimeout: config.DurationMs(900 * time.Second)},
	})

	req := Request{
		Ctx:        context.Background(),
		Endpoint:   "/v1/chat/completions",
		Body:       json.RawMessage(`{"model":"claude-opus-4-8(high)","messages":[{"role":"user","content":"hi"}]}`),
		ProviderID: "openai",
		// req.Model carries the UI's forced-level suffix.
		Model:          "claude-opus-4-8(high)",
		Stream:         false,
		ResponseWriter: nil,
	}

	if _, err := h.Handle(context.Background(), req); err != nil {
		t.Fatalf("Handle error: %v", err)
	}

	// The upstream body must carry the clean model id, not the suffixed one.
	var upstream map[string]any
	if err := json.Unmarshal(exec.gotBody, &upstream); err != nil {
		t.Fatalf("unmarshal captured upstream body: %v (body=%s)", err, string(exec.gotBody))
	}
	gotModel, _ := upstream["model"].(string)
	if gotModel != "claude-opus-4-8" {
		t.Fatalf("upstream body.model = %q, want %q (suffix must be stripped)", gotModel, "claude-opus-4-8")
	}
}

// TestHandle_NoSuffixLeavesModelUnchanged verifies a bare model name is not
// mangled when no "(level)" suffix is present.
func TestHandle_NoSuffixLeavesModelUnchanged(t *testing.T) {
	jsonBody := `{"id":"cmpl-x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	exec := &capturingExecutor{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader([]byte(jsonBody))),
		},
	}

	h := New(Dependencies{
		Registry:   func(id string) (DomainProvider, error) { return &capturingProvider{id: "openai", exec: exec}, nil },
		UsageRepo:  &inMemoryUsageRepo{},
		StreamPipe: fakeStreamPiper{},
		JSONToSSE:  fakeJSONToSSE{},
		Config:     config.Config{StreamStallTimeout: config.DurationMs(180 * time.Second), StreamStallTimeoutReasoning: config.DurationMs(600 * time.Second), StreamReadinessMaxTimeout: config.DurationMs(900 * time.Second)},
	})

	req := Request{
		Ctx:        context.Background(),
		Endpoint:   "/v1/chat/completions",
		Body:       json.RawMessage(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`),
		ProviderID: "openai",
		Model:      "gpt-4",
		Stream:     false,
	}

	if _, err := h.Handle(context.Background(), req); err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	var upstream map[string]any
	if err := json.Unmarshal(exec.gotBody, &upstream); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	gotModel, _ := upstream["model"].(string)
	if gotModel != "gpt-4" {
		t.Fatalf("upstream body.model = %q, want %q", gotModel, "gpt-4")
	}
}

// TestHandle_BlackboxUpstreamModelRemap ports the 940a35e0 blackbox catalog
// overhaul: a catalog entry's upstreamModelId is remapped onto the upstream
// body.model before the call (blackbox "claude-opus-4.8" → "blackboxai/
// anthropic/claude-opus-4.8"), and a trailing "(level)" suffix survives the
// remap so the UI's forced level reaches the upstream.
func TestHandle_BlackboxUpstreamModelRemap(t *testing.T) {
	jsonBody := `{"id":"cmpl-x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	exec := &capturingExecutor{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader([]byte(jsonBody))),
		},
	}

	h := New(Dependencies{
		Registry:   func(id string) (DomainProvider, error) { return &capturingProvider{id: "blackbox", exec: exec}, nil },
		UsageRepo:  &inMemoryUsageRepo{},
		StreamPipe: fakeStreamPiper{},
		JSONToSSE:  fakeJSONToSSE{},
		Config:     config.Config{StreamStallTimeout: config.DurationMs(180 * time.Second), StreamStallTimeoutReasoning: config.DurationMs(600 * time.Second), StreamReadinessMaxTimeout: config.DurationMs(900 * time.Second)},
	})

	cases := []struct {
		name, model, want string
	}{
		{"bare", "claude-opus-4.8", "blackboxai/anthropic/claude-opus-4.8"},
		{"with-suffix", "claude-opus-4.8(high)", "blackboxai/anthropic/claude-opus-4.8(high)"},
		{"gpt-bare", "gpt-5.4", "blackboxai/openai/gpt-5.4"},
		{"unknown-noop", "no-such-model", "no-such-model"},
	}
	for _, c := range cases {
		exec.gotBody = nil
		req := Request{
			Ctx:        context.Background(),
			Endpoint:   "/v1/chat/completions",
			Body:       json.RawMessage(`{"model":"` + c.model + `","messages":[{"role":"user","content":"hi"}]}`),
			ProviderID: "blackbox",
			Model:      c.model,
			Stream:     false,
		}
		if _, err := h.Handle(context.Background(), req); err != nil {
			t.Fatalf("%s: Handle error: %v", c.name, err)
		}
		var upstream map[string]any
		if err := json.Unmarshal(exec.gotBody, &upstream); err != nil {
			t.Fatalf("%s: unmarshal: %v (body=%s)", c.name, err, string(exec.gotBody))
		}
		gotModel, _ := upstream["model"].(string)
		if gotModel != c.want {
			t.Errorf("%s: upstream body.model = %q, want %q", c.name, gotModel, c.want)
		}
	}
}
