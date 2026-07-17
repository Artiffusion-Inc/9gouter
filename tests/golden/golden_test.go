// Package golden ports the Vitest snapshot-based golden contract harness from
// tests/translator/ to Go. It loads .snap fixtures via go:embed, parses them,
// and asserts that the Go translator reproduces each snapshot byte-for-byte
// after the same clean() normalization the JS tests apply.
package golden

import (
	"embed"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/adapter/translator"
	_ "github.com/Artiffusion-Inc/9router/internal/adapter/translator/openai"
	"github.com/Artiffusion-Inc/9router/internal/domain/format"
)

//go:embed fixtures/*.snap
var fixtures embed.FS

func TestParseAllSnapshots(t *testing.T) {
	for _, name := range []string{
		"fixtures/golden-request.test.js.snap",
		"fixtures/golden-url-header.test.js.snap",
		"fixtures/golden-response-stream.test.js.snap",
		"fixtures/golden-translator-concerns.test.js.snap",
	} {
		snaps, err := parseSnapFile(fixtures, name)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if len(snaps) == 0 {
			t.Fatalf("no snapshots found in %s", name)
		}
		t.Logf("%s: parsed %d snapshots", name, len(snaps))
	}
}

func TestGoldenRequestOpenAIToClaudeFullBody(t *testing.T) {
	snaps, err := parseSnapFile(fixtures, "fixtures/golden-request.test.js.snap")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	const key = "GOLDEN request: OpenAI → Claude > full body (system/image/tool/tool_result) 1"
	want, ok := snaps[key]
	if !ok {
		t.Fatalf("snapshot %q not found; have keys: %v", key, keys(snaps))
	}

	body, err := json.Marshal(baseBody())
	if err != nil {
		t.Fatalf("marshal baseBody: %v", err)
	}

	gotBytes, err := translator.TranslateRequest(format.Openai, format.Claude, "claude-opus-4-6", body, true, "claude")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	got, err := clean(gotBytes)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}

	if got != want {
		t.Errorf("snapshot mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// baseBody returns the OpenAI request body used by the JS golden-request.test.js.
func baseBody() map[string]any {
	return map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful."},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "What's in this image?"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,IMGDATA", "detail": "high"}},
			}},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"NYC"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "get_weather", "description": "Get weather", "parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []any{"city"}}}},
		},
		"temperature": 0.7,
	}
}

// clean normalizes a JSON body exactly like golden-request.test.js clean():
//   - drop _toolNameMap, conversationId, agentContinuationId keys at any level
//   - replace "Current time is <...>" with "Current time is <TS>"
//
// It returns a compact JSON string matching how the JS snapshot bodies are
// stored (Vitest snapshots use JSON.stringify with 2-space indentation).
func clean(raw []byte) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	v = stripKeys(v, map[string]struct{}{
		"_toolNameMap":        {},
		"conversationId":      {},
		"agentContinuationId": {},
	})
	s := snapshotString(v)
	re := regexp.MustCompile(`Current time is [^"\\]+`)
	s = string(re.ReplaceAll([]byte(s), []byte("Current time is <TS>")))
	return s, nil
}

func stripKeys(v any, drop map[string]struct{}) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v2 := range x {
			if _, ok := drop[k]; ok {
				continue
			}
			out[k] = stripKeys(v2, drop)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v2 := range x {
			out[i] = stripKeys(v2, drop)
		}
		return out
	default:
		return x
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
