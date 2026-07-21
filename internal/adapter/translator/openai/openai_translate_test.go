package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

// mustUnmarshal is a test helper.
func mustUnmarshal(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, b)
	}
	return v
}

// asMap extracts a sub-object field.
func asMap(t *testing.T, v map[string]any, key string) map[string]any {
	t.Helper()
	m, ok := v[key].(map[string]any)
	if !ok {
		t.Fatalf("field %q is not an object, got %T", key, v[key])
	}
	return m
}

// asArray extracts a sub-array field.
func asArray(t *testing.T, v map[string]any, key string) []any {
	t.Helper()
	a, ok := v[key].([]any)
	if !ok {
		t.Fatalf("field %q is not an array, got %T", key, v[key])
	}
	return a
}

// --- adjustMaxTokens ---

func TestAdjustMaxTokensDefault(t *testing.T) {
	got := adjustMaxTokens(map[string]any{})
	if got != defaultMaxTokens {
		t.Fatalf("default = %d, want %d", got, defaultMaxTokens)
	}
}

func TestAdjustMaxTokensExplicit(t *testing.T) {
	got := adjustMaxTokens(map[string]any{"max_tokens": float64(1024)})
	if got != 1024 {
		t.Fatalf("explicit = %d, want 1024", got)
	}
}

func TestAdjustMaxTokensJSONNumber(t *testing.T) {
	got := adjustMaxTokens(map[string]any{"max_tokens": json.Number("2048")})
	if got != 2048 {
		t.Fatalf("json.Number = %d, want 2048", got)
	}
}

func TestAdjustMaxTokensClampsToDefault(t *testing.T) {
	got := adjustMaxTokens(map[string]any{"max_tokens": float64(999_999)})
	if got != defaultMaxTokens {
		t.Fatalf("over-limit = %d, want clamped %d", got, defaultMaxTokens)
	}
}

func TestAdjustMaxTokensToolsFloor(t *testing.T) {
	// With tools and a low max_tokens, it is bumped to defaultMinTokens.
	got := adjustMaxTokens(map[string]any{
		"max_tokens": float64(100),
		"tools":      []any{map[string]any{"type": "function"}},
	})
	if got != defaultMinTokens {
		t.Fatalf("tools floor = %d, want %d", got, defaultMinTokens)
	}
}

func TestAdjustMaxTokensThinkingBudget(t *testing.T) {
	// max_tokens <= budget_tokens → max_tokens = budget + 1024.
	got := adjustMaxTokens(map[string]any{
		"max_tokens": float64(2000),
		"thinking":   map[string]any{"budget_tokens": float64(4000)},
	})
	if got != 4000+1024 {
		t.Fatalf("thinking budget = %d, want %d", got, 4000+1024)
	}
}

// --- supportsClaudeAdaptiveThinking ---

func TestSupportsClaudeAdaptiveThinking(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"claude-opus-4-6", true},
		{"claude-sonnet-4-7", true},
		{"claude-opus-4-6-20250827", true},
		{"anthropic/claude-opus-4-6", true},
		{"claude-3-5-sonnet", false},   // 3.x
		{"claude-opus-4-5", false},     // 4.5 boundary excluded
		{"claude-opus-4", false},       // 4.0 no minor
		{"gpt-4o", false},              // not claude
		{"claude-haiku-4-5-20250827", false},
		{"claude-sonnet-4-5", false},
		{"claude-opus-4-6-something", true},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			if got := supportsClaudeAdaptiveThinking(c.model); got != c.want {
				t.Fatalf("supportsClaudeAdaptiveThinking(%q) = %v, want %v", c.model, got, c.want)
			}
		})
	}
}

// --- normalizeClaudeEffort ---

