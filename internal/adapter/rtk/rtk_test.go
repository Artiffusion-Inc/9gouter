package rtk

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9router/internal/domain/format"
)

func TestCompressMessages_Disabled(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "tool", "content": strings.Repeat("a", 1000)},
		},
	}
	stats := CompressMessages(body, false, nil)
	if stats != nil {
		t.Fatalf("expected nil when disabled, got %+v", stats)
	}
}

func TestCompressMessages_ToolString(t *testing.T) {
	text := strings.Repeat("error: something failed\n", 200)
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "tool", "content": text},
		},
	}
	stats := CompressMessages(body, true, nil)
	if stats == nil || len(stats.Hits) == 0 {
		t.Fatalf("expected compression hits, got %+v", stats)
	}
	if stats.BytesAfter >= stats.BytesBefore {
		t.Fatalf("expected savings, before=%d after=%d", stats.BytesBefore, stats.BytesAfter)
	}
}

func TestInjectCaveman_OpenAI(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	InjectCaveman(body, format.Openai, "lite")
	msgs := body["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"].(string) != "system" {
		t.Fatalf("expected prepended system, got %+v", first)
	}
}

func TestInjectPonytail_ClaudSystemString(t *testing.T) {
	body := map[string]any{
		"system": "base",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	InjectPonytail(body, format.Claude, "full")
	sys := body["system"].(string)
	if !strings.Contains(sys, "lazy senior developer") {
		t.Fatalf("expected ponytail prompt, got %q", sys)
	}
}

func TestFormatRtkLog(t *testing.T) {
	stats := &Stats{
		BytesBefore: 1000,
		BytesAfter:  800,
		Hits:        []Hit{{Filter: "dedup-log", Saved: 200}},
	}
	line := FormatRtkLog(stats)
	if !strings.Contains(line, "saved 200B") {
		t.Fatalf("unexpected log line: %s", line)
	}
}

func TestAutoDetectFilter(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		expect string
	}{
		{"git diff", "diff --git a/foo b/foo\n@@ -1 +1 @@\n-hello\n+world\n", FilterGitDiff},
		{"git status", "On branch main\nChanges to be committed:\n\tmodified:   foo.go\n", FilterGitStatus},
		{"dedup", strings.Repeat("a\n", 10), FilterDedupLog},
		{"smart truncate", func() string {
			var b strings.Builder
			for i := 0; i < 300; i++ {
				fmt.Fprintf(&b, "unique line number %d with some content\n", i)
			}
			return b.String()
		}(), FilterSmartTruncate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := autoDetectFilter(tc.input)
			if fn == nil {
				if tc.expect != "" {
					t.Fatalf("expected %s, got nil", tc.expect)
				}
				return
			}
			got := filterName(fn)
			if got != tc.expect {
				t.Fatalf("expected %s, got %s", tc.expect, got)
			}
		})
	}
}
