package translator

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSynthesizeEmptyAndGarbage(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"not-json", "this is not json"},
		{"no-choices", `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[]}`},
		{"missing-choices", `{"id":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Synthesize([]byte(tc.body))
			if err != nil {
				t.Fatalf("Synthesize: %v", err)
			}
			if got != "" {
				t.Fatalf("expected empty, got %q", got)
			}
		})
	}
}

func TestSynthesizeBasic(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-test",
		"object":"chat.completion",
		"created":1700000000,
		"model":"gpt-test",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]
	}`)
	got, err := Synthesize(body)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if !strings.HasPrefix(got, "data: {") {
		t.Fatalf("expected data frames, got %q", got)
	}
	if !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Fatalf("expected [DONE] terminator, got %q", got)
	}

	frames := strings.Split(strings.TrimSpace(got), "\n\n")
	if len(frames) != 4 {
		t.Fatalf("expected 4 frames (role, content, final, [DONE]), got %d: %v", len(frames), frames)
	}

	var final map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(frames[len(frames)-2], "data: ")), &final); err != nil {
		t.Fatalf("final frame unmarshal: %v", err)
	}
	choices, _ := final["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(choices))
	}
	choice := choices[0].(map[string]any)
	if finishReason := choice["finish_reason"].(string); finishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", finishReason)
	}
}

func TestSynthesizeReasoningAndToolCalls(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-reason",
		"created":1700000001,
		"model":"reason-model",
		"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30},
		"choices":[
			{
				"index":0,
				"message":{
					"role":"assistant",
					"reasoning_content":"I think",
					"content":"therefore I am",
					"tool_calls":[{"id":"call_1","type":"function","function":{"name":"foo","arguments":"{}"}}]
				},
				"finish_reason":"tool_calls"
			}
		]
	}`)
	got, err := Synthesize(body)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Fatalf("expected [DONE] terminator, got %q", got)
	}

	frames := strings.Split(strings.TrimSpace(got), "\n\n")
	if len(frames) != 6 { // role, reasoning, content, tool_calls, final, [DONE]
		t.Fatalf("expected 6 frames, got %d: %v", len(frames), frames)
	}

	assertFrameContains(t, frames[0], "role", "assistant")
	assertFrameContains(t, frames[1], "reasoning_content", "I think")
	assertFrameContains(t, frames[2], "content", "therefore I am")
	assertFrameContains(t, frames[3], "tool_calls", "call_1")

	var final map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(frames[4], "data: ")), &final); err != nil {
		t.Fatalf("final frame unmarshal: %v", err)
	}
	if _, ok := final["usage"]; !ok {
		t.Fatalf("expected usage in final frame")
	}
}

func TestSynthesizeUnsupportedReasoning(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-unsupported",
		"created":1700000002,
		"model":"m",
		"choices":[{"index":0,"message":{"thinking":"unsupported reasoning"},"finish_reason":"stop"}]
	}`)
	got, err := Synthesize(body)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	frames := strings.Split(strings.TrimSpace(got), "\n\n")
	if len(frames) != 4 {
		t.Fatalf("expected 4 frames, got %d: %v", len(frames), frames)
	}
	assertFrameContains(t, frames[0], "role", "assistant")
	assertFrameContains(t, frames[1], "reasoning_content", "unsupported reasoning")
}

func TestSynthesizeFinishReasonMappings(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"max_tokens", "length"},
		{"safety", "content_filter"},
		{"STOP", "stop"},
		{"", "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			body := []byte(`{
				"id":"chatcmpl-fr",
				"created":1,
				"model":"m",
				"choices":[{"index":0,"message":{},"finish_reason":"` + tc.input + `"}]
			}`)
			got, err := Synthesize(body)
			if err != nil {
				t.Fatalf("Synthesize: %v", err)
			}
			frames := strings.Split(strings.TrimSpace(got), "\n\n")
			var final map[string]any
			lastData := frames[len(frames)-2]
			if err := json.Unmarshal([]byte(strings.TrimPrefix(lastData, "data: ")), &final); err != nil {
				t.Fatalf("final frame unmarshal: %v", err)
			}
			choices := final["choices"].([]any)
			choice := choices[0].(map[string]any)
			if got := choice["finish_reason"].(string); got != tc.want {
				t.Fatalf("finish_reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func assertFrameContains(t *testing.T, frame, key, substring string) {
	t.Helper()
	data := strings.TrimPrefix(frame, "data: ")
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("unmarshal frame %q: %v", frame, err)
	}
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in frame %q", frame)
	}
	choice := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	v, ok := delta[key]
	if !ok {
		t.Fatalf("frame missing delta.%s in %q", key, frame)
	}
	var str string
	switch val := v.(type) {
	case string:
		str = val
	case []any:
		b, _ := json.Marshal(val)
		str = string(b)
	default:
		b, _ := json.Marshal(v)
		str = string(b)
	}
	if !strings.Contains(str, substring) {
		t.Fatalf("delta.%s = %q, want containing %q", key, str, substring)
	}
}
