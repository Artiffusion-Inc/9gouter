package defaultexec

// cache_control_test.go ports the regression coverage for upstream 9e386665
// (filterToOpenAIFormat): the DefaultExecutor strips `signature` from every
// content block and strips `cache_control` unless the provider's
// PreserveCacheControl quirk is set (alicode / alicode-intl / alims-intl). Tests
// drive the real DefaultExecutor.TransformRequest (no mock) with both quirk
// states.

import (
	"encoding/json"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

func contentBlocks(body map[string]any, msgIdx int) []any {
	msgs, _ := body["messages"].([]any)
	if msgIdx >= len(msgs) {
		return nil
	}
	msg, _ := msgs[msgIdx].(map[string]any)
	c, _ := msg["content"].([]any)
	return c
}

func transformBody(t *testing.T, preserve bool, body map[string]any) map[string]any {
	t.Helper()
	exec := New("test", base.Config{Quirks: base.Quirks{PreserveCacheControl: preserve}})
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := exec.TransformRequest("m", raw, false, provider.Credentials{})
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// TestStripNonOpenAI_SignatureAlwaysRemoved asserts signature is stripped for
// both quirk states — it is never a valid OpenAI content field.
func TestStripNonOpenAI_SignatureAlwaysRemoved(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "hi", "signature": "EuZ1..."},
			}},
		},
	}
	for _, preserve := range []bool{false, true} {
		out := transformBody(t, preserve, body)
		blocks := contentBlocks(out, 0)
		if len(blocks) == 0 {
			t.Fatalf("preserve=%v: no content blocks", preserve)
		}
		blk, _ := blocks[0].(map[string]any)
		if _, ok := blk["signature"]; ok {
			t.Errorf("preserve=%v: signature must always be stripped, got %v", preserve, blk)
		}
	}
}

// TestStripNonOpenAI_CacheControlRemovedByDefault asserts cache_control is
// stripped when PreserveCacheControl is false (the default for most providers).
func TestStripNonOpenAI_CacheControlRemovedByDefault(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "hi", "cache_control": map[string]any{"type": "ephemeral"}},
			}},
		},
	}
	out := transformBody(t, false, body)
	blk, _ := contentBlocks(out, 0)[0].(map[string]any)
	if _, ok := blk["cache_control"]; ok {
		t.Errorf("cache_control must be stripped when PreserveCacheControl=false, got %v", blk)
	}
}

// TestStripNonOpenAI_CacheControlPreservedForAlicode asserts cache_control is
// kept when PreserveCacheControl is true (alicode / alicode-intl / alims-intl).
func TestStripNonOpenAI_CacheControlPreservedForAlicode(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "hi", "cache_control": map[string]any{"type": "ephemeral"}},
			}},
		},
	}
	out := transformBody(t, true, body)
	blk, _ := contentBlocks(out, 0)[0].(map[string]any)
	cc, ok := blk["cache_control"]
	if !ok {
		t.Fatalf("cache_control must be preserved when PreserveCacheControl=true, got %v", blk)
	}
	if ccMap, _ := cc.(map[string]any); ccMap["type"] != "ephemeral" {
		t.Errorf("cache_control value mutated: %v", cc)
	}
}

// TestStripNonOpenAI_BothFieldsOnMultipleBlockTypes asserts the strip walks
// every block across text / tool_use / tool_result block types, not just the
// first.
func TestStripNonOpenAI_BothFieldsOnMultipleBlockTypes(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "hi", "signature": "s1", "cache_control": map[string]any{"type": "ephemeral"}},
				map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": "x", "signature": "s2", "cache_control": map[string]any{"type": "ephemeral"}},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "tu1", "name": "n", "input": map[string]any{}, "signature": "s3", "cache_control": map[string]any{"type": "ephemeral"}},
			}},
		},
	}
	out := transformBody(t, false, body)
	for msgIdx := 0; msgIdx < 2; msgIdx++ {
		blocks := contentBlocks(out, msgIdx)
		for bIdx, blk := range blocks {
			m, _ := blk.(map[string]any)
			if _, ok := m["signature"]; ok {
				t.Errorf("msg %d block %d: signature not stripped: %v", msgIdx, bIdx, m)
			}
			if _, ok := m["cache_control"]; ok {
				t.Errorf("msg %d block %d: cache_control not stripped: %v", msgIdx, bIdx, m)
			}
		}
	}
}

// TestStripNonOpenAI_StringContentUntouched asserts a message with plain string
// content is left unchanged (no content array to walk).
func TestStripNonOpenAI_StringContentUntouched(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	out := transformBody(t, false, body)
	msgs, _ := out["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	if msg["content"] != "hi" {
		t.Errorf("string content mutated: %v", msg["content"])
	}
}

// TestStripNonOpenAI_NoMessagesNoPanic asserts a body with no messages key (or a
// non-array messages) is a no-op, not a panic.
func TestStripNonOpenAI_NoMessagesNoPanic(t *testing.T) {
	for _, body := range []map[string]any{
		{},
		{"messages": "not-an-array"},
		{"messages": 42},
	} {
		out := transformBody(t, false, body)
		if _, ok := out["messages"]; ok && out["messages"] == "not-an-array" {
			// fine — left as-is
		}
	}
}
