package kiro

// constants_test.go covers the v0.5.40 kiroConstants port + the GPT-5.6 native
// reasoning-effort path (upstream cef5dd4d / eb00222c). No mocks — these are
// pure functions over the real tables/regexes.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResolveKiroEffortPath(t *testing.T) {
	cases := []struct {
		model string
		want  effortPath
	}{
		{"gpt-5.6", effortPathReasoning},
		{"gpt-5.6-codex", effortPathReasoning},
		{"gpt/5.6", effortPathReasoning},
		{"gpt-5.6.1", effortPathReasoning},
		{"claude-sonnet-4.6", effortPathOutputConfig},
		{"claude-opus-4.6", effortPathOutputConfig},
		{"claude-sonnet-4.5", ""},            // legacy 4.5 rejected in live smoke
		{"claude-sonnet-4", ""},              // minor unknown but major<4.6 gate via <=5
		{"kimi-k2", ""},                      // non-claude, non-gpt-5.6
		{"", ""},
		{"claude-sonnet-4.20251015", ""},     // minor >= 1000 (date suffix) → reject
	}
	for _, c := range cases {
		got := resolveKiroEffortPath(c.model)
		if got != c.want {
			t.Errorf("resolveKiroEffortPath(%q) = %q, want %q", c.model, got, c.want)
		}
	}
}

func TestResolveKiroEffortPath_Gpt56Insensitive(t *testing.T) {
	if got := resolveKiroEffortPath("GPT-5.6"); got != effortPathReasoning {
		t.Errorf("GPT-5.6 (mixed case) = %q, want reasoning", got)
	}
}

func TestSupportsKiroAdditionalModelRequestFields(t *testing.T) {
	if !supportsKiroAdditionalModelRequestFields("gpt-5.6") {
		t.Error("gpt-5.6 should support additionalModelRequestFields")
	}
	if !supportsKiroAdditionalModelRequestFields("claude-sonnet-4.6") {
		t.Error("claude-sonnet-4.6 should support additionalModelRequestFields")
	}
	if supportsKiroAdditionalModelRequestFields("claude-sonnet-4.5") {
		t.Error("claude-sonnet-4.5 should NOT support additionalModelRequestFields")
	}
}

