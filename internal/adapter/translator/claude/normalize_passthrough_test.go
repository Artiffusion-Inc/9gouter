package claude

import (
	"encoding/base64"
	"strings"
	"testing"
)

// --- signature validator ---

// TestIsValidClaudeSignatureDefault confirms the bundled default signature is a
// valid E-form signature (decoded[0] == 0x12). This is the invariant the
// placeholder re-insertion relies on.
func TestIsValidClaudeSignatureDefault(t *testing.T) {
	if !isValidClaudeSignature(defaultThinkingClaudeSignature) {
		t.Error("defaultThinkingClaudeSignature must be a valid Claude signature")
	}
	if !hasClaudeSignaturePrefix(defaultThinkingClaudeSignature) {
		t.Error("default signature must have E/R prefix")
	}
}

// TestIsValidClaudeSignatureForms covers E-form accept, R-form accept, foreign,
// empty, and cache-prefixed variants.
//
// Note on form construction: 'E'/'R' are NOT separate marker prefixes — they are
// the first base64 symbol, which arises automatically from decoded[0]: a payload
// starting with 0x12 base64-encodes to a string starting with 'E' (6 high bits
// 000100 == 4 == 'E'); a payload starting with 0x45 ('E') base64-encodes to a
// string starting with 'R' (6 high bits 010001 == 17 == 'R'). So a valid E-form
// is just base64(payload whose decoded[0]==0x12), and a valid R-form is
// base64(outer whose decoded[0]==0x45 and whose string form base64-decodes to a
// payload whose decoded[0]==0x12).
func TestIsValidClaudeSignatureForms(t *testing.T) {
	// Valid E-form: base64 of [0x12, 0x00] -> "EgA=" (starts with 'E').
	validE := mustB64(t, []byte{0x12, 0x00})
	if !isValidClaudeSignature(validE) {
		t.Errorf("valid E-form %q rejected", validE)
	}
	if !hasClaudeSignaturePrefix(validE) {
		t.Errorf("valid E-form %q lacks E/R prefix", validE)
	}

	// E-form whose decoded[0] != 0x12 → invalid (starts with a non-E symbol).
	invalidE := mustB64(t, []byte{0x00, 0x00}) // "AA==" -> prefix 'A'
	if isValidClaudeSignature(invalidE) {
		t.Errorf("E-form with wrong marker accepted: %q", invalidE)
	}

	// Valid R-form: inner payload [0x12,0x00] -> inner b64 "EgA=" (string
	// starting with 'E' == 0x45); outer = base64([]byte("EgA=")) starts with 'R'.
	innerB64 := mustB64(t, []byte{0x12, 0x00}) // "EgA="
	outer := mustB64(t, []byte(innerB64))
	if !isValidClaudeSignature(outer) {
		t.Errorf("valid R-form %q rejected", outer)
	}
	if !hasClaudeSignaturePrefix(outer) {
		t.Errorf("valid R-form %q lacks E/R prefix", outer)
	}

	// R-form with wrong inner marker: inner payload [0x00] -> "AA=" (starts
	// with 'A', not 'E'); outer = base64("AA=") -> outer[0] != 0x45 → invalid.
	badInnerB64 := mustB64(t, []byte{0x00}) // "AA="
	badOuter := mustB64(t, []byte(badInnerB64))
	if isValidClaudeSignature(badOuter) {
		t.Errorf("R-form with wrong inner marker accepted: %q", badOuter)
	}

	// Foreign prefix (Gemini-style, starts with 'C') → invalid.
	if isValidClaudeSignature("CiQBjz1rX/AlslZWMe5R") {
		t.Error("foreign 'C'-prefixed signature accepted")
	}

	// Empty / missing → invalid.
	if isValidClaudeSignature("") || isValidClaudeSignature("   ") {
		t.Error("empty signature accepted")
	}

	// Unknown prefix (first base64 symbol not 'E'/'R', e.g. 'Z') → invalid.
	if isValidClaudeSignature("Zabcdef") {
		t.Error("unknown-prefixed signature accepted")
	}

	// Cache-prefixed valid E-form still validates (prefix stripped at first '#').
	// stripCachePrefix drops everything up to and including the first '#', so the
	// remainder must itself be a valid E-form (one '#' only).
	prefixed := "cachehash#" + validE
	if !isValidClaudeSignature(prefixed) {
		t.Errorf("cache-prefixed valid E-form rejected: %q", prefixed)
	}

	// Garbage that is not base64 → invalid, no panic.
	if isValidClaudeSignature("E!!!not-base64!!!") {
		t.Error("non-base64 E-form accepted")
	}
}

