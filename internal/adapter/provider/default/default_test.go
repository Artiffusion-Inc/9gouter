package defaultexec

import (
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/provider/base"
)

// bodyWithAssistantToolCall builds a request body with one assistant message
// carrying a tool_call (the scope that Kimi's "toolCalls" rule injects into).
func bodyWithAssistantToolCall() map[string]any {
	return map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{
				"role":       "assistant",
				"content":    "ok",
				"tool_calls": []any{map[string]any{"id": "1", "type": "function"}},
			},
		},
	}
}

func assistantReasoning(body map[string]any) (string, bool) {
	msgs, ok := body["messages"].([]any)
	if !ok {
		return "", false
	}
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] == "assistant" {
			rc, _ := msg["reasoning_content"].(string)
			return rc, true
		}
	}
	return "", false
}

// TestApplyModelReasoningInject_KimiProviderKeyed verifies the #2690 fix: Kimi
// reasoning inject is keyed on the provider id, so a Kimi model whose upstream
// id does NOT start with "kimi-" (e.g. "k2.5") still gets the placeholder.
func TestApplyModelReasoningInject_KimiProviderKeyed(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("kimi", base.Config{})}
	body := bodyWithAssistantToolCall()
	out := e.applyModelReasoningInject("k2.5", body)
	rc, ok := assistantReasoning(out)
	if !ok {
		t.Fatal("no assistant message found")
	}
	if rc != " " {
		t.Errorf("reasoning_content = %q, want \" \" (placeholder injected for kimi provider + bare model)", rc)
	}
}

// TestApplyModelReasoningInject_KimiCodingProviderKeyed verifies the
// kimi-coding provider id is also keyed.
func TestApplyModelReasoningInject_KimiCodingProviderKeyed(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("kimi-coding", base.Config{})}
	body := bodyWithAssistantToolCall()
	out := e.applyModelReasoningInject("some-other-id", body)
	rc, _ := assistantReasoning(out)
	if rc != " " {
		t.Errorf("reasoning_content = %q, want \" \" for kimi-coding provider", rc)
	}
}

// TestApplyModelReasoningInject_KimiModelPrefixFallback verifies the legacy
// model-prefix path still works when the provider is not kimi (e.g. routed
// through a generic executor).
func TestApplyModelReasoningInject_KimiModelPrefixFallback(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("some-provider", base.Config{})}
	body := bodyWithAssistantToolCall()
	out := e.applyModelReasoningInject("kimi-k2.5", body)
	rc, _ := assistantReasoning(out)
	if rc != " " {
		t.Errorf("reasoning_content = %q, want \" \" (model-prefix fallback)", rc)
	}
}

// TestApplyModelReasoningInject_KimiToolCallsScope verifies the "toolCalls"
// scope does NOT inject when the assistant message has no tool_calls.
func TestApplyModelReasoningInject_KimiToolCallsScopeNoToolCalls(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("kimi", base.Config{})}
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "ok"},
		},
	}
	out := e.applyModelReasoningInject("kimi-k2.5", body)
	rc, _ := assistantReasoning(out)
	if rc != "" {
		t.Errorf("reasoning_content = %q, want empty (no tool_calls → no inject)", rc)
	}
}

// TestApplyModelReasoningInject_DeepSeekAllScope verifies deepseek model
// matching uses the "all" scope (injects even without tool_calls).
func TestApplyModelReasoningInject_DeepSeekAllScope(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("some-provider", base.Config{})}
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "ok"},
		},
	}
	out := e.applyModelReasoningInject("deepseek-chat", body)
	rc, _ := assistantReasoning(out)
	if rc != " " {
		t.Errorf("reasoning_content = %q, want \" \" (deepseek all-scope)", rc)
	}
}

// TestApplyModelReasoningInject_NoMatch verifies a non-kimi, non-deepseek
// provider+model leaves the body unchanged.
func TestApplyModelReasoningInject_NoMatch(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("openai", base.Config{})}
	body := bodyWithAssistantToolCall()
	out := e.applyModelReasoningInject("gpt-4o", body)
	rc, _ := assistantReasoning(out)
	if rc != "" {
		t.Errorf("reasoning_content = %q, want empty (no rule matches)", rc)
	}
}

// TestApplyModelReasoningInject_PreservesExistingReasoning verifies an
// assistant message that already carries reasoning_content is not overwritten.
func TestApplyModelReasoningInject_PreservesExistingReasoning(t *testing.T) {
	e := &DefaultExecutor{BaseExecutor: base.NewBaseExecutor("kimi", base.Config{})}
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "content": "ok", "tool_calls": []any{map[string]any{}}, "reasoning_content": "existing"},
		},
	}
	out := e.applyModelReasoningInject("k2.5", body)
	rc, _ := assistantReasoning(out)
	if rc != "existing" {
		t.Errorf("reasoning_content = %q, want \"existing\" (preserve)", rc)
	}
}