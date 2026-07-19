package http

import "testing"

func TestEstimateAnthropicInputTokens_SimpleString(t *testing.T) {
	body := []byte(`{"system":"hello","messages":[{"role":"user","content":"hi there"}]}`)
	// system=5 ("hello") + content=8 ("hi there") = 13 chars -> ceil(13/4)=4
	got := estimateAnthropicInputTokens(body)
	if got != 4 {
		t.Errorf("estimate = %d, want 4", got)
	}
}

func TestEstimateAnthropicInputTokens_ContentBlocks(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":[{"type":"text","text":"abcd"}]}
		]
	}`)
	// "abcd"=4 -> ceil(4/4)=1
	got := estimateAnthropicInputTokens(body)
	if got != 1 {
		t.Errorf("estimate = %d, want 1", got)
	}
}

func TestEstimateAnthropicInputTokens_ToolUseBlock(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"tool_use","name":"get_weather","input":{"city":"SF"}}
	]}]}`)
	// name="get_weather"=11 + input object: key "city"=4 + value "SF"=2 = 6 -> 17 -> ceil(17/4)=5
	got := estimateAnthropicInputTokens(body)
	if got != 5 {
		t.Errorf("estimate = %d, want 5", got)
	}
}

func TestEstimateAnthropicInputTokens_Empty(t *testing.T) {
	got := estimateAnthropicInputTokens([]byte(`{}`))
	if got != 0 {
		t.Errorf("empty estimate = %d, want 0", got)
	}
}

func TestEstimateAnthropicInputTokens_InvalidJSON(t *testing.T) {
	got := estimateAnthropicInputTokens([]byte(`not json`))
	if got != 0 {
		t.Errorf("invalid estimate = %d, want 0", got)
	}
}

func TestEstimateAnthropicInputTokens_SystemArray(t *testing.T) {
	// system as array of blocks sums the text fields.
	body := []byte(`{"system":[{"type":"text","text":"abcdefgh"}],"messages":[]}`)
	// "abcdefgh"=8 -> ceil(8/4)=2. Object walk: key "type"=4 + "text"=4 +
	// value "text"(4) + value "abcdefgh"(8) = 20. JS walks the system object
	// via countValueChars too, so total = object walk (20) + ... Actually JS
	// countValueChars(system) where system is an array -> sum of elements,
	// each element is an object -> key+value sum. So 20 -> ceil(20/4)=5.
	got := estimateAnthropicInputTokens(body)
	if got != 5 {
		t.Errorf("system-array estimate = %d, want 5", got)
	}
}