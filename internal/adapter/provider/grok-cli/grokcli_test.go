package grokcliexec

// grokcli_test.go pins the Grok Build subscription protocol port (upstream
// 59b78282): the chat-path headers (session/conv/req/turn/agent/model/email/
// userid), the reasoning.effort model gate (grok-build does NOT get effort but
// still gets encrypted_content; grok-4.5 does), the per-session turn index
// monotonicity + TTL + LRU, and the item/tool normalization + allowlist filter.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/provider/base"
	domain "github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

func newExec() *Executor {
	return New(base.Config{
		ID:      "grok-cli",
		BaseURL: "https://cli-chat-proxy.grok.com/v1/responses",
		Format:  "openai-responses",
	})
}

func credsWith(psd map[string]any) domain.Credentials {
	return domain.Credentials{AccessToken: "tok", ProviderSpecificData: psd}
}

func transform(t *testing.T, e *Executor, model string, body map[string]any, psd map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	out, err := e.TransformRequest(model, raw, true, credsWith(psd))
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// TestBuildHeaders_EmitsProtocolHeaders pins the 59b78282 subscription headers:
// session/conv share one id, req-id is present, turn-idx is ≥1, and
// agent-id/model-override/email/userid flow through from psd.
func TestBuildHeaders_EmitsProtocolHeaders(t *testing.T) {
	e := newExec()
	e.currentSessionID = "sess-1"
	e.currentReqID = "req-1"
	e.currentTurnIdx = 7
	e.currentAgentID = "agent-1"
	e.currentModel = "grok-build"
	h := e.BuildHeaders(credsWith(map[string]any{
		"email":  "u@x.ai",
		"userId": "u-123",
	}), true)

	if got := h.Get("x-grok-session-id"); got != "sess-1" {
		t.Errorf("x-grok-session-id = %q, want sess-1", got)
	}
	if got := h.Get("x-grok-conv-id"); got != "sess-1" {
		t.Errorf("x-grok-conv-id = %q, want sess-1 (same as session-id)", got)
	}
	if got := h.Get("x-grok-req-id"); got != "req-1" {
		t.Errorf("x-grok-req-id = %q, want req-1", got)
	}
	if got := h.Get("x-grok-turn-idx"); got != "7" {
		t.Errorf("x-grok-turn-idx = %q, want 7", got)
	}
	if got := h.Get("x-grok-agent-id"); got != "agent-1" {
		t.Errorf("x-grok-agent-id = %q, want agent-1", got)
	}
	if got := h.Get("x-grok-model-override"); got != "grok-build" {
		t.Errorf("x-grok-model-override = %q, want grok-build", got)
	}
	if got := h.Get("x-email"); got != "u@x.ai" {
		t.Errorf("x-email = %q", got)
	}
	if got := h.Get("x-userid"); got != "u-123" {
		t.Errorf("x-userid = %q", got)
	}
	// The retired 59b78282 headers must not be present.
	if h.Get("x-authenticateresponse") != "" {
		t.Error("x-authenticateresponse should be absent (removed in 59b78282)")
	}
	if h.Get("x-compaction-at") != "" {
		t.Error("x-compaction-at should be absent (removed in 59b78282)")
	}
}

// TestBuildHeaders_FallbacksWhenTransformNotRun pins that a direct headers probe
// (no TransformRequest first) still emits non-empty session/req ids + turn≥1,
// so the protocol header set is never empty even on a bare probe.
func TestBuildHeaders_FallbacksWhenTransformNotRun(t *testing.T) {
	e := newExec()
	h := e.BuildHeaders(credsWith(nil), true)
	if h.Get("x-grok-session-id") == "" {
		t.Error("x-grok-session-id empty without TransformRequest")
	}
	if h.Get("x-grok-conv-id") == "" {
		t.Error("x-grok-conv-id empty without TransformRequest")
	}
	if h.Get("x-grok-req-id") == "" {
		t.Error("x-grok-req-id empty without TransformRequest")
	}
	if got := h.Get("x-grok-turn-idx"); got != "1" {
		t.Errorf("x-grok-turn-idx = %q, want 1 fallback", got)
	}
}

// TestReasoningGate_GrokBuildNoEffort pins the critical grok-build fix: the
// model does not accept reasoning.effort, so effort is deleted — but
// encrypted_content is still requested because reasoning is present and effort
// is not "none" (the 59b78282 condition change).
func TestReasoningGate_GrokBuildNoEffort(t *testing.T) {
	e := newExec()
	m := transform(t, e, "grok-build", map[string]any{
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	}, nil)
	r, _ := m["reasoning"].(map[string]any)
	if _, ok := r["effort"]; ok {
		t.Errorf("grok-build must not carry reasoning.effort; got %v", r["effort"])
	}
	if r["summary"] != "concise" {
		t.Errorf("summary = %v, want concise", r["summary"])
	}
	inc, _ := m["include"].([]any)
	found := false
	for _, v := range inc {
		if v == "reasoning.encrypted_content" {
			found = true
		}
	}
	if !found {
		t.Error("grok-build must request reasoning.encrypted_content even without effort")
	}
}

// TestReasoningGate_Grok45GetsEffort pins the converse: grok-4.5* does accept
// reasoning.effort, normalized through max→xhigh + unknown→high.
func TestReasoningGate_Grok45GetsEffort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"max", "xhigh"},
		{"high", "high"},
		{"bogus", "high"},
		{"low", "low"},
	}
	for _, c := range cases {
		e := newExec()
		m := transform(t, e, "grok-4.5", map[string]any{
			"input":            []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
			"reasoning_effort": c.in,
		}, nil)
		r, _ := m["reasoning"].(map[string]any)
		if got, _ := r["effort"].(string); got != c.want {
			t.Errorf("grok-4.5 effort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// The effort suffix on the model id is stripped + applied.
	e := newExec()
	m := transform(t, e, "grok-4.5-high", map[string]any{
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	}, nil)
	if got, _ := m["model"].(string); got != "grok-4.5" {
		t.Errorf("model = %q, want grok-4.5 (suffix stripped)", got)
	}
	r, _ := m["reasoning"].(map[string]any)
	if got, _ := r["effort"].(string); got != "high" {
		t.Errorf("effort from suffix = %q, want high", got)
	}
}

// TestTurnIdx_MonotonicAcrossSession pins the per-session turn store: a second
// user turn for the same session id increments past the first; a fresh session
// resets to the input-derived count.
func TestTurnIdx_MonotonicAcrossSession(t *testing.T) {
	resetGrokCliTurnStore()
	defer resetGrokCliTurnStore()
	e := newExec()
	psd := map[string]any{"sessionId": "sess-turn"}
	input1 := []any{map[string]any{"type": "message", "role": "user", "content": "a"}}
	transform(t, e, "grok-build", map[string]any{"input": input1}, psd)
	if e.currentTurnIdx != 1 {
		t.Fatalf("first turn = %d, want 1", e.currentTurnIdx)
	}
	// Second user turn in the same session → prev+1.
	transform(t, e, "grok-build", map[string]any{"input": input1}, psd)
	if e.currentTurnIdx != 2 {
		t.Errorf("second turn = %d, want 2 (prev+1 delta)", e.currentTurnIdx)
	}
	// A 3-message input in the same session → max(fromInput=3, prev+1=3) = 3.
	input3 := []any{
		map[string]any{"type": "message", "role": "user", "content": "a"},
		map[string]any{"type": "message", "role": "user", "content": "b"},
		map[string]any{"type": "message", "role": "user", "content": "c"},
	}
	transform(t, e, "grok-build", map[string]any{"input": input3}, psd)
	if e.currentTurnIdx != 3 {
		t.Errorf("third turn = %d, want 3 (max(fromInput, prev+1))", e.currentTurnIdx)
	}
	// Fresh session → input-derived count (1), not prev+1.
	e2 := newExec()
	transform(t, e2, "grok-build", map[string]any{"input": input1}, map[string]any{"sessionId": "sess-fresh"})
	if e2.currentTurnIdx != 1 {
		t.Errorf("fresh session turn = %d, want 1", e2.currentTurnIdx)
	}
}

// TestTurnIdx_LRUEviction pins the grokCliTurnStoreMax cap: inserting more
// sessions than the cap evicts the oldest so the store stays bounded.
func TestTurnIdx_LRUEviction(t *testing.T) {
	// Lower the effective cap by filling past it with distinct sessions.
	resetGrokCliTurnStore()
	defer resetGrokCliTurnStore()
	e := newExec()
	for i := 0; i < grokCliTurnStoreMax+10; i++ {
		psd := map[string]any{"sessionId": "s-" + itoa(i)}
		transform(t, e, "grok-build", map[string]any{
			"input": []any{map[string]any{"type": "message", "role": "user", "content": "x"}},
		}, psd)
	}
	if got := grokCliTurnStoreSize(); got > grokCliTurnStoreMax {
		t.Errorf("turn store size = %d, want ≤ %d (LRU bound)", got, grokCliTurnStoreMax)
	}
}

// TestNormalizeGrokCliInput_ToolCallRoundtrip pins the item-type normalization:
// a custom_tool_call + custom_tool_call_output pair is rewritten to
// function_call / function_call_output, and an orphan output (no matching
// function_call) is dropped.
func TestNormalizeGrokCliInput_ToolCallRoundtrip(t *testing.T) {
	body := map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "do thing"},
			map[string]any{"type": "custom_tool_call", "call_id": "c1", "name": "search", "input": "q"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "c1", "output": "result"},
			// Orphan output with no matching function_call → dropped.
			map[string]any{"type": "function_call_output", "call_id": "orphan", "output": "x"},
			// Non-native reasoning item without encrypted_content → dropped.
			map[string]any{"type": "reasoning", "id": "rs_notnative", "encrypted_content": ""},
		},
	}
	normalizeGrokCliInput(body)
	arr, _ := body["input"].([]any)
	if len(arr) != 3 {
		t.Fatalf("input len = %d, want 3 (user + function_call + function_call_output)", len(arr))
	}
	fc, _ := arr[1].(map[string]any)
	if fc["type"] != "function_call" {
		t.Errorf("custom_tool_call normalized to %v, want function_call", fc["type"])
	}
	if fc["name"] != "search" {
		t.Errorf("function_call name = %v", fc["name"])
	}
	if _, ok := fc["arguments"]; !ok {
		t.Error("function_call missing arguments")
	}
	fco, _ := arr[2].(map[string]any)
	if fco["type"] != "function_call_output" {
		t.Errorf("custom_tool_call_output normalized to %v, want function_call_output", fco["type"])
	}
}

