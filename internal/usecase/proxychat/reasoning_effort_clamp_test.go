package proxychat

import (
	"encoding/json"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

// TestClampReasoningEffortForOpenAINative ports the upstream regression test
// for 28894096 (thinkingUnified.applyFormat case "openai"): OpenAI's
// reasoning_effort enum caps at "xhigh" and rejects "max" with HTTP 400, so
// Claude Code's "max" must be clamped to "xhigh" before the body reaches an
// OpenAI-native upstream. Other levels pass through unchanged, and non-OpenAI
// targets are left alone.
func TestClampReasoningEffortForOpenAINative(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		target   format.Format
		wantEff  string // expected reasoning_effort after clamp; "" = unchanged/no field
		changed  bool   // whether reasoning_effort was set/changed at all
	}{
		{
			name:    `direct reasoning_effort "max" clamped to "xhigh"`,
			body:    `{"model":"gpt-5","reasoning_effort":"max"}`,
			target:  format.Openai,
			wantEff: "xhigh",
			changed: true,
		},
		{
			name:    `output_config effort max stays (Claude shape, not reasoning_effort)`,
			body:    `{"model":"gpt-5","output_config":{"effort":"max"}}`,
			target:  format.Openai,
			wantEff: "",
			changed: false,
		},
		{
			name:    `"xhigh" passes through unchanged`,
			body:    `{"model":"gpt-5","reasoning_effort":"xhigh"}`,
			target:  format.Openai,
			wantEff: "xhigh",
			changed: true,
		},
		{
			name:    `"high" passes through unchanged`,
			body:    `{"model":"gpt-5","reasoning_effort":"high"}`,
			target:  format.Openai,
			wantEff: "high",
			changed: true,
		},
		{
			name:    "openai-responses target also clamps (FORMAT_TO_NATIVE maps to openai)",
			body:    `{"model":"o3","reasoning_effort":"max"}`,
			target:  format.OpenaiResponses,
			wantEff: "xhigh",
			changed: true,
		},
		{
			name:    "codex target also clamps (FORMAT_TO_NATIVE maps to openai)",
			body:    `{"model":"gpt-5","reasoning_effort":"max"}`,
			target:  format.Codex,
			wantEff: "xhigh",
			changed: true,
		},
		{
			name:    "claude target NOT clamped (Claude accepts max via output_config)",
			body:    `{"model":"claude-opus-4-8","reasoning_effort":"max"}`,
			target:  format.Claude,
			wantEff: "max",
			changed: true,
		},
		{
			name:    "no reasoning_effort field → no-op",
			body:    `{"model":"gpt-5"}`,
			target:  format.Openai,
			wantEff: "",
			changed: false,
		},
		{
			name:    `case-insensitive "MAX" clamped`,
			body:    `{"model":"gpt-5","reasoning_effort":"MAX"}`,
			target:  format.Openai,
			wantEff: "xhigh",
			changed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			clampReasoningEffortForOpenAINative(body, tc.target)

			got, has := body["reasoning_effort"].(string)
			if tc.changed {
				if !has {
					t.Fatalf("expected reasoning_effort to be present, got absent")
				}
				if got != tc.wantEff {
					t.Fatalf("reasoning_effort = %q, want %q", got, tc.wantEff)
				}
			} else {
				if has {
					t.Fatalf("expected reasoning_effort unchanged/absent, got %q", got)
				}
			}
		})
	}
}