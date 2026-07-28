package proxychat

// streamusage_test.go pins the streaming usage collector (#162): parsing
// final usage from de-framed SSE/NDJSON frames across provider formats
// (OpenAI chat, OpenAI Responses, Claude message_start/message_delta,
// Gemini usageMetadata, Ollama eval_count), and the TTFT→streamMs/tps math.

import (
	"encoding/json"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/pricing"
)

// TestStreamUsageCollector_OpenAITerminalChunk: an OpenAI chat.completions
// stream where only the final chunk carries {usage:{...}} with
// cached_tokens + completion_tokens_details.reasoning_tokens.
func TestStreamUsageCollector_OpenAITerminalChunk(t *testing.T) {
	c := newStreamUsageCollector()
	// delta chunk (no usage)
	c.OnFrame([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"))
	// terminal chunk with usage
	c.OnFrame([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":45,"total_tokens":165,"cached_tokens":80,"completion_tokens_details":{"reasoning_tokens":12}}}` + "\n\n"))

	prompt, completion := c.promptCompletion()
	if prompt != 120 || completion != 45 {
		t.Fatalf("prompt=%d completion=%d, want 120/45", prompt, completion)
	}
	tok := c.tokens()
	if tok == nil {
		t.Fatal("tokens() nil, want breakdown")
	}
	if tok.CachedTokens != 80 {
		t.Errorf("cached=%d want 80", tok.CachedTokens)
	}
	if tok.ReasoningTokens != 12 {
		t.Errorf("reasoning=%d want 12", tok.ReasoningTokens)
	}
	if tok.PromptTokens != 120 || tok.CompletionTokens != 45 {
		t.Errorf("prompt/completion = %d/%d want 120/45", tok.PromptTokens, tok.CompletionTokens)
	}
}

// TestStreamUsageCollector_ClaudeMessageStartDelta: Claude streams
// message_start (input_tokens + cache fields) then message_delta
// (cumulative output_tokens). The final state must hold both.
func TestStreamUsageCollector_ClaudeMessageStartDelta(t *testing.T) {
	c := newStreamUsageCollector()
	c.OnFrame([]byte(`event: message_start` + "\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":500,"cache_read_input_tokens":300,"cache_creation_input_tokens":50,"output_tokens":0}}}` + "\n\n"))
	c.OnFrame([]byte(`event: message_delta` + "\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":210}}` + "\n\n"))

	prompt, completion := c.promptCompletion()
	if prompt != 500 || completion != 210 {
		t.Fatalf("prompt=%d completion=%d, want 500/210", prompt, completion)
	}
	tok := c.tokens()
	if tok == nil {
		t.Fatal("tokens() nil")
	}
	if tok.CachedTokens != 300 {
		t.Errorf("cached=%d want 300", tok.CachedTokens)
	}
	if tok.CacheCreationTokens != 50 {
		t.Errorf("cache_creation=%d want 50", tok.CacheCreationTokens)
	}
}

// TestStreamUsageCollector_GeminiUsageMetadata: Gemini streams a terminal
// usageMetadata frame. usageMetadata must normalize to prompt/completion/
// cached/reasoning.
func TestStreamUsageCollector_GeminiUsageMetadata(t *testing.T) {
	c := newStreamUsageCollector()
	c.OnFrame([]byte(`data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":77,"candidatesTokenCount":33,"totalTokenCount":110,"cachedContentTokenCount":40,"thoughtsTokenCount":8}}` + "\n\n"))

	prompt, completion := c.promptCompletion()
	if prompt != 77 || completion != 33 {
		t.Fatalf("prompt=%d completion=%d, want 77/33", prompt, completion)
	}
	tok := c.tokens()
	if tok == nil {
		t.Fatal("tokens() nil")
	}
	if tok.CachedTokens != 40 {
		t.Errorf("cached=%d want 40", tok.CachedTokens)
	}
	if tok.ReasoningTokens != 8 {
		t.Errorf("reasoning=%d want 8", tok.ReasoningTokens)
	}
}

// TestStreamUsageCollector_OllamaNDJSON: Ollama streams bare JSON lines;
// the final done=true line carries eval_count + prompt_eval_count (no
// usage wrapper). These must be recognized.
func TestStreamUsageCollector_OllamaNDJSON(t *testing.T) {
	c := newStreamUsageCollector()
	c.OnFrame([]byte(`{"model":"gemma3","message":{"role":"assistant","content":"hi"},"done":false}` + "\n"))
	c.OnFrame([]byte(`{"model":"gemma3","done":true,"done_reason":"stop","eval_count":42,"prompt_eval_count":18,"total_duration":1}` + "\n"))

	prompt, completion := c.promptCompletion()
	if prompt != 18 || completion != 42 {
		t.Fatalf("prompt=%d completion=%d, want 18/42", prompt, completion)
	}
}

// TestStreamUsageCollector_OpenAIResponses: the response.completed event
// nests usage under response.usage with input_tokens/output_tokens +
// input_tokens_details (cached) / output_tokens_details (reasoning).
func TestStreamUsageCollector_OpenAIResponses(t *testing.T) {
	c := newStreamUsageCollector()
	c.OnFrame([]byte(`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n"))
	c.OnFrame([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":200,"output_tokens":60,"input_tokens_details":{"cached_tokens":90},"output_tokens_details":{"reasoning_tokens":15}}}}` + "\n\n"))

	prompt, completion := c.promptCompletion()
	if prompt != 200 || completion != 60 {
		t.Fatalf("prompt=%d completion=%d, want 200/60", prompt, completion)
	}
	tok := c.tokens()
	if tok == nil {
		t.Fatal("tokens() nil")
	}
	if tok.CachedTokens != 90 {
		t.Errorf("cached=%d want 90", tok.CachedTokens)
	}
	if tok.ReasoningTokens != 15 {
		t.Errorf("reasoning=%d want 15", tok.ReasoningTokens)
	}
}

// TestStreamUsageCollector_ToleratesGarbage: non-JSON lines, "[DONE]"
// sentinels, keepalive comments, and "event:" lines must not abort
// collection. The collector must silently skip them.
func TestStreamUsageCollector_ToleratesGarbage(t *testing.T) {
	c := newStreamUsageCollector()
	c.OnFrame([]byte(`: keepalive` + "\n\n"))
	c.OnFrame([]byte(`event: ping` + "\n\n"))
	c.OnFrame([]byte(`data: [DONE]` + "\n\n"))
	c.OnFrame([]byte(`not json at all` + "\n\n"))
	c.OnFrame([]byte(`data: {"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n"))

	prompt, completion := c.promptCompletion()
	if prompt != 10 || completion != 5 {
		t.Fatalf("prompt=%d completion=%d, want 10/5 (garbage must not abort)", prompt, completion)
	}
}

// TestStreamUsageCollector_Empty: a stream with no usage frame yields a nil
// tokens() so the caller falls back to headroom estimates.
func TestStreamUsageCollector_Empty(t *testing.T) {
	c := newStreamUsageCollector()
	c.OnFrame([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"))
	if c.tokens() != nil {
		t.Errorf("tokens() = %+v, want nil when no usage collected", c.tokens())
	}
	p, comp := c.promptCompletion()
	if p != 0 || comp != 0 {
		t.Errorf("prompt/completion = %d/%d, want 0/0", p, comp)
	}
}

// TestStreamUsageCollector_MergeAcrossFrames: usage split across two frames
// (prompt on one, completion+cached on another) must merge into one record.
func TestStreamUsageCollector_MergeAcrossFrames(t *testing.T) {
	c := newStreamUsageCollector()
	c.OnFrame([]byte(`data: {"usage":{"prompt_tokens":300}}` + "\n\n"))
	c.OnFrame([]byte(`data: {"usage":{"completion_tokens":90,"cached_tokens":150}}` + "\n\n"))
	prompt, completion := c.promptCompletion()
	if prompt != 300 || completion != 90 {
		t.Fatalf("prompt=%d completion=%d, want 300/90", prompt, completion)
	}
	if tok := c.tokens(); tok == nil || tok.CachedTokens != 150 {
		t.Errorf("cached not merged: %+v", tok)
	}
}

// TestStreamUsageCollector_ZeroDoesNotClobber: a later frame carrying a 0
// for a field must not overwrite a real value collected earlier (Claude's
// message_start sets output_tokens:0 before the delta carries the real count).
func TestStreamUsageCollector_ZeroDoesNotClobber(t *testing.T) {
	c := newStreamUsageCollector()
	c.OnFrame([]byte(`data: {"usage":{"completion_tokens":210}}` + "\n\n"))
	c.OnFrame([]byte(`data: {"usage":{"completion_tokens":0}}` + "\n\n"))
	_, completion := c.promptCompletion()
	if completion != 210 {
		t.Fatalf("completion=%d, want 210 (zero must not clobber)", completion)
	}
}

// TestStripSSEPrefix covers the de-framed payload trimming.
func TestStripSSEPrefix(t *testing.T) {
	cases := map[string]string{
		"data: {\"a\":1}\n\n":             `{"a":1}`,
		"{\"a\":1}\n":                     `{"a":1}`, // ndjson bare line
		"data: [DONE]\n\n":                "",
		": comment\n\n":                   "",
		"event: foo\ndata: {\"a\":1}\n\n": `{"a":1}`,
		"garbage\n\n":                     "",
	}
	for in, want := range cases {
		got := stripSSEPrefix([]byte(in))
		if (len(got) == 0) != (want == "") && string(got) != want {
			t.Errorf("stripSSEPrefix(%q) = %q, want %q", in, string(got), want)
		} else if want != "" && string(got) != want {
			t.Errorf("stripSSEPrefix(%q) = %q, want %q", in, string(got), want)
		}
	}
}

// TestStreamTokensMap_Building verifies the detail tokens map for the
// streaming path.
func TestStreamTokensMap_Building(t *testing.T) {
	tok := &pricing.Tokens{PromptTokens: 100, CompletionTokens: 50, CachedTokens: 30, ReasoningTokens: 10, CacheCreationTokens: 5}
	m := streamTokensMap(tok, 100, 50)
	if m["prompt_tokens"] != 100 || m["completion_tokens"] != 50 {
		t.Errorf("flat counts wrong: %+v", m)
	}
	if m["cached_tokens"] != 30 || m["reasoning_tokens"] != 10 || m["cache_creation_input_tokens"] != 5 {
		t.Errorf("breakdown wrong: %+v", m)
	}
	if m["total_tokens"] != 150 {
		t.Errorf("total wrong: %d", m["total_tokens"])
	}
}

// TestPricingTokensJSON ensures the collector's output is JSON-marshallable
// (the detail save path marshals it).
func TestPricingTokensJSON(t *testing.T) {
	tok := &pricing.Tokens{PromptTokens: 1, CompletionTokens: 2, CachedTokens: 3}
	b, err := json.Marshal(tok)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) == "" {
		t.Error("empty marshalled tokens")
	}
}