func TestNormalizeClaudeEffort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},
		{"xhigh", "high"},
		{"max", "high"},
		{"HIGH", "high"},
		{"bogus", "medium"},
		{"", "medium"},
	}
	for _, c := range cases {
		if got := normalizeClaudeEffort(c.in); got != c.want {
			t.Errorf("normalizeClaudeEffort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- parseDataURI ---

func TestParseDataURI(t *testing.T) {
	p := parseDataURI("data:image/png;base64,abc123==")
	if p == nil {
		t.Fatal("parseDataURI returned nil for valid URI")
	}
	if p.mimeType != "image/png" || p.base64 != "abc123==" {
		t.Fatalf("parsed = %+v, want image/png/abc123==", p)
	}
	if parseDataURI("https://example.com/x.png") != nil {
		t.Error("non-data URI should return nil")
	}
	if parseDataURI("data:image/png,abc") != nil {
		t.Error("non-base64 data URI should return nil")
	}
}

// --- convertOpenAIToolChoice ---

func TestConvertOpenAIToolChoice(t *testing.T) {
	// string "required" → type any
	got := convertOpenAIToolChoice("required")
	if got["type"] != "any" {
		t.Fatalf("required → %v, want type=any", got)
	}
	// other string → auto
	got = convertOpenAIToolChoice("auto")
	if got["type"] != "auto" {
		t.Fatalf("auto → %v, want type=auto", got)
	}
	// named function
	got = convertOpenAIToolChoice(map[string]any{
		"type": "function",
		"function": map[string]any{"name": "get_weather"},
	})
	if got["type"] != "tool" || got["name"] != "get_weather" {
		t.Fatalf("named → %v, want type=tool name=get_weather", got)
	}
	// pass-through recognized type
	got = convertOpenAIToolChoice(map[string]any{"type": "none"})
	if got["type"] != "none" {
		t.Fatalf("none passthrough → %v, want type=none", got)
	}
	// unknown map → default auto
	got = convertOpenAIToolChoice(map[string]any{"type": "weird"})
	if got["type"] != "auto" {
		t.Fatalf("unknown → %v, want type=auto", got)
	}
	// unknown type → auto
	got = convertOpenAIToolChoice(123)
	if got["type"] != "auto" {
		t.Fatalf("int → %v, want type=auto", got)
	}
}

// --- extractTextContent ---

func TestExtractTextContent(t *testing.T) {
	if got := extractTextContent("plain"); got != "plain" {
		t.Fatalf("string = %q, want plain", got)
	}
	if got := extractTextContent(123); got != "" {
		t.Fatalf("non-string/array = %q, want empty", got)
	}
	got := extractTextContent([]any{
		map[string]any{"type": "text", "text": "hello "},
		map[string]any{"type": "text", "text": "world"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}}, // ignored
	})
	if got != "hello world" {
		t.Fatalf("array = %q, want 'hello world'", got)
	}
}

// --- clampCallID ---

func TestClampCallID(t *testing.T) {
	short := "call_123"
	if got := clampCallID(short); got != short {
		t.Fatalf("clamp short = %v, want %v", got, short)
	}
	long := strings.Repeat("a", maxCallIDLen+10)
	if got, ok := clampCallID(long).(string); !ok || len(got) != maxCallIDLen {
		t.Fatalf("clamp long = %v len=%d, want len=%d", got, len(got), maxCallIDLen)
	}
	// non-string passthrough
	if got := clampCallID(42); got != 42 {
		t.Fatalf("clamp int = %v, want 42", got)
	}
}

// --- openaiToClaudeRequest: end-to-end scenarios ---

func TestOpenAIToClaudeRequestBasic(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	out, err := openaiToClaudeRequest("claude-opus-4-6", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	if v["model"] != "claude-opus-4-6" {
		t.Errorf("model = %v", v["model"])
	}
	if v["stream"] != true {
		t.Errorf("stream = %v, want true", v["stream"])
	}
	msgs := asArray(t, v, "messages")
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "user" {
		t.Errorf("role = %v, want user", m["role"])
	}
}

func TestOpenAIToClaudeRequestSystemExtraction(t *testing.T) {
	// system messages are pulled out into result["system"] blocks.
	body := `{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`
	out, err := openaiToClaudeRequest("claude-opus-4-6", json.RawMessage(body), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	sys := asArray(t, v, "system")
	// Always has the Claude Code prompt block first + the user system block.
	if len(sys) < 2 {
		t.Fatalf("system blocks = %d, want >=2", len(sys))
	}
	first := sys[0].(map[string]any)
	if first["type"] != "text" || first["text"] != claudeSystemPrompt {
		t.Errorf("first system block = %+v, want Claude Code prompt", first)
	}
	// Last system block carries cache_control.
	last := sys[len(sys)-1].(map[string]any)
	if _, ok := last["cache_control"]; !ok {
		t.Errorf("last system block missing cache_control")
	}
	// Messages must NOT contain the system role.
	msgs := asArray(t, v, "messages")
	for _, mm := range msgs {
		if mm.(map[string]any)["role"] == "system" {
			t.Errorf("system message leaked into messages")
		}
	}
}

func TestOpenAIToClaudeRequestToolRoleBecomesUser(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"result"}]}`
	out, err := openaiToClaudeRequest("claude-opus-4-6", json.RawMessage(body), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	msgs := asArray(t, v, "messages")
	// tool message must be emitted as a user-role message containing a
	// tool_result block.
	var foundToolResult bool
	for _, mm := range msgs {
		m := mm.(map[string]any)
		content, _ := m["content"].([]any)
		for _, b := range content {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if bm["type"] == "tool_result" {
				foundToolResult = true
				if bm["tool_use_id"] != "c1" {
					t.Errorf("tool_use_id = %v, want c1", bm["tool_use_id"])
				}
			}
		}
	}
	if !foundToolResult {
		t.Errorf("no tool_result block emitted from role=tool message")
	}
}

func TestOpenAIToClaudeRequestAssistantToolUseBlock(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]}]}`
	out, err := openaiToClaudeRequest("claude-opus-4-6", json.RawMessage(body), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	msgs := asArray(t, v, "messages")
	var foundToolUse bool
	for _, mm := range msgs {
		m := mm.(map[string]any)
		content, _ := m["content"].([]any)
		for _, b := range content {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if bm["type"] == "tool_use" {
				foundToolUse = true
				if bm["name"] != "get_weather" {
					t.Errorf("tool_use name = %v, want get_weather", bm["name"])
				}
				// arguments JSON should be parsed into an object.
				in, ok := bm["input"].(map[string]any)
				if !ok || in["city"] != "SF" {
					t.Errorf("tool_use input = %v, want {city:SF}", bm["input"])
				}
			}
		}
	}
	if !foundToolUse {
		t.Errorf("no tool_use block emitted from assistant tool_calls")
	}
}

func TestOpenAIToClaudeRequestTools(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}]}`
	out, err := openaiToClaudeRequest("claude-opus-4-6", json.RawMessage(body), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	tools := asArray(t, v, "tools")
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "f" {
		t.Errorf("tool name = %v, want f", tool["name"])
	}
	if tool["description"] != "d" {
		t.Errorf("tool description = %v, want d", tool["description"])
	}
	if _, ok := tool["cache_control"]; !ok {
		t.Errorf("last tool missing cache_control")
	}
}

func TestOpenAIToClaudeRequestToolChoice(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"}],"tool_choice":"required"}`
	out, err := openaiToClaudeRequest("claude-opus-4-6", json.RawMessage(body), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	tc := asMap(t, v, "tool_choice")
	if tc["type"] != "any" {
		t.Errorf("tool_choice = %v, want type=any", tc)
	}
}

