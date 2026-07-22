package cursorexec

// agent_test.go pins the AgentService protobuf codec port (upstream v0.5.40,
// commit 6994cd1f). Pure codec, no network.

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"testing"
)

// decodeOneConnectFrame strips the 5-byte Connect header and returns the
// payload, mirroring what a server-side decoder would do.
func decodeOneConnectFrame(t *testing.T, frame []byte) []byte {
	t.Helper()
	if len(frame) < 5 {
		t.Fatalf("frame too short: %d", len(frame))
	}
	length := int(binary.BigEndian.Uint32(frame[1:5]))
	if len(frame) < 5+length {
		t.Fatalf("incomplete frame: want %d have %d", 5+length, len(frame))
	}
	return frame[5 : 5+length]
}

func TestBuildAgentRunFrame_ContainsUserMessageAndModel(t *testing.T) {
	old := randomAgentUUID
	randomAgentUUID = func() string { return "11111111-1111-1111-1111-111111111111" }
	defer func() { randomAgentUUID = old }()

	frame := BuildAgentRunFrame([]AgentMessage{
		{Role: "user", Content: "Hello there"},
	}, "gpt-5.3-codex")

	payload := decodeOneConnectFrame(t, frame)
	// AgentClientMessage.run_request is field 1.
	clientMsg := decodeMessage(payload)
	runReq := clientMsg.first(1)
	if runReq == nil {
		t.Fatal("missing run_request field 1")
	}
	rr := decodeMessage(runReq)
	// field 2 = conversation_action, field 9 = requested_model.
	if rr.first(2) == nil {
		t.Error("missing conversation_action field 2")
	}
	if rr.first(9) == nil {
		t.Error("missing requested_model field 9")
	}
	// requested_model: field 1 = model string.
	modelMsg := decodeMessage(rr.first(9))
	if string(modelMsg.first(1)) != "gpt-5.3-codex" {
		t.Errorf("model=%q want gpt-5.3-codex", string(modelMsg.first(1)))
	}
}

func TestBuildAgentRunFrame_SystemPromptFoldedToField8(t *testing.T) {
	old := randomAgentUUID
	randomAgentUUID = func() string { return "id" }
	defer func() { randomAgentUUID = old }()

	frame := BuildAgentRunFrame([]AgentMessage{
		{Role: "system", Content: "You are helpful"},
		{Role: "user", Content: "hi"},
	}, "m")
	payload := decodeOneConnectFrame(t, frame)
	rr := decodeMessage(decodeMessage(payload).first(1))
	if sys := rr.first(8); sys == nil || string(sys) != "You are helpful" {
		t.Errorf("system field 8 = %q want %q", sys, "You are helpful")
	}
}

func TestBuildAgentRunFrame_HistoryEncoded(t *testing.T) {
	old := randomAgentUUID
	randomAgentUUID = func() string { return "id" }
	defer func() { randomAgentUUID = old }()

	// user, assistant, user → history = [user, assistant], current = last user.
	frame := BuildAgentRunFrame([]AgentMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply"},
		{Role: "user", Content: "second"},
	}, "m")
	payload := decodeOneConnectFrame(t, frame)
	rr := decodeMessage(decodeMessage(payload).first(1))
	action := decodeMessage(rr.first(2))
	userAction := decodeMessage(action.first(1))
	// current user_message text must be "second".
	userMessage := decodeMessage(userAction.first(1))
	if string(userMessage.first(1)) != "second" {
		t.Errorf("current text=%q want second", string(userMessage.first(1)))
	}
	// history field 7 present with 2 entries.
	hist := userAction.first(7)
	if hist == nil {
		t.Fatal("missing history field 7")
	}
	histMsg := decodeMessage(hist)
	if len(histMsg[1]) != 2 {
		t.Errorf("history entries=%d want 2", len(histMsg[1]))
	}
}

func TestBuildAgentRunFrame_EmptyContentFallsBackToContinue(t *testing.T) {
	old := randomAgentUUID
	randomAgentUUID = func() string { return "id" }
	defer func() { randomAgentUUID = old }()

	// A user message with no extractable content → "Continue." fallback.
	frame := BuildAgentRunFrame([]AgentMessage{
		{Role: "user", Content: []any{}},
	}, "m")
	payload := decodeOneConnectFrame(t, frame)
	rr := decodeMessage(decodeMessage(payload).first(1))
	action := decodeMessage(rr.first(2))
	userMessage := decodeMessage(decodeMessage(action.first(1)).first(1))
	if string(userMessage.first(1)) != "Continue." {
		t.Errorf("empty-content fallback=%q want Continue.", string(userMessage.first(1)))
	}
}

func TestIsAgentTextRequest(t *testing.T) {
	cases := []struct {
		name string
		msgs []AgentMessage
		want bool
	}{
		{"empty", nil, false},
		{"string content", []AgentMessage{{Role: "user", Content: "hi"}}, true},
		{"text parts", []AgentMessage{{Role: "user", Content: []any{map[string]any{"type": "text", "text": "a"}}}}, true},
		{"tool role disqualifies", []AgentMessage{{Role: "tool", Content: "x"}}, false},
		{"tool_calls disqualify", []AgentMessage{{Role: "assistant", Content: "x", ToolCalls: []any{map[string]any{}}}}, false},
		{"non-text part disqualifies", []AgentMessage{{Role: "user", Content: []any{map[string]any{"type": "image_url"}}}}, false},
		{"system+user ok", []AgentMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsAgentTextRequest(c.msgs); got != c.want {
				t.Errorf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestCreateRequestContextResponse_Shape(t *testing.T) {
	frame := CreateRequestContextResponse()
	payload := decodeOneConnectFrame(t, frame)
	// AgentClientMessage.exec_client_message is field 2.
	execClient := decodeMessage(payload).first(2)
	if execClient == nil {
		t.Fatal("missing exec_client_message field 2")
	}
	// exec_client_message field 10 = request_context_result.
	reqCtxResult := decodeMessage(execClient).first(10)
	if reqCtxResult == nil {
		t.Fatal("missing request_context_result field 10")
	}
	// request_context_result field 1 = request_context_success (empty).
	if decodeMessage(reqCtxResult).first(1) == nil {
		t.Error("missing request_context_success field 1")
	}
}

func TestDecodeAgentFrames_GzipFrame(t *testing.T) {
	// Build a gzip-compressed frame with an "ok"-type payload.
	plain := []byte("hello-agent")
	var gzbuf bytes.Buffer
	zw := zlib.NewWriter(&gzbuf)
	zw.Write(plain)
	zw.Close()
	compressed := gzbuf.Bytes()
	// Connect frame: flags=0x01 (gzip), BE length, payload.
	frame := make([]byte, 5+len(compressed))
	frame[0] = cursorCompressGzip
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(compressed)))
	copy(frame[5:], compressed)

	var payloads [][]byte
	DecodeAgentFrames(frame, func(p []byte) { payloads = append(payloads, p) })
	if len(payloads) != 1 || string(payloads[0]) != "hello-agent" {
		t.Fatalf("got %v want [hello-agent]", payloads)
	}
}

func TestDecodeAgentFrames_TrailerSkipped(t *testing.T) {
	// A trailer frame (flags 0x02) is not emitted to onFrame.
	payload := []byte("trailer-body")
	frame := make([]byte, 5+len(payload))
	frame[0] = cursorCompressTrailer
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)

	called := false
	DecodeAgentFrames(frame, func(p []byte) { called = true })
	if called {
		t.Error("trailer frame should not be emitted")
	}
}

func TestDecodeAgentFrames_PartialFrameReturned(t *testing.T) {
	// A buffer with only 3 bytes (less than a header) returns the unconsumed tail.
	rest := DecodeAgentFrames([]byte{0x01, 0x02, 0x03}, func([]byte) {})
	if len(rest) != 3 {
		t.Errorf("rest len=%d want 3", len(rest))
	}
}

func TestDecodeAgentServerMessage_Text(t *testing.T) {
	// interaction_update (field 1) → field 1 = text delta (string directly).
	update := agentString(1, "answer chunk")
	serverMsg := agentMessage(1, update)
	ev, needCtx := DecodeAgentServerMessage(serverMsg)
	if ev.Type != "text" || ev.Value != "answer chunk" {
		t.Errorf("got %+v want text/answer chunk", ev)
	}
	if needCtx {
		t.Error("text event must not request context")
	}
}

func TestDecodeAgentServerMessage_Done(t *testing.T) {
	// interaction_update field 14 present → done.
	update := agentBool(14, true)
	serverMsg := agentMessage(1, update)
	ev, _ := DecodeAgentServerMessage(serverMsg)
	if ev.Type != "done" {
		t.Errorf("got %+v want done", ev)
	}
}

func TestDecodeAgentServerMessage_RequestContext(t *testing.T) {
	// exec_request field 2 with field 10 present → needsRequestContext.
	execReq := agentMessage(10, nil)
	serverMsg := agentMessage(2, execReq)
	_, needCtx := DecodeAgentServerMessage(serverMsg)
	if !needCtx {
		t.Error("expected needsRequestContext for field 2/10")
	}
}

func TestDecodeAgentServerMessage_UnsupportedExecRequest(t *testing.T) {
	// exec_request field 2 without field 10 → unsupported IDE tool error.
	execReq := agentMessage(1, nil) // wrong field, no field 10
	serverMsg := agentMessage(2, execReq)
	ev, _ := DecodeAgentServerMessage(serverMsg)
	if ev.Type != "error" {
		t.Errorf("got %+v want error", ev)
	}
}
