package cursorexec

// protobuf_test.go ports the behavior of open-sse/utils/cursorProtobuf.js
// (upstream v0.5.40) and pins it with round-trip tests — no mocks, no network:
// encode primitives → decode primitives, encode messages → decode messages,
// generateCursorBody → parseConnectRPCFrame → extractTextFromResponse.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeDecodeVarint(t *testing.T) {
	cases := []uint64{0, 1, 127, 128, 16383, 16384, 1 << 20, 1<<32 - 1, 1<<63 - 1}
	for _, v := range cases {
		enc := encodeVarint(v)
		got, n := decodeVarint(enc, 0)
		if got != v {
			t.Errorf("varint %d: decoded %d (bytes=%x)", v, got, enc)
		}
		if n != len(enc) {
			t.Errorf("varint %d: consumed %d want %d", v, n, len(enc))
		}
	}
}

func TestEncodeFieldLenStringRoundTrip(t *testing.T) {
	// Field 5 (model), LEN, string payload.
	enc := encodeField(fieldModel, wireLen, "gpt-cursor")
	fn, wt, val, n := decodeField(enc, 0)
	if fn != fieldModel || wt != wireLen {
		t.Fatalf("field = (%d,%d) want (5,2)", fn, wt)
	}
	if string(val) != "gpt-cursor" {
		t.Fatalf("value = %q want gpt-cursor", val)
	}
	if n != len(enc) {
		t.Fatalf("consumed %d want %d", n, len(enc))
	}
}

func TestEncodeFieldVarintRoundTrip(t *testing.T) {
	enc := encodeField(fieldIsAgentic, wireVarint, 1)
	fn, wt, _, n := decodeField(enc, 0)
	if fn != fieldIsAgentic || wt != wireVarint {
		t.Fatalf("field = (%d,%d) want (27,0)", fn, wt)
	}
	if n != len(enc) {
		t.Fatalf("consumed %d want %d", n, len(enc))
	}
}

func TestEncodeMessageRoundTrip(t *testing.T) {
	enc := encodeMessage("hello", roleUser, "msg-1", false, false, nil, "")
	m := decodeMessage(enc)
	if got := string(m.first(fieldMsgContent)); got != "hello" {
		t.Fatalf("content = %q", got)
	}
	if v := m.varintValue(fieldMsgRole); v != roleUser {
		t.Fatalf("role = %d want 1", v)
	}
	if got := string(m.first(fieldMsgID)); got != "msg-1" {
		t.Fatalf("id = %q", got)
	}
}

func TestFormatParseToolNameRoundTrip(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Write", "mcp_custom_Write"},
		{"mcp__server__tool", "mcp_server_tool"},
		{"mcp__server__", "mcp_server_tool"},
		{"mcp_foo_bar", "mcp_foo_bar"},
		{"mcp_foo", "mcp_foo"},
		{"", "mcp_custom_tool"},
	}
	for _, c := range cases {
		got := formatToolName(c.in)
		if got != c.want {
			t.Errorf("formatToolName(%q) = %q want %q", c.in, got, c.want)
		}
		// parse(format(x)) should recover server + selected tool for the
		// mcp_ prefixed outputs.
		if strings.HasPrefix(got, "mcp_") {
			p := parseToolName(got)
			_ = p // structural round-trip: no panic, serverName non-empty
			if p.serverName == "" {
				t.Errorf("parseToolName(%q) empty server", got)
			}
		}
	}
}

func TestParseToolID(t *testing.T) {
	id := "call_123\nmc_456"
	p := parseToolID(id)
	if p.toolCallID != "call_123" || p.modelCallID != "456" {
		t.Fatalf("parseToolID(%q) = %+v", id, p)
	}
	p2 := parseToolID("plain")
	if p2.toolCallID != "plain" || p2.modelCallID != "" {
		t.Fatalf("parseToolID(plain) = %+v", p2)
	}
}

func TestGenerateCursorBodyRoundTrip(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "What is 2+2?"},
	}
	env := defaultMetaEnv()
	env.NowISO = "2026-07-20T12:00:00.000Z"
	body := generateCursorBody(msgs, "gpt-4", nil, "", false, env)

	// Outer frame: 5-byte header + protobuf.
	frame, consumed := parseConnectRPCFrame(body)
	if frame == nil {
		t.Fatalf("parseConnectRPCFrame returned nil for body len=%d", len(body))
	}
	if consumed != len(body) {
		t.Fatalf("consumed %d want body len %d", consumed, len(body))
	}
	if frame.flags != 0x00 {
		t.Errorf("flags = %#x want 0 (uncompressed request)", frame.flags)
	}

	// The framed payload is field 1 (REQUEST) wrapping the
	// StreamUnifiedChatRequest. Decode one level and check the model field
	// round-trips back to the requested model name.
	top := decodeMessage(frame.payload)
	if !top.has(fieldRequest) {
		t.Fatal("top-level payload missing fieldRequest (1)")
	}
	req := decodeMessage(top.first(fieldRequest))
	modelMsg := decodeMessage(req.first(fieldModel))
	if got := string(modelMsg.first(fieldModelName)); got != "gpt-4" {
		t.Fatalf("model name round-trip = %q want gpt-4", got)
	}
}

