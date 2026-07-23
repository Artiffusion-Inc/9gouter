package kimchiexec

import (
	"encoding/json"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	defexec "github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/default"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// TestToKimiReasoningEffort ports the upstream 8c068a1f regression cases for
// thinkingUnified.toKimiReasoningEffort: SGLang backends only accept
// low/medium/high/max, so auto→high, minimal→low, xhigh→max, and the four
// native levels pass through; anything else is rejected (caller drops it).
func TestToKimiReasoningEffort(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"auto", "high", true},
		{"minimal", "low", true},
		{"xhigh", "max", true},
		{"low", "low", true},
		{"medium", "medium", true},
		{"high", "high", true},
		{"max", "max", true},
		// case-insensitive
		{"AUTO", "high", true},
		{"XHigh", "max", true},
		// outside the whitelist → drop
		{"garbage", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := toKimiReasoningEffort(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("toKimiReasoningEffort(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestTransformRequestKimiReasoningEffortNormalize verifies the kimchi executor
// normalizes a non-Anthropic model's reasoning_effort to the SGLang enum on the
// request path (upstream 8c068a1f), while Anthropic-backed models keep the
// existing drop behavior.
func TestTransformRequestKimiReasoningEffortNormalize(t *testing.T) {
	e := &Executor{DefaultExecutor: defexec.New("kimchi", base.Config{})}

	t.Run("kimi model auto -> high", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"model":            "kimi-k2.5",
			"reasoning_effort": "auto",
			"messages":         []any{map[string]any{"role": "user", "content": "q"}},
		})
		out, err := e.TransformRequest("kimi-k2.5", body, true, provider.Credentials{})
		if err != nil {
			t.Fatalf("TransformRequest: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got, _ := m["reasoning_effort"].(string); got != "high" {
			t.Fatalf("reasoning_effort = %q, want \"high\"", got)
		}
	})

	t.Run("kimi model xhigh -> max", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"model":            "kimi-k2.5",
			"reasoning_effort": "xhigh",
			"messages":         []any{map[string]any{"role": "user", "content": "q"}},
		})
		out, _ := e.TransformRequest("kimi-k2.5", body, true, provider.Credentials{})
		var m map[string]any
		json.Unmarshal(out, &m)
		if got, _ := m["reasoning_effort"].(string); got != "max" {
			t.Fatalf("reasoning_effort = %q, want \"max\"", got)
		}
	})

	t.Run("kimi model minimal -> low", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"model":            "kimi-k2.5",
			"reasoning_effort": "minimal",
			"messages":         []any{map[string]any{"role": "user", "content": "q"}},
		})
		out, _ := e.TransformRequest("kimi-k2.5", body, true, provider.Credentials{})
		var m map[string]any
		json.Unmarshal(out, &m)
		if got, _ := m["reasoning_effort"].(string); got != "low" {
			t.Fatalf("reasoning_effort = %q, want \"low\"", got)
		}
	})

	t.Run("kimi model high passes through", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"model":            "kimi-k2.5",
			"reasoning_effort": "high",
			"messages":         []any{map[string]any{"role": "user", "content": "q"}},
		})
		out, _ := e.TransformRequest("kimi-k2.5", body, true, provider.Credentials{})
		var m map[string]any
		json.Unmarshal(out, &m)
		if got, _ := m["reasoning_effort"].(string); got != "high" {
			t.Fatalf("reasoning_effort = %q, want \"high\"", got)
		}
	})

	t.Run("kimi model invalid value dropped", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"model":            "kimi-k2.5",
			"reasoning_effort": "garbage",
			"messages":         []any{map[string]any{"role": "user", "content": "q"}},
		})
		out, _ := e.TransformRequest("kimi-k2.5", body, true, provider.Credentials{})
		var m map[string]any
		json.Unmarshal(out, &m)
		if _, has := m["reasoning_effort"]; has {
			t.Fatalf("reasoning_effort should be dropped for invalid value, got %v", m["reasoning_effort"])
		}
	})

	t.Run("anthropic-backed model drops reasoning_effort", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"model":            "claude-sonnet-4-6",
			"reasoning_effort": "auto",
			"messages":         []any{map[string]any{"role": "user", "content": "q"}},
		})
		out, _ := e.TransformRequest("claude-sonnet-4-6", body, true, provider.Credentials{})
		var m map[string]any
		json.Unmarshal(out, &m)
		if _, has := m["reasoning_effort"]; has {
			t.Fatalf("reasoning_effort should be dropped for anthropic-backed model, got %v", m["reasoning_effort"])
		}
	})
}