// TestNormalizeGrokCliTools_CustomFreeform pins the type:"custom" tool rewrite:
// the freeform parameters are substituted, the name is capped at 128, and
// tool_choice type:"custom" is remapped to {type:"function",name}.
func TestNormalizeGrokCliTools_CustomFreeform(t *testing.T) {
	longName := strings.Repeat("n", 200)
	body := map[string]any{
		"tools": []any{
			map[string]any{"type": "custom", "name": longName},
			map[string]any{"type": "web_search"}, // hosted, passthrough
		},
		"tool_choice": map[string]any{"type": "custom", "name": longName},
	}
	normalizeGrokCliTools(body)
	tools, _ := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools len = %d, want 2 (function + hosted)", len(tools))
	}
	fn, _ := tools[0].(map[string]any)
	if fn["type"] != "function" {
		t.Errorf("custom tool type = %v, want function", fn["type"])
	}
	if name, _ := fn["name"].(string); len(name) != 128 {
		t.Errorf("custom tool name len = %d, want 128 (truncated)", len(name))
	}
	params, _ := fn["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Errorf("freeform parameters type = %v, want object", params["type"])
	}
	tc, _ := body["tool_choice"].(map[string]any)
	if tc["type"] != "function" {
		t.Errorf("tool_choice type = %v, want function (remapped from custom)", tc["type"])
	}
}