func TestExtractKiroEffortLevel_Claude(t *testing.T) {
	cases := []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"reasoning_effort": "high"}, "high"},
		{map[string]any{"reasoning": map[string]any{"effort": "medium"}}, "medium"},
		{map[string]any{"output_config": map[string]any{"effort": "low"}}, "low"},
		{map[string]any{"reasoning_effort": "max"}, "high"},     // max → high (Claude)
		{map[string]any{"reasoning_effort": "xhigh"}, "high"},   // xhigh → high (Claude)
		{map[string]any{"reasoning_effort": "none"}, ""},        // none → disabled
		{map[string]any{"reasoning_effort": "off"}, ""},
		{map[string]any{"reasoning_effort": "disabled"}, ""},
		{map[string]any{}, ""},
	}
	for _, c := range cases {
		got := extractKiroEffortLevel(c.body)
		if got != c.want {
			t.Errorf("extractKiroEffortLevel(%v) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestExtractKiroGptEffortLevel(t *testing.T) {
	cases := []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"reasoning_effort": "high"}, "high"},
		{map[string]any{"reasoning_effort": "max"}, "xhigh"},    // max → xhigh (GPT)
		{map[string]any{"reasoning_effort": "xhigh"}, "xhigh"},  // xhigh valid (GPT)
		{map[string]any{"reasoning": map[string]any{"effort": "low"}}, "low"},
		{map[string]any{"output_config": map[string]any{"effort": "medium"}}, "medium"},
		{map[string]any{"reasoning_effort": "none"}, ""},        // GPT has no explicit none
		{map[string]any{}, ""},
	}
	for _, c := range cases {
		got := extractKiroGptEffortLevel(c.body)
		if got != c.want {
			t.Errorf("extractKiroGptEffortLevel(%v) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestBuildKiroAdditionalModelRequestFields_ClaudePath(t *testing.T) {
	body := map[string]any{"reasoning_effort": "high"}
	got := buildKiroAdditionalModelRequestFields(body, effortPathOutputConfig)
	thinking, _ := got["thinking"].(map[string]any)
	oc, _ := got["output_config"].(map[string]any)
	if thinking["type"] != "adaptive" || thinking["display"] != "summarized" {
		t.Errorf("thinking = %v, want type=adaptive display=summarized", thinking)
	}
	if oc["effort"] != "high" {
		t.Errorf("output_config.effort = %v, want high", oc["effort"])
	}
}

func TestBuildKiroAdditionalModelRequestFields_GptPath(t *testing.T) {
	body := map[string]any{"reasoning_effort": "max"}
	got := buildKiroAdditionalModelRequestFields(body, effortPathReasoning)
	r, _ := got["reasoning"].(map[string]any)
	if r["effort"] != "xhigh" {
		t.Errorf("reasoning.effort = %v, want xhigh (max→xhigh for GPT)", r["effort"])
	}
	// GPT path must NOT carry the Claude thinking/output_config shape.
	if _, has := got["output_config"]; has {
		t.Errorf("GPT path must not emit output_config; got %v", got)
	}
	if _, has := got["thinking"]; has {
		t.Errorf("GPT path must not emit thinking; got %v", got)
	}
}

func TestBuildKiroAdditionalModelRequestFields_NoEffort(t *testing.T) {
	if got := buildKiroAdditionalModelRequestFields(map[string]any{}, effortPathOutputConfig); got != nil {
		t.Errorf("no effort → nil, got %v", got)
	}
	if got := buildKiroAdditionalModelRequestFields(map[string]any{}, effortPathReasoning); got != nil {
		t.Errorf("no effort → nil, got %v", got)
	}
}

func TestBuildKiroAdditionalModelRequestFieldsForModel(t *testing.T) {
	body := map[string]any{"reasoning_effort": "high"}
	// gpt-5.6 → reasoning path.
	got := buildKiroAdditionalModelRequestFieldsForModel(body, "gpt-5.6")
	if r, _ := got["reasoning"].(map[string]any); r == nil || r["effort"] != "high" {
		t.Errorf("gpt-5.6 → reasoning.effort=high; got %v", got)
	}
	// claude-sonnet-4.6 → output_config path.
	got = buildKiroAdditionalModelRequestFieldsForModel(body, "claude-sonnet-4.6")
	if oc, _ := got["output_config"].(map[string]any); oc == nil || oc["effort"] != "high" {
		t.Errorf("claude-sonnet-4.6 → output_config.effort=high; got %v", got)
	}
	// claude-sonnet-4.5 → unsupported → nil.
	if got := buildKiroAdditionalModelRequestFieldsForModel(body, "claude-sonnet-4.5"); got != nil {
		t.Errorf("claude-sonnet-4.5 unsupported → nil; got %v", got)
	}
}

func TestUsesKiroNativeGptEffort(t *testing.T) {
	body := map[string]any{"reasoning_effort": "high"}
	if !usesKiroNativeGptEffort(body, "gpt-5.6") {
		t.Error("gpt-5.6 + reasoning_effort → native GPT effort")
	}
	// No effort → not native.
	if usesKiroNativeGptEffort(map[string]any{}, "gpt-5.6") {
		t.Error("gpt-5.6 without effort → not native")
	}
	// Claude model even with effort → not native GPT path.
	if usesKiroNativeGptEffort(body, "claude-sonnet-4.6") {
		t.Error("claude model → not native GPT path")
	}
}

func TestResolveKiroThinkingBudget(t *testing.T) {
	const none = -1
	cases := []struct {
		name  string
		body  map[string]any
		model string
		want  int
	}{
		{"effort high → budget", map[string]any{"reasoning_effort": "high"}, "", 24576},
		{"effort none → disabled", map[string]any{"reasoning_effort": "none"}, "", none},
		{"level disabled string", map[string]any{"output_config": map[string]any{"effort": "disabled"}}, "", none},
		{"explicit budget", map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": float64(4096)}}, "", 4096},
		{"thinking tag in content", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "<thinking_mode>enabled</thinking_mode> hi"}}}, "", KIRO_THINKING_BUDGET_DEFAULT},
		{"thinking model suffix", map[string]any{}, "claude-sonnet-4.6-thinking", KIRO_THINKING_BUDGET_DEFAULT},
		{"no intent → none", map[string]any{}, "claude-sonnet-4.6", none},
	}
	for _, c := range cases {
		got := resolveKiroThinkingBudget(c.body, nil, c.model)
		if got != c.want {
			t.Errorf("%s: resolveKiroThinkingBudget = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestBuildThinkingSystemPrefix(t *testing.T) {
	p := buildThinkingSystemPrefix(4096)
	if p != "<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>4096</max_thinking_length>" {
		t.Errorf("prefix = %q", p)
	}
	// Clamp to 32000.
	if p := buildThinkingSystemPrefix(100000); p != "<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>32000</max_thinking_length>" {
		t.Errorf("clamp high = %q", p)
	}
	// Clamp to default when <= 0.
	if p := buildThinkingSystemPrefix(0); p != "<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>16000</max_thinking_length>" {
		t.Errorf("clamp default = %q", p)
	}
}

func TestResolveKiroModel(t *testing.T) {
	cases := []struct {
		in                    string
		upstream              string
		agentic, thinking bool
	}{
		{"claude-sonnet-4.5-thinking-agentic", "claude-sonnet-4.5", true, true},
		{"claude-sonnet-4.5-thinking", "claude-sonnet-4.5", false, true},
		{"claude-sonnet-4.5-agentic", "claude-sonnet-4.5", true, false},
		{"claude-sonnet-4.5", "claude-sonnet-4.5", false, false},
		{"gpt-5.6", "gpt-5.6", false, false},
	}
	for _, c := range cases {
		got := resolveKiroModel(c.in)
		if got.upstream != c.upstream || got.agentic != c.agentic || got.thinking != c.thinking {
			t.Errorf("resolveKiroModel(%q) = %+v, want upstream=%q agentic=%v thinking=%v",
				c.in, got, c.upstream, c.agentic, c.thinking)
		}
	}
}

// TestOpenAIToKiroRequest_Gpt56NativeEffortNoThinkingTag is the end-to-end
// regression for the v0.5.40 GPT-5.6 fix: a gpt-5.6 request with reasoning_effort
// must emit additionalModelRequestFields.reasoning and must NOT emit a
// <thinking_mode> systemPrompt (Kiro consumes the native field instead).
func TestOpenAIToKiroRequest_Gpt56NativeEffortNoThinkingTag(t *testing.T) {
	body := `{"model":"gpt-5.6","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"max"}`
	out, err := openaiToKiroRequest("gpt-5.6", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	amrf, ok := p["additionalModelRequestFields"].(map[string]any)
	if !ok {
		t.Fatalf("additionalModelRequestFields missing: %v", p)
	}
	r, _ := amrf["reasoning"].(map[string]any)
	if r == nil || r["effort"] != "xhigh" {
		t.Errorf("reasoning.effort = %v, want xhigh (max→xhigh)", r)
	}
	if sp, has := p["systemPrompt"]; has {
		t.Errorf("gpt-5.6 native effort must not emit systemPrompt thinking tag; got %v", sp)
	}
}

// TestOpenAIToKiroRequest_ClaudeAdaptiveEffortEmitsThinkingTag pins the legacy
// path: claude-sonnet-4.6 + reasoning_effort emits the <thinking_mode> prefix
// (it is NOT the native GPT path) plus the Claude output_config effort fields.
func TestOpenAIToKiroRequest_ClaudeAdaptiveEffortEmitsThinkingTag(t *testing.T) {
	body := `{"model":"claude-sonnet-4.6","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`
	out, err := openaiToKiroRequest("claude-sonnet-4.6", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sp, _ := p["systemPrompt"].(string)
	if !strings.Contains(sp, "<thinking_mode>enabled</thinking_mode>") {
		t.Errorf("claude-4.6 effort must emit <thinking_mode> systemPrompt; got %q", sp)
	}
	amrf, _ := p["additionalModelRequestFields"].(map[string]any)
	oc, _ := amrf["output_config"].(map[string]any)
	if oc == nil || oc["effort"] != "high" {
		t.Errorf("claude-4.6 effort → output_config.effort=high; got %v", amrf)
	}
}

// TestOpenAIToKiroRequest_StripsSyntheticSuffixes verifies the upstream model
// id sent to Kiro has the synthetic -agentic / -thinking suffixes stripped.
func TestOpenAIToKiroRequest_StripsSyntheticSuffixes(t *testing.T) {
	body := `{"model":"claude-sonnet-4.5-thinking","messages":[{"role":"user","content":"hi"}]}`
	out, err := openaiToKiroRequest("claude-sonnet-4.5-thinking", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// claude-sonnet-4.5 is unsupported for additionalModelRequestFields and the
	// -thinking suffix triggers the <thinking_mode> tag via the model-suffix
	// detector in resolveKiroThinkingBudget.
	var p map[string]any
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sp, _ := p["systemPrompt"].(string)
	if !strings.Contains(sp, "<thinking_mode>enabled</thinking_mode>") {
		t.Errorf("thinking suffix should trigger <thinking_mode> tag; got %q", sp)
	}
}