func TestOpenAIToClaudeRequestAdaptiveThinking(t *testing.T) {
	// claude-opus-4-6 supports adaptive thinking; reasoning_effort → thinking + output_config.
	body := `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`
	out, err := openaiToClaudeRequest("claude-opus-4-6", json.RawMessage(body), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	thinking := asMap(t, v, "thinking")
	if thinking["type"] != "adaptive" {
		t.Errorf("thinking = %v, want type=adaptive", thinking)
	}
	oc := asMap(t, v, "output_config")
	if oc["effort"] != "high" {
		t.Errorf("output_config effort = %v, want high", oc["effort"])
	}
}

func TestOpenAIToClaudeRequestNonAdaptiveModelIgnoresEffort(t *testing.T) {
	// claude-3-5-sonnet does NOT support adaptive thinking → no thinking field.
	body := `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`
	out, err := openaiToClaudeRequest("claude-3-5-sonnet", json.RawMessage(body), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	if _, ok := v["thinking"]; ok {
		t.Errorf("non-adaptive model should not get thinking field")
	}
}

func TestOpenAIToClaudeRequestJSONSchemaResponseFormat(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"schema":{"type":"object","properties":{"x":{"type":"string"}}}}}}`
	out, err := openaiToClaudeRequest("claude-opus-4-6", json.RawMessage(body), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	sys := asArray(t, v, "system")
	// The schema prompt must be appended to the system blocks.
	var foundSchema bool
	for _, s := range sys {
		if txt, _ := s.(map[string]any)["text"].(string); strings.Contains(txt, "JSON schema") {
			foundSchema = true
		}
	}
	if !foundSchema {
		t.Errorf("json_schema response_format did not produce a schema system prompt")
	}
}

func TestOpenAIToClaudeRequestJSONObjectResponseFormat(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`
	out, err := openaiToClaudeRequest("claude-opus-4-6", json.RawMessage(body), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	sys := asArray(t, v, "system")
	var foundJSONPrompt bool
	for _, s := range sys {
		if txt, _ := s.(map[string]any)["text"].(string); strings.Contains(txt, "respond with valid JSON") {
			foundJSONPrompt = true
		}
	}
	if !foundJSONPrompt {
		t.Errorf("json_object response_format did not produce a JSON system prompt")
	}
}