func mustB64(t *testing.T, b []byte) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(b)
}

// --- normalize passthrough ---

// TestNormalizeHaikuAdaptiveDowngrade ports step 1: a Haiku model with
// thinking.type "adaptive" is downgraded to "enabled" with budget 10000.
func TestNormalizeHaikuAdaptiveDowngrade(t *testing.T) {
	body := map[string]any{
		"thinking": map[string]any{"type": "adaptive"},
		"messages": []any{},
	}
	NormalizeClaudePassthrough(body, "claude-haiku-4.5")
	th, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking = %#v", body["thinking"])
	}
	if th["type"] != "enabled" {
		t.Errorf("thinking.type = %v, want enabled", th["type"])
	}
	if bt, _ := th["budget_tokens"].(float64); bt != 10000 {
		t.Errorf("budget_tokens = %v, want 10000", th["budget_tokens"])
	}
}

// TestNormalizeNonHaikuKeepsAdaptive: a non-Haiku model keeps adaptive thinking.
func TestNormalizeNonHaikuKeepsAdaptive(t *testing.T) {
	body := map[string]any{
		"thinking": map[string]any{"type": "adaptive"},
		"messages": []any{},
	}
	NormalizeClaudePassthrough(body, "claude-opus-4.8")
	if th, _ := body["thinking"].(map[string]any); th["type"] != "adaptive" {
		t.Errorf("non-Haiku adaptive downgraded to %v", th["type"])
	}
}

// TestNormalizeHaikuStripsEffort ports step 2a: output_config.effort is removed
// for Haiku; other fields survive; empty output_config is dropped entirely.
func TestNormalizeHaikuStripsEffort(t *testing.T) {
	// effort + other field: only effort removed.
	body := map[string]any{
		"output_config": map[string]any{"effort": "high", "max_tokens": 1024},
		"messages":      []any{},
	}
	NormalizeClaudePassthrough(body, "claude-haiku-4.5")
	oc, ok := body["output_config"].(map[string]any)
	if !ok {
		t.Fatal("output_config should survive with remaining fields")
	}
	if _, has := oc["effort"]; has {
		t.Error("effort not stripped for Haiku")
	}
	if mt, _ := oc["max_tokens"]; mt != 1024 {
		t.Errorf("max_tokens lost: %v", mt)
	}

	// effort only: output_config dropped entirely.
	body2 := map[string]any{
		"output_config": map[string]any{"effort": "high"},
		"messages":      []any{},
	}
	NormalizeClaudePassthrough(body2, "claude-haiku-4.5")
	if _, has := body2["output_config"]; has {
		t.Error("empty output_config should be dropped for Haiku")
	}
}

// TestNormalizeHoistSystemMessages ports step 2b: mid-conversation role:system
// messages are hoisted into the top-level system array.
func TestNormalizeHoistSystemMessages(t *testing.T) {
	body := map[string]any{
		"system": "base instruction",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "system", "content": "mid instruction"},
			map[string]any{"role": "assistant", "content": "hello"},
		},
	}
	NormalizeClaudePassthrough(body, "claude-opus-4.8")
	sys, ok := body["system"].([]any)
	if !ok {
		t.Fatalf("system = %#v, want array", body["system"])
	}
	// First block is the wrapped existing string; second is the hoisted one.
	if len(sys) != 2 {
		t.Fatalf("len(system) = %d, want 2", len(sys))
	}
	first := sys[0].(map[string]any)
	if first["text"] != "base instruction" {
		t.Errorf("first system block = %#v", first)
	}
	second := sys[1].(map[string]any)
	if second["text"] != "mid instruction" {
		t.Errorf("hoisted system block = %#v", second)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("len(messages) = %d, want 2 (system hoisted out)", len(msgs))
	}
	if r, _ := msgs[0].(map[string]any)["role"].(string); r != "user" {
		t.Errorf("first remaining role = %v, want user", r)
	}
}

// TestNormalizeHoistSystemBlocks verifies a system message with block-array
// content is collapsed and hoisted.
func TestNormalizeHoistSystemBlocks(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "line1"},
				map[string]any{"type": "text", "text": "line2"},
			}},
			map[string]any{"role": "user", "content": "ok"},
		},
	}
	NormalizeClaudePassthrough(body, "claude-opus-4.8")
	sys := body["system"].([]any)
	if len(sys) != 1 {
		t.Fatalf("len(system) = %d, want 1", len(sys))
	}
	text, _ := sys[0].(map[string]any)["text"].(string)
	if text != "line1\nline2" {
		t.Errorf("hoisted system text = %q, want line1\\nline2", text)
	}
}