// TestStripStoredItemReferences pins that server-prefix string items +
// item_reference entries are dropped, and a non-native server-prefix id is
// stripped from surviving items (a native rs_/msg_/fc_ UUID id is kept).
func TestStripStoredItemReferences(t *testing.T) {
	nativeID := "rs_12345678-1234-1234-1234-123456789012"
	body := map[string]any{
		"input": []any{
			"rs_garbage-id",                          // string server-prefix → dropped
			map[string]any{"type": "item_reference"}, // dropped
			map[string]any{"type": "message", "role": "assistant", "id": "resp_abc", "content": "x"}, // id stripped
			map[string]any{"type": "message", "role": "assistant", "id": nativeID, "content": "y"},   // native id kept
		},
	}
	stripStoredItemReferences(body)
	arr, _ := body["input"].([]any)
	if len(arr) != 2 {
		t.Fatalf("input len = %d, want 2 (string + item_reference dropped)", len(arr))
	}
	first, _ := arr[0].(map[string]any)
	if _, ok := first["id"]; ok {
		t.Errorf("non-native resp_ id should be stripped; got %v", first["id"])
	}
	second, _ := arr[1].(map[string]any)
	if second["id"] != nativeID {
		t.Errorf("native id = %v, want kept %s", second["id"], nativeID)
	}
}

