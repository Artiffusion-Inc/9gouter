package http

import (
	"bytes"
	"strings"
	"testing"
)

func TestOllamaTransform_ContentAndDone(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":" world"}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var out bytes.Buffer
	conv := newOllamaNDJSONConverter("llama3.2")
	if err := conv.Convert(&out, strings.NewReader(sse)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	want := 3 // Hello, world, done
	if len(lines) != want {
		t.Fatalf("got %d lines, want %d: %s", len(lines), want, out.String())
	}
	if !strings.Contains(lines[0], `"content":"Hello"`) || !strings.Contains(lines[0], `"done":false`) {
		t.Errorf("line0 = %s", lines[0])
	}
	if !strings.Contains(lines[1], `"content":" world"`) {
		t.Errorf("line1 = %s", lines[1])
	}
	if !strings.Contains(lines[2], `"done":true`) || !strings.Contains(lines[2], `"content":""`) {
		t.Errorf("line2 (done sentinel) = %s", lines[2])
	}
	if conv.sentinelEmitted != true {
		t.Errorf("sentinelEmitted should be true after done")
	}
}

func TestOllamaTransform_ToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get","arguments":""}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\""}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var out bytes.Buffer
	conv := newOllamaNDJSONConverter("m1")
	if err := conv.Convert(&out, strings.NewReader(sse)); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	if !strings.Contains(body, `"tool_calls"`) {
		t.Errorf("missing tool_calls in output: %s", body)
	}
	if !strings.Contains(body, `"name":"get"`) {
		t.Errorf("missing tool name: %s", body)
	}
	// arguments should be parsed JSON object {"q":1}
	if !strings.Contains(body, `"arguments":{"q":1}`) {
		t.Errorf("arguments not parsed: %s", body)
	}
	if !strings.Contains(body, `"done":true`) {
		t.Errorf("missing done:true: %s", body)
	}
	// [DONE] after tool_calls must NOT emit a second done sentinel.
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 NDJSON line (tool_calls+done), got %d: %s", len(lines), body)
	}
}

func TestOllamaTransform_EOFEmitsDone(t *testing.T) {
	// No [DONE], no finish_reason — EOF must emit the trailing done sentinel.
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		``,
	}, "\n")
	var out bytes.Buffer
	conv := newOllamaNDJSONConverter("m")
	_ = conv.Convert(&out, strings.NewReader(sse))
	body := out.String()
	if !strings.Contains(body, `"done":true`) {
		t.Errorf("EOF should emit done:true: %s", body)
	}
}

func TestOllamaTransform_NoDoubleDoneAfterDONE(t *testing.T) {
	// [DONE] emits done:true, then EOF must NOT emit it again.
	sse := "data: [DONE]\n\n"
	var out bytes.Buffer
	conv := newOllamaNDJSONConverter("m")
	_ = conv.Convert(&out, strings.NewReader(sse))
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("expected exactly 1 done line, got %d: %s", len(lines), out.String())
	}
}

func TestOllamaTransform_MalformedJSONSkipped(t *testing.T) {
	sse := strings.Join([]string{
		`data: {not valid json`,
		``,
		`data: {"choices":[{"delta":{"content":"ok"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	var out bytes.Buffer
	conv := newOllamaNDJSONConverter("m")
	_ = conv.Convert(&out, strings.NewReader(sse))
	body := out.String()
	if !strings.Contains(body, `"content":"ok"`) {
		t.Errorf("valid line after malformed one should still emit: %s", body)
	}
}

func TestOllamaTransform_EmptyModelDefaults(t *testing.T) {
	conv := newOllamaNDJSONConverter("")
	if conv.model != "llama3.2" {
		t.Errorf("empty model should default to llama3.2, got %q", conv.model)
	}
}

func TestOllamaTransform_NonDataLinesIgnored(t *testing.T) {
	sse := strings.Join([]string{
		`: comment line`,
		`event: ping`,
		`data: {"choices":[{"delta":{"content":"x"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	var out bytes.Buffer
	conv := newOllamaNDJSONConverter("m")
	_ = conv.Convert(&out, strings.NewReader(sse))
	body := out.String()
	if !strings.Contains(body, `"content":"x"`) {
		t.Errorf("content line should emit: %s", body)
	}
}