// TestNormalizeDropForeignThinkingSignature ports cd557a25 step 3: an assistant
// turn with a foreign-signature thinking block has it dropped.
func TestNormalizeDropForeignThinkingSignature(t *testing.T) {
	body := map[string]any{
		"thinking": map[string]any{"type": "enabled"},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "thinking", "thinking": "hmm", "signature": "CiQBforeign"},
					map[string]any{"type": "text", "text": "answer"},
				},
			},
		},
	}
	NormalizeClaudePassthrough(body, "claude-opus-4.8")
	msgs := body["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("len(content) = %d, want 1 (foreign thinking dropped)", len(content))
	}
	if content[0].(map[string]any)["type"] != "text" {
		t.Errorf("remaining block = %#v, want text", content[0])
	}
}

// TestNormalizeKeepValidThinkingSignature: a valid-signature thinking block is
// kept.
func TestNormalizeKeepValidThinkingSignature(t *testing.T) {
	validSig := defaultThinkingClaudeSignature
	body := map[string]any{
		"thinking": map[string]any{"type": "enabled"},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "thinking", "thinking": "hmm", "signature": validSig},
					map[string]any{"type": "text", "text": "answer"},
				},
			},
		},
	}
	NormalizeClaudePassthrough(body, "claude-opus-4.8")
	content := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("len(content) = %d, want 2 (valid thinking kept)", len(content))
	}
	if content[0].(map[string]any)["type"] != "thinking" {
		t.Errorf("first block = %#v, want thinking", content[0])
	}
}

// TestNormalizeReinsertPlaceholderForToolUse ports cd557a25: when thinking is
// enabled, all thinking blocks are dropped (foreign signatures), and a tool_use
// remains, a valid placeholder is unshifted ahead of the tool_use.
func TestNormalizeReinsertPlaceholderForToolUse(t *testing.T) {
	body := map[string]any{
		"thinking": map[string]any{"type": "enabled"},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "thinking", "thinking": "hmm", "signature": "CiQBforeign"},
					map[string]any{"type": "tool_use", "id": "tu_1", "name": "f", "input": map[string]any{}},
				},
			},
		},
	}
	NormalizeClaudePassthrough(body, "claude-opus-4.8")
	content := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	// placeholder + tool_use.
	if len(content) != 2 {
		t.Fatalf("len(content) = %d, want 2 (placeholder + tool_use)", len(content))
	}
	ph := content[0].(map[string]any)
	if ph["type"] != "thinking" {
		t.Errorf("placeholder type = %v, want thinking", ph["type"])
	}
	if ph["thinking"] != "." {
		t.Errorf("placeholder thinking = %v, want '.'", ph["thinking"])
	}
	sig, _ := ph["signature"].(string)
	if !isValidClaudeSignature(sig) {
		t.Error("placeholder signature must be valid")
	}
	if content[1].(map[string]any)["type"] != "tool_use" {
		t.Errorf("second block = %#v, want tool_use", content[1])
	}
}

// TestNormalizeNoPlaceholderWhenThinkingDisabled: placeholder re-insertion only
// happens when thinking is enabled. With thinking disabled, a bare tool_use is
// left as-is.
func TestNormalizeNoPlaceholderWhenThinkingDisabled(t *testing.T) {
	body := map[string]any{
		"thinking": map[string]any{"type": "disabled"},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "thinking", "thinking": "hmm", "signature": "CiQBforeign"},
					map[string]any{"type": "tool_use", "id": "tu_1", "name": "f", "input": map[string]any{}},
				},
			},
		},
	}
	NormalizeClaudePassthrough(body, "claude-opus-4.8")
	content := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("len(content) = %d, want 1 (no placeholder when disabled)", len(content))
	}
	if content[0].(map[string]any)["type"] != "tool_use" {
		t.Errorf("remaining block = %#v, want tool_use", content[0])
	}
}