// TestApplyResponsesAllowlist pins that the final filter drops every key the
// Responses API does not accept (messages, max_tokens, user, …) while keeping
// the allowlisted body fields.
func TestApplyResponsesAllowlist(t *testing.T) {
	body := map[string]any{
		"model":                  "grok-build",
		"input":                  []any{},
		"messages":               []any{},
		"max_tokens":             1000,
		"user":                   "u",
		"reasoning":              map[string]any{"summary": "concise"},
		"prompt_cache_retention": map[string]any{},
		"temperature":            0.7,
	}
	applyResponsesAllowlist(body)
	for _, banned := range []string{"messages", "max_tokens", "user", "prompt_cache_retention"} {
		if _, ok := body[banned]; ok {
			t.Errorf("allowlist leaked banned key %q", banned)
		}
	}
	for _, kept := range []string{"model", "input", "reasoning", "temperature"} {
		if _, ok := body[kept]; !ok {
			t.Errorf("allowlist dropped kept key %q", kept)
		}
	}
}

// TestResolveGrokCliSessionID pins the session-id resolution order: explicit
// body field > metadata field > psd connectionId/workspaceId > fresh uuid.
func TestResolveGrokCliSessionID(t *testing.T) {
	if got := resolveGrokCliSessionID(map[string]any{"session_id": "s1"}, nil); got != "s1" {
		t.Errorf("session_id body field = %q", got)
	}
	if got := resolveGrokCliSessionID(map[string]any{"metadata": map[string]any{"conversation_id": "c1"}}, nil); got != "c1" {
		t.Errorf("metadata conversation_id = %q", got)
	}
	if got := resolveGrokCliSessionID(map[string]any{}, map[string]any{"connectionId": "conn-1"}); got != "conn-1" {
		t.Errorf("psd connectionId = %q", got)
	}
	if got := resolveGrokCliSessionID(map[string]any{}, nil); got == "" {
		t.Error("empty body+psd should mint a fresh uuid, not empty")
	}
}

// TestResolveGrokCliAgentID pins the agent-id fallback: a connection-bound
// deviceId wins, otherwise the process-stable id is returned.
func TestResolveGrokCliAgentID(t *testing.T) {
	if got := resolveGrokCliAgentID(map[string]any{"deviceId": "dev-1"}); got != "dev-1" {
		t.Errorf("deviceId psd = %q, want dev-1", got)
	}
	if got := resolveGrokCliAgentID(nil); got == "" {
		t.Error("empty psd should fall back to process-stable agent id, not empty")
	}
	// Process-stable id must be a v5-ish UUID (version nibble 5, variant a).
	stable := resolveGrokCliAgentID(nil)
	if len(stable) != 36 || stable[14] != '5' || stable[19] != 'a' {
		t.Errorf("process-stable agent id = %q, want v5-ish UUID shape", stable)
	}
}