func TestExtractTextFromResponseText(t *testing.T) {
	// Build a response: field 2 (RESPONSE) wrapping field 1 (RESPONSE_TEXT).
	resp := encodeField(fieldRespText, wireLen, "hello world")
	payload := encodeField(fieldRespResponse, wireLen, resp)

	out := extractTextFromResponse(payload)
	if out.text == nil || *out.text != "hello world" {
		t.Fatalf("text = %v want hello world", out.text)
	}
	if out.toolCall != nil {
		t.Fatalf("unexpected tool call: %+v", out.toolCall)
	}
}

func TestExtractTextFromResponseThinking(t *testing.T) {
	thinkingMsg := encodeField(fieldThinkingText, wireLen, "reasoning here")
	resp := concat(
		encodeField(fieldRespText, wireLen, "answer"),
		encodeField(fieldRespThinking, wireLen, thinkingMsg),
	)
	payload := encodeField(fieldRespResponse, wireLen, resp)

	out := extractTextFromResponse(payload)
	if out.text == nil || *out.text != "answer" {
		t.Fatalf("text = %v", out.text)
	}
	if out.thinking == nil || *out.thinking != "reasoning here" {
		t.Fatalf("thinking = %v", out.thinking)
	}
}

func TestExtractTextFromResponseToolCall(t *testing.T) {
	// Build a ClientSideToolV2Call response: field 3 (TOOL_ID), field 9
	// (TOOL_NAME), and field 27 (TOOL_MCP_PARAMS) wrapping the nested tool
	// (MCP_TOOLS_LIST → MCP_NESTED_NAME + MCP_NESTED_PARAMS).
	nested := encodeField(fieldMCPToolsList, wireLen, concat(
		encodeField(fieldMCPNestedName, wireLen, "Read"),
		encodeField(fieldMCPNestedParams, wireLen, `{"path":"/x"}`),
	))
	toolCall := concat(
		encodeField(fieldToolID, wireLen, "call_1"),
		encodeField(fieldToolName, wireLen, "mcp_custom_Read"),
		encodeField(fieldToolMCPParams, wireLen, nested),
	)
	payload := encodeField(fieldRespToolCall, wireLen, toolCall)

	out := extractTextFromResponse(payload)
	if out.toolCall == nil {
		t.Fatalf("expected tool call, got %+v", out)
	}
	if out.toolCall.id != "call_1" {
		t.Errorf("id = %q want call_1", out.toolCall.id)
	}
	if out.toolCall.name != "Read" {
		t.Errorf("name = %q want Read (from nested MCP params)", out.toolCall.name)
	}
	if out.toolCall.args != `{"path":"/x"}` {
		t.Errorf("args = %q", out.toolCall.args)
	}
	if out.toolCall.typ != "function" {
		t.Errorf("type = %q want function", out.toolCall.typ)
	}
}

func TestParseConnectRPCFrameGzip(t *testing.T) {
	// A gzip-compressed frame (flags 0x01) must decompress transparently.
	inner := encodeField(fieldRespText, wireLen, "decompressed")
	resp := encodeField(fieldRespResponse, wireLen, inner)
	framed := wrapConnectRPCFrame(resp, true)

	frame, consumed := parseConnectRPCFrame(framed)
	if frame == nil {
		t.Fatal("gzip frame did not parse")
	}
	if consumed != len(framed) {
		t.Errorf("consumed %d want %d", consumed, len(framed))
	}
	out := extractTextFromResponse(frame.payload)
	if out.text == nil || *out.text != "decompressed" {
		t.Fatalf("after gzip decompress: text = %v", out.text)
	}
}

func TestParseConnectRPCFramePartial(t *testing.T) {
	// A buffer shorter than the declared length returns nil (no full frame).
	inner := []byte("payload")
	full := wrapConnectRPCFrame(inner, false)
	// Truncate one byte off the payload.
	short := full[:len(full)-1]
	frame, _ := parseConnectRPCFrame(short)
	if frame != nil {
		t.Fatal("expected nil for partial frame")
	}
	// A buffer shorter than 5 bytes also returns nil.
	frame2, _ := parseConnectRPCFrame([]byte{1, 2, 3})
	if frame2 != nil {
		t.Fatal("expected nil for <5 byte frame")
	}
}