// TestNormalizeNoPlaceholderWhenNoToolUse: a dropped thinking block without a
// following tool_use does NOT get a placeholder (Anthropic only requires
// thinking before tool_use).
func TestNormalizeNoPlaceholderWhenNoToolUse(t *testing.T) {
	body := map[string]any{
		"thinking": map[string]any{"type": "enabled"},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "thinking", "thinking": "hmm", "signature": "CiQBforeign"},
					map[string]any{"type": "text", "text": "answer"},
				},
			},
		},
	}
	NormalizeClaudePassthrough(body, "claude-opus-4.8")
	content := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("len(content) = %d, want 1 (no placeholder without tool_use)", len(content))
	}
	if content[0].(map[string]any)["type"] != "text" {
		t.Errorf("remaining block = %#v, want text", content[0])
	}
}

// TestNormalizeHaikuAdaptiveThenPlaceholder verifies the step ordering: a Haiku
// model arrives as "adaptive" (downgraded to "enabled" in step 1), so by step 3
// thinkingEnabled is true and a placeholder IS inserted for a bare tool_use.
func TestNormalizeHaikuAdaptiveThenPlaceholder(t *testing.T) {
	body := map[string]any{
		"thinking": map[string]any{"type": "adaptive"},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "thinking", "thinking": "hmm", "signature": "CiQBforeign"},
					map[string]any{"type": "tool_use", "id": "tu_1", "name": "f", "input": map[string]any{}},
				},
			},
		},
	}
	NormalizeClaudePassthrough(body, "claude-haiku-4.5")
	content := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("len(content) = %d, want 2 (placeholder inserted after adaptive→enabled)", len(content))
	}
	if content[0].(map[string]any)["type"] != "thinking" {
		t.Errorf("first block = %#v, want placeholder thinking", content[0])
	}
}

// TestNormalizeNilBodyNoPanic verifies the nil/non-map guards.
func TestNormalizeNilBodyNoPanic(t *testing.T) {
	if got := NormalizeClaudePassthrough(nil, "claude-opus-4.8"); got != nil {
		t.Errorf("nil body should return nil, got %#v", got)
	}
	// Empty body is a no-op.
	body := map[string]any{}
	NormalizeClaudePassthrough(body, "")
	if len(body) != 0 {
		t.Errorf("empty body mutated: %#v", body)
	}
}

// TestNormalizeEmptySystemStringNotHoisted verifies an empty system string is
// not turned into a block when hoisting (the JS only pushes non-empty text).
func TestNormalizeEmptySystemStringNotHoisted(t *testing.T) {
	body := map[string]any{
		"system": "   ",
		"messages": []any{
			map[string]any{"role": "system", "content": "  "},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	NormalizeClaudePassthrough(body, "claude-opus-4.8")
	// No non-empty system blocks → system not assigned, messages keep user only.
	if sys, has := body["system"]; has {
		if arr, ok := sys.([]any); ok && len(arr) > 0 {
			t.Errorf("empty system should not produce blocks: %#v", sys)
		}
	}
	msgs := body["messages"].([]any)
	// Mirrors JS: body.messages is reassigned only when a non-empty system block
	// was hoisted. An empty role:system message yields no system block, so
	// systemBlocks is empty and body.messages is left unchanged — the empty
	// system message stays in the array (it is not hoisted, just not promoted).
	if len(msgs) != 2 {
		t.Errorf("len(messages) = %d, want 2 (empty system not hoisted, messages unchanged)", len(msgs))
	}
}

// TestNormalizeRedactedThinkingDropped covers redacted_thinking blocks: a
// foreign-signed redacted block is dropped just like a thinking block.
func TestNormalizeRedactedThinkingDropped(t *testing.T) {
	body := map[string]any{
		"thinking": map[string]any{"type": "enabled"},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "redacted_thinking", "signature": "CiQBforeign"},
					map[string]any{"type": "text", "text": "ok"},
				},
			},
		},
	}
	NormalizeClaudePassthrough(body, "claude-opus-4.8")
	content := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Errorf("redacted_thinking not dropped: %#v", content)
	}
}

// guard: ensure we didn't accidentally ship a placeholder that fails validation
// (regression on the bundled constant).
func TestPlaceholderSignatureValid(t *testing.T) {
	ph := buildThinkingPlaceholder()
	sig, _ := ph["signature"].(string)
	if !isValidClaudeSignature(sig) {
		t.Fatal("buildThinkingPlaceholder signature must pass isValidClaudeSignature")
	}
	if !strings.HasPrefix(sig, "E") {
		t.Errorf("placeholder signature should be E-form, got prefix %q", sig[:1])
	}
}