func TestOpenAIToClaudeRequestImageURLDataURI(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}]}`
	out, err := openaiToClaudeRequest("claude-opus-4-6", json.RawMessage(body), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	msgs := asArray(t, v, "messages")
	m := msgs[0].(map[string]any)
	content := m["content"].([]any)
	img := content[0].(map[string]any)
	if img["type"] != "image" {
		t.Fatalf("block type = %v, want image", img["type"])
	}
	src := img["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "QUJD" {
		t.Errorf("image source = %+v, want base64/image/png/QUJD", src)
	}
}

func TestOpenAIToClaudeRequestImageURLHttp(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]}]}`
	out, err := openaiToClaudeRequest("claude-opus-4-6", json.RawMessage(body), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	msgs := asArray(t, v, "messages")
	m := msgs[0].(map[string]any)
	content := m["content"].([]any)
	img := content[0].(map[string]any)
	src := img["source"].(map[string]any)
	if src["type"] != "url" || src["url"] != "https://example.com/x.png" {
		t.Errorf("image source = %+v, want url type", src)
	}
}

func TestOpenAIToClaudeRequestAssistantReasoningContent(t *testing.T) {
	// assistant with reasoning_content → a thinking block is prepended.
	body := `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok","reasoning_content":"let me think"}]}`
	out, err := openaiToClaudeRequest("claude-opus-4-6", json.RawMessage(body), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	msgs := asArray(t, v, "messages")
	// find the assistant message
	var asst map[string]any
	for _, mm := range msgs {
		if mm.(map[string]any)["role"] == "assistant" {
			asst = mm.(map[string]any)
		}
	}
	if asst == nil {
		t.Fatal("no assistant message")
	}
	content := asst["content"].([]any)
	first := content[0].(map[string]any)
	if first["type"] != "thinking" || first["thinking"] != "let me think" {
		t.Errorf("first assistant block = %+v, want thinking/let me think", first)
	}
}

func TestOpenAIToClaudeRequestInvalidJSON(t *testing.T) {
	_, err := openaiToClaudeRequest("m", json.RawMessage(`{bad`), false)
	if err == nil {
		t.Fatal("invalid JSON should return error")
	}
}

// --- openaiToOpenaiResponsesRequest ---

func TestOpenAIToOpenAIResponsesRequestBasic(t *testing.T) {
	body := `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`
	out, err := openaiToOpenaiResponsesRequest("gpt-4o", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	if v["model"] != "gpt-4o" {
		t.Errorf("model = %v", v["model"])
	}
	if v["stream"] != true {
		t.Errorf("stream = %v, want true", v["stream"])
	}
	if v["instructions"] != "sys" {
		t.Errorf("instructions = %v, want sys", v["instructions"])
	}
	input := asArray(t, v, "input")
	if len(input) != 1 {
		t.Fatalf("input len = %d, want 1 (user msg; system extracted)", len(input))
	}
	item := input[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "user" {
		t.Errorf("input item = %+v, want message/user", item)
	}
}

func TestOpenAIToOpenAIResponsesRequestInputPassthrough(t *testing.T) {
	// If body already has "input", it is shallow-copied through with model+stream.
	body := `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	out, err := openaiToOpenaiResponsesRequest("gpt-4o", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	if v["model"] != "gpt-4o" {
		t.Errorf("model = %v", v["model"])
	}
	if _, ok := v["input"]; !ok {
		t.Errorf("input passthrough lost input field")
	}
}

func TestOpenAIToOpenAIResponsesRequestAssistantToolCall(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`
	out, err := openaiToOpenaiResponsesRequest("gpt-4o", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	input := asArray(t, v, "input")
	var foundFunctionCall bool
	for _, it := range input {
		if it.(map[string]any)["type"] == "function_call" {
			foundFunctionCall = true
			if it.(map[string]any)["name"] != "f" {
				t.Errorf("function_call name = %v, want f", it.(map[string]any)["name"])
			}
		}
	}
	if !foundFunctionCall {
		t.Errorf("no function_call input item emitted from assistant tool_calls")
	}
}

func TestOpenAIToOpenAIResponsesRequestToolMessage(t *testing.T) {
	// role=tool is translated to a function_call_output input item for the
	// Responses API (matching the assistant's preceding function_call by
	// tool_call_id). The assistant's tool_call is emitted as a function_call
	// item. Previously the role=tool message was dropped because the filter
	// `if role != roleUser && role != roleAssistant { continue }` ran before
	// the roleTool branch — fixed by hoisting the roleTool branch above the
	// filter.
	body := `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"result"}]}`
	out, err := openaiToOpenaiResponsesRequest("gpt-4o", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	input := asArray(t, v, "input")
	var foundFunctionCall bool
	var fcOutputItem map[string]any
	for _, it := range input {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		if typ == "function_call" {
			foundFunctionCall = true
		}
		if typ == "function_call_output" {
			fcOutputItem = m
		}
	}
	if !foundFunctionCall {
		t.Errorf("assistant tool_calls should emit a function_call input item; input=%v", input)
	}
	if fcOutputItem == nil {
		t.Fatalf("role=tool should emit a function_call_output item; input=%v", input)
	}
	if fcOutputItem["call_id"] != "c1" {
		t.Errorf("function_call_output call_id = %v, want c1 (matches tool_call_id)", fcOutputItem["call_id"])
	}
	if fcOutputItem["output"] != "result" {
		t.Errorf("function_call_output output = %v, want \"result\"", fcOutputItem["output"])
	}
}

func TestOpenAIToOpenAIResponsesRequestReasoningEffort(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`
	out, err := openaiToOpenaiResponsesRequest("gpt-4o", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	r := asMap(t, v, "reasoning")
	if r["effort"] != "high" || r["summary"] != "auto" {
		t.Errorf("reasoning = %v, want effort=high summary=auto", r)
	}
}

// --- openaiResponsesToOpenaiRequest ---

func TestOpenAIResponsesToOpenAIRequestBasic(t *testing.T) {
	body := `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	out, err := openaiResponsesToOpenaiRequest("gpt-4o", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	msgs := asArray(t, v, "messages")
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "user" {
		t.Errorf("role = %v, want user", m["role"])
	}
	// input_text content is normalized to openai text blocks.
	content := m["content"].([]any)
	if content[0].(map[string]any)["type"] != "text" {
		t.Errorf("content type = %v, want text", content[0].(map[string]any)["type"])
	}
}

func TestOpenAIResponsesToOpenAIRequestInstructionsToSystem(t *testing.T) {
	body := `{"instructions":"be brief","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	out, err := openaiResponsesToOpenaiRequest("gpt-4o", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	msgs := asArray(t, v, "messages")
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("first message = %v, want system", msgs[0].(map[string]any)["role"])
	}
	if msgs[0].(map[string]any)["content"] != "be brief" {
		t.Errorf("system content = %v, want 'be brief'", msgs[0].(map[string]any)["content"])
	}
}

func TestOpenAIResponsesToOpenAIRequestNoInputPassthrough(t *testing.T) {
	// No "input" field → passthrough raw body unchanged.
	body := `{"max_tokens":100}`
	out, err := openaiResponsesToOpenaiRequest("gpt-4o", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(out) != body {
		t.Errorf("no-input passthrough = %s, want %s", out, body)
	}
}

func TestOpenAIResponsesToOpenAIRequestMaxOutputTokensMapped(t *testing.T) {
	body := `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"max_output_tokens":500}`
	out, err := openaiResponsesToOpenaiRequest("gpt-4o", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	if v["max_tokens"] != float64(500) {
		t.Errorf("max_tokens = %v, want 500 (from max_output_tokens)", v["max_tokens"])
	}
	if _, ok := v["max_output_tokens"]; ok {
		t.Errorf("max_output_tokens should be deleted after mapping")
	}
}

func TestOpenAIResponsesToOpenAIRequestFunctionCallItem(t *testing.T) {
	body := `{"input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"x\":1}"}]}`
	out, err := openaiResponsesToOpenaiRequest("gpt-4o", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	msgs := asArray(t, v, "messages")
	var foundToolCalls bool
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "assistant" {
			tcs, ok := mm["tool_calls"].([]any)
			if ok && len(tcs) > 0 {
				foundToolCalls = true
				tc := tcs[0].(map[string]any)
				fn := tc["function"].(map[string]any)
				if fn["name"] != "f" {
					t.Errorf("tool_call name = %v, want f", fn["name"])
				}
			}
		}
	}
	if !foundToolCalls {
		t.Errorf("function_call input item did not produce assistant tool_calls")
	}
}

func TestOpenAIResponsesToOpenAIRequestFunctionCallOutputItem(t *testing.T) {
	body := `{"input":[{"type":"function_call_output","call_id":"c1","output":"result"}]}`
	out, err := openaiResponsesToOpenaiRequest("gpt-4o", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	msgs := asArray(t, v, "messages")
	var foundTool bool
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "tool" {
			foundTool = true
			if mm["tool_call_id"] != "c1" || mm["content"] != "result" {
				t.Errorf("tool msg = %+v, want call_id=c1 content=result", mm)
			}
		}
	}
	if !foundTool {
		t.Errorf("function_call_output input item did not produce a tool message")
	}
}

func TestOpenAIResponsesToOpenAIRequestReasoningItem(t *testing.T) {
	// reasoning item with summary → attached to the NEXT assistant message as
	// reasoning_content. Here a message follows.
	body := `{"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking..."}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`
	out, err := openaiResponsesToOpenaiRequest("gpt-4o", json.RawMessage(body), true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v := mustUnmarshal(t, out)
	msgs := asArray(t, v, "messages")
	var foundReasoning bool
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "assistant" {
			if rc, _ := mm["reasoning_content"].(string); rc == "thinking..." {
				foundReasoning = true
			}
		}
	}
	if !foundReasoning {
		t.Errorf("reasoning summary not attached to following assistant message")
	}
}

// --- helpers: shallowCopyMap / toString / marshalJSONString / normalizeToolParameters / normalizeResponsesInput ---

func TestShallowCopyMap(t *testing.T) {
	src := map[string]any{"a": float64(1), "b": "x"}
	dst := shallowCopyMap(src)
	if len(dst) != 2 || dst["a"] != float64(1) || dst["b"] != "x" {
		t.Fatalf("shallowCopy = %+v", dst)
	}
	dst["c"] = float64(3)
	if _, ok := src["c"]; ok {
		t.Errorf("shallow copy leaked into source")
	}
}

func TestToString(t *testing.T) {
	if toString(nil) != "" {
		t.Errorf("nil → %q, want empty", toString(nil))
	}
	if toString("abc") != "abc" {
		t.Errorf("string → %q", toString("abc"))
	}
	if toString(123) != "123" {
		t.Errorf("int → %q, want 123", toString(123))
	}
}

func TestMarshalJSONString(t *testing.T) {
	if marshalJSONString(map[string]any{"a": 1}) != `{"a":1}` {
		t.Errorf("marshalJSONString map = %q", marshalJSONString(map[string]any{"a": 1}))
	}
}

func TestNormalizeToolParameters(t *testing.T) {
	// nil → default object with empty properties.
	got := normalizeToolParameters(nil)
	m := got.(map[string]any)
	if m["type"] != "object" {
		t.Errorf("nil params type = %v, want object", m["type"])
	}
	// object without properties → adds empty properties.
	got = normalizeToolParameters(map[string]any{"type": "object"})
	m = got.(map[string]any)
	if _, ok := m["properties"]; !ok {
		t.Errorf("object params missing properties not filled")
	}
	// object with properties → passthrough (returns the same map instance).
	in := map[string]any{"type": "object", "properties": map[string]any{"a": float64(1)}}
	out := normalizeToolParameters(in).(map[string]any)
	if out["type"] != "object" {
		t.Errorf("passthrough type = %v, want object", out["type"])
	}
	props, _ := out["properties"].(map[string]any)
	if props["a"] != float64(1) {
		t.Errorf("passthrough properties = %v, want {a:1}", props)
	}
	// non-map passthrough.
	if normalizeToolParameters("x") != "x" {
		t.Errorf("non-map should passthrough")
	}
}

func TestNormalizeResponsesInput(t *testing.T) {
	// string → single user message with input_text.
	got := normalizeResponsesInput("hello")
	if len(got) != 1 {
		t.Fatalf("string input len = %d, want 1", len(got))
	}
	// empty/whitespace string → "..." placeholder.
	got = normalizeResponsesInput("  ")
	item := got[0].(map[string]any)
	content := item["content"].([]any)
	txt := content[0].(map[string]any)["text"]
	if txt != "..." {
		t.Errorf("whitespace input text = %v, want ...", txt)
	}
	// empty array → placeholder user message.
	got = normalizeResponsesInput([]any{})
	if len(got) != 1 {
		t.Fatalf("empty array len = %d, want 1", len(got))
	}
	// non-empty array → passthrough (same slice elements).
	in := []any{map[string]any{"type": "message"}}
	got = normalizeResponsesInput(in)
	if len(got) != 1 {
		t.Fatalf("non-empty array passthrough len = %d, want 1", len(got))
	}
	if t0, _ := got[0].(map[string]any)["type"].(string); t0 != "message" {
		t.Errorf("non-empty array passthrough item = %v, want message", got[0])
	}
	// nil/other → nil.
	if normalizeResponsesInput(123) != nil {
		t.Errorf("non-string/array should return nil")
	}
}

// --- getContentBlocksFromMessage edge cases ---

func TestGetContentBlocksFromMessageToolRole(t *testing.T) {
	msg := map[string]any{"role": "tool", "tool_call_id": "c1", "content": "result"}
	blocks := getContentBlocksFromMessage(msg)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	if blocks[0]["type"] != "tool_result" || blocks[0]["tool_use_id"] != "c1" {
		t.Errorf("tool block = %+v, want tool_result/c1", blocks[0])
	}
}

func TestGetContentBlocksFromMessageUserStringEmpty(t *testing.T) {
	// Empty user string content → no blocks (not an empty text block).
	msg := map[string]any{"role": "user", "content": ""}
	blocks := getContentBlocksFromMessage(msg)
	if len(blocks) != 0 {
		t.Errorf("empty user string should produce 0 blocks, got %d", len(blocks))
	}
}

func TestGetContentBlocksFromMessageAssistantThinkingBlock(t *testing.T) {
	msg := map[string]any{
		"role":    "assistant",
		"content": []any{},
		"content_thinking": []any{
			map[string]any{"type": "thinking", "thinking": "hmm", "signature": "sig"},
		},
	}
	// The assistant branch reads content via the []any case; a thinking block
	// inside content is mapped. Put it in content instead to exercise that path.
	msg["content"] = []any{
		map[string]any{"type": "thinking", "thinking": "hmm", "signature": "sig"},
	}
	blocks := getContentBlocksFromMessage(msg)
	var found bool
	for _, b := range blocks {
		if b["type"] == "thinking" && b["thinking"] == "hmm" && b["signature"] == "sig" {
			found = true
		}
	}
	if !found {
		t.Errorf("thinking block not mapped from assistant content")
	}
}