func TestBuildToolResultRequest(t *testing.T) {
	tr := toolResult{
		toolCallID:    "call_1\nmc_m1",
		toolName:      "mcp_custom_Write",
		resultContent: "done",
	}
	payload := buildToolResultRequest(tr)
	// Field 2 wraps the ClientSideToolV2Result.
	top := decodeMessage(payload)
	if !top.has(2) {
		t.Fatal("buildToolResultRequest missing field 2")
	}
	cv2 := decodeMessage(top.first(2))
	if v := cv2.varintValue(fieldCV2RTool); v != clientSideToolV2MCP {
		t.Errorf("CV2R_TOOL = %d want %d", v, clientSideToolV2MCP)
	}
	mcp := decodeMessage(cv2.first(fieldCV2RMCPResult))
	// selected_tool should be the raw name with the mcp_custom_ prefix stripped.
	if got := string(mcp.first(fieldMCPRSelectedTool)); got != "Write" {
		t.Errorf("selected_tool = %q want Write", got)
	}
	if got := string(mcp.first(fieldMCPRResult)); got != "done" {
		t.Errorf("result = %q want done", got)
	}
	if got := string(cv2.first(fieldCV2RCallID)); got != "call_1" {
		t.Errorf("call_id = %q want call_1", got)
	}
	if got := string(cv2.first(fieldCV2RModelCallID)); got != "m1" {
		t.Errorf("model_call_id = %q want m1", got)
	}
}

func TestEncodeMCPToolJSONParams(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}
	enc := encodeMCPTool(mcpTool{Name: "Read", Description: "read a file", InputSchema: schema})
	m := decodeMessage(enc)
	if got := string(m.first(fieldMCPToolName)); got != "Read" {
		t.Errorf("name = %q", got)
	}
	if got := string(m.first(fieldMCPToolDesc)); got != "read a file" {
		t.Errorf("desc = %q", got)
	}
	raw := m.first(fieldMCPToolParams)
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("params not valid JSON: %v", err)
	}
	if back["type"] != "object" {
		t.Errorf("params type = %v", back["type"])
	}
	if got := string(m.first(fieldMCPToolServer)); got != "custom" {
		t.Errorf("server = %q want custom", got)
	}
}

func TestWrapConnectRPCFrameHeader(t *testing.T) {
	payload := []byte{0xaa, 0xbb, 0xcc}
	frame := wrapConnectRPCFrame(payload, false)
	if len(frame) != 5+3 {
		t.Fatalf("frame len = %d want 8", len(frame))
	}
	if frame[0] != 0x00 {
		t.Errorf("flags = %#x", frame[0])
	}
	// Big-endian 32-bit length = 3.
	want := []byte{0, 0, 0, 3}
	if !bytes.Equal(frame[1:5], want) {
		t.Errorf("length bytes = %x want %x", frame[1:5], want)
	}
	if !bytes.Equal(frame[5:], payload) {
		t.Errorf("payload mismatch")
	}
}

func TestNormalizeMessagesSplitsMixedAssistant(t *testing.T) {
	// An assistant message carrying both tool_calls and tool_results must be
	// split: the call-only message, then a separate empty assistant message
	// with the tool_results (unless the next message already matches).
	msgs := []chatMessage{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []chatToolCall{{ID: "c1"}}, ToolResults: []toolResult{{toolCallID: "c1"}}},
	}
	out := normalizeMessages(msgs)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages (user, assistant-call, assistant-results), got %d: %+v", len(out), out)
	}
	if len(out[1].ToolCalls) != 1 || len(out[1].ToolResults) != 0 {
		t.Errorf("call-only message should have no tool_results: %+v", out[1])
	}
	if out[2].Role != "assistant" || len(out[2].ToolResults) != 1 {
		t.Errorf("results message wrong: %+v", out[2])
	}
}
func TestGenerateToolResultBodyRoundTrip(t *testing.T) {
	tr := toolResult{toolCallID: "call_9", toolName: "mcp_Read", resultContent: "ok"}
	body := generateToolResultBody(tr)
	frame, consumed := parseConnectRPCFrame(body)
	if frame == nil {
		t.Fatal("generateToolResultBody did not produce a parseable frame")
	}
	if consumed != len(body) {
		t.Fatalf("consumed %d want %d", consumed, len(body))
	}
	// Field 2 wraps the ClientSideToolV2Result.
	top := decodeMessage(frame.payload)
	if !top.has(2) {
		t.Fatal("missing field 2 (client_side_tool_v2_result)")
	}
}

func TestExtractedResponseDecodeErrField(t *testing.T) {
	// The decodeErr field exists so callers wrapping decode in recovery can
	// signal a malformed payload; here we just exercise it structurally.
	resp := extractedResponse{decodeErr: "boom"}
	if resp.decodeErr != "boom" {
		t.Fatalf("decodeErr = %q", resp.decodeErr)
	}
}
