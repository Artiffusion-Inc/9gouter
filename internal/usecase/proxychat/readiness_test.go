package proxychat

import (
	"encoding/json"
	"testing"
	"time"
)

func bodyWithMessages(n int, extra map[string]any) json.RawMessage {
	msgs := make([]map[string]any, n)
	for i := 0; i < n; i++ {
		msgs[i] = map[string]any{"role": "user", "content": "hi"}
	}
	m := map[string]any{"messages": msgs}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return b
}

func bodyWithTools(n int) json.RawMessage {
	tools := make([]map[string]any, n)
	for i := 0; i < n; i++ {
		tools[i] = map[string]any{"type": "function", "function": map[string]any{"name": "f"}}
	}
	b, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user", "content": "x"}}, "tools": tools})
	return b
}

func TestReadiness_Disabled(t *testing.T) {
	r := ResolveStreamReadinessTimeout(0, 0, "openai", "gpt-4", bodyWithMessages(10, nil))
	if r.TimeoutMs != 0 || r.Reasons[0] != "disabled" {
		t.Fatalf("expected disabled, got %+v", r)
	}
}

func TestReadiness_BaseWhenNoBumps(t *testing.T) {
	base := 120 * time.Second
	r := ResolveStreamReadinessTimeout(base, 15*time.Minute, "openai", "gpt-4", bodyWithMessages(10, nil))
	if r.TimeoutMs != base {
		t.Fatalf("expected base %v, got %v", base, r.TimeoutMs)
	}
	if last := r.Reasons[len(r.Reasons)-1]; last != "base" {
		t.Fatalf("expected reasons to end with base, got %v", r.Reasons)
	}
}

func TestReadiness_LargeHistoryBump(t *testing.T) {
	base := 120 * time.Second
	r := ResolveStreamReadinessTimeout(base, 15*time.Minute, "openai", "gpt-4", bodyWithMessages(200, nil))
	want := base + largeHistoryBumpMs*time.Millisecond
	if r.TimeoutMs != want {
		t.Fatalf("expected %v, got %v", want, r.TimeoutMs)
	}
}

func TestReadiness_VeryLargeHistoryBump(t *testing.T) {
	base := 120 * time.Second
	r := ResolveStreamReadinessTimeout(base, 15*time.Minute, "openai", "gpt-4", bodyWithMessages(500, nil))
	want := base + veryLargeHistoryBumpMs*time.Millisecond
	if r.TimeoutMs != want {
		t.Fatalf("expected %v, got %v", want, r.TimeoutMs)
	}
}

func TestReadiness_ToolHeavyBump(t *testing.T) {
	base := 120 * time.Second
	r := ResolveStreamReadinessTimeout(base, 15*time.Minute, "openai", "gpt-4", bodyWithTools(20))
	want := base + toolHeavyBumpMs*time.Millisecond
	if r.TimeoutMs != want {
		t.Fatalf("expected %v, got %v", want, r.TimeoutMs)
	}
}

func TestReadiness_LargePayloadBump(t *testing.T) {
	base := 120 * time.Second
	big := make([]byte, largeCharThreshold+1000)
	for i := range big {
		big[i] = 'a'
	}
	body := json.RawMessage(`{"messages":[{"role":"user","content":"` + string(big) + `"}]}`)
	r := ResolveStreamReadinessTimeout(base, 15*time.Minute, "openai", "gpt-4", body)
	want := base + largePayloadBumpMs*time.Millisecond
	if r.TimeoutMs != want {
		t.Fatalf("expected %v, got %v", want, r.TimeoutMs)
	}
}

func TestReadiness_VeryLargePayloadBump(t *testing.T) {
	base := 120 * time.Second
	big := make([]byte, veryLargeCharThreshold+1000)
	for i := range big {
		big[i] = 'a'
	}
	body := json.RawMessage(`{"messages":[{"role":"user","content":"` + string(big) + `"}]}`)
	r := ResolveStreamReadinessTimeout(base, 15*time.Minute, "openai", "gpt-4", body)
	want := base + veryLargePayloadBumpMs*time.Millisecond
	if r.TimeoutMs != want {
		t.Fatalf("expected %v, got %v", want, r.TimeoutMs)
	}
}

func TestReadiness_CodexGpt5xHighReasoning(t *testing.T) {
	base := 120 * time.Second
	body, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "x"}},
		"reasoning_effort": "high",
	})
	r := ResolveStreamReadinessTimeout(base, 15*time.Minute, "codex", "gpt-5.5-codex-high", body)
	want := base + codexGpt5xHighReasoningBump*time.Millisecond
	if r.TimeoutMs != want {
		t.Fatalf("expected %v, got %v", want, r.TimeoutMs)
	}
	if !hasReason(r.Reasons, "codex_gpt_5_5_high_reasoning") {
		t.Fatalf("missing codex reason, got %v", r.Reasons)
	}
}

func TestReadiness_CodexGpt5xLargeResponses(t *testing.T) {
	base := 120 * time.Second
	r := ResolveStreamReadinessTimeout(base, 15*time.Minute, "codex", "gpt-5.5-codex", bodyWithMessages(200, nil))
	want := base + codexGpt5xHighReasoningBump*time.Millisecond + largeHistoryBumpMs*time.Millisecond
	if r.TimeoutMs != want {
		t.Fatalf("expected %v, got %v", want, r.TimeoutMs)
	}
	if !hasReason(r.Reasons, "codex_gpt_5_5_large_responses") {
		t.Fatalf("missing codex large responses reason, got %v", r.Reasons)
	}
}

func TestReadiness_ClampToMax(t *testing.T) {
	base := 120 * time.Second
	max := 130 * time.Second
	body, _ := json.Marshal(map[string]any{
		"messages":        make([]map[string]any, 500),
		"tools":           make([]map[string]any, 20),
		"reasoning_effort": "high",
	})
	r := ResolveStreamReadinessTimeout(base, max, "codex", "gpt-5.5-codex-high", body)
	if r.TimeoutMs != max {
		t.Fatalf("expected clamp at %v, got %v", max, r.TimeoutMs)
	}
}

func TestReadiness_OnlyIncreases(t *testing.T) {
	base := 300 * time.Second
	body := bodyWithMessages(500, nil)
	r := ResolveStreamReadinessTimeout(base, 15*time.Minute, "openai", "gpt-4", body)
	if r.TimeoutMs < base {
		t.Fatalf("timeout decreased from %v to %v", base, r.TimeoutMs)
	}
}

func TestReadiness_BumpsStack(t *testing.T) {
	base := 120 * time.Second
	body, _ := json.Marshal(map[string]any{
		"messages": make([]map[string]any, 500),
		"tools":    make([]map[string]any, 20),
	})
	r := ResolveStreamReadinessTimeout(base, 15*time.Minute, "openai", "gpt-4o", body)
	want := base + veryLargeHistoryBumpMs*time.Millisecond + toolHeavyBumpMs*time.Millisecond
	if r.TimeoutMs != want {
		t.Fatalf("expected stacked %v, got %v", want, r.TimeoutMs)
	}
}

func hasReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}
