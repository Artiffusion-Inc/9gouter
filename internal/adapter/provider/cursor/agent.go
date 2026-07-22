// agent.go ports the AgentService protobuf codec from open-sse/executors/cursor.js
// (upstream v0.5.40, commit 6994cd1f "executeAgent" path). The legacy ChatService
// (api2.cursor.sh / StreamUnifiedChatWithTools) was retired by the gateway; the
// AgentService at agent.api5.cursor.sh is HTTP/2-only and speaks a different
// protobuf: agent.v1.AgentClientMessage.run_request / AgentServerMessage.
//
// This file owns the pure codec half (buildAgentRunFrame, request-context
// response, frame decode, interaction_update text extraction) so it is
// unit-testable without an HTTP/2 server. The duplex h2 transport + the
// executeAgent loop live in executor.go / h2stream.go.
package cursorexec

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/binary"
	"io"
	"strings"
)

// Connect RPC frame compression flags (mirror cursor.js COMPRESS_FLAG).
const (
	cursorCompressGzip    = 0x01
	cursorCompressTrailer = 0x02
)

// agentString/agentMessage encode a length-delimited (wire type 2) field.
// agentBool encodes a varint (wire type 0) field. These mirror the JS
// helpers of the same names; the AgentService protobuf is hand-rolled
// (reverse-engineered wire format, no .proto).
func agentString(field int, value string) []byte  { return encodeField(field, wireLen, value) }
func agentMessage(field int, value []byte) []byte { return encodeField(field, wireLen, value) }
func agentBool(field int, value bool) []byte {
	if value {
		return encodeField(field, wireVarint, uint64(1))
	}
	return encodeField(field, wireVarint, uint64(0))
}

// randomAgentUUID is a random v4 UUID string for the user_message id. Mirrors
// crypto.randomUUID() in buildAgentRunFrame. Isolated so tests can stub it.
var randomAgentUUID = func() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0F) | 0x40
	b[8] = (b[8] & 0x3F) | 0x80
	return formatUUID(b)
}

// AgentMessage is the minimal normalized OpenAI message shape buildAgentRunFrame
// consumes. Decoding json.RawMessage into this keeps the codec testable without
// the proxychat body types.
type AgentMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	// tool_calls / role=="tool" disqualify a message from the agent text path
	// (isAgentTextRequest); they are carried only to detect non-text turns.
	ToolCalls []any `json:"tool_calls"`
}

// IsAgentTextRequest reports whether body.messages is a plain text turn: every
// message has string or text-part content and none carries tool_calls / tool
// role. Mirrors isAgentTextRequest in cursor.js — AgentService can answer the
// text turn even when a compatible client attaches its built-in tool schemas,
// while the retired ChatService rejected those.
func IsAgentTextRequest(messages []AgentMessage) bool {
	if len(messages) == 0 {
		return false
	}
	for _, m := range messages {
		if len(m.ToolCalls) > 0 || m.Role == "tool" {
			return false
		}
		switch c := m.Content.(type) {
		case string:
			// ok
		case []any:
			for _, part := range c {
				pm, ok := part.(map[string]any)
				if !ok {
					return false
				}
				if t, _ := pm["type"].(string); t != "text" {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

// encodeHistoryMessage encodes one non-current conversation turn into a
// ConversationHistoryMessage: user → field 1, assistant → field 2, each
// wrapping repeated content (field 1 → field 1 → text). Returns nil if the
// turn has no extractable text. Mirrors encodeHistoryMessage in cursor.js.
func encodeHistoryMessage(m AgentMessage) []byte {
	content := textFromContent(m.Content)
	if content == "" {
		return nil
	}
	text := agentString(1, content)
	if m.Role == "assistant" {
		return agentMessage(2, agentMessage(1, agentMessage(1, text)))
	}
	return agentMessage(1, agentMessage(1, agentMessage(1, text)))
}

// BuildAgentRunFrame builds the agent.v1.AgentClientMessage.run_request
// Connect RPC frame for the AgentService Run call: an empty
// ConversationStateStructure (fresh session), the user action (current message
// + optional history), the folded system prompt, and the requested model. The
// returned frame is already Connect-RPC-wrapped (5-byte header + payload).
func BuildAgentRunFrame(messages []AgentMessage, model string) []byte {
	var systemParts []string
	var chat []AgentMessage
	for _, m := range messages {
		if m.Role == "system" {
			if t := textFromContent(m.Content); t != "" {
				systemParts = append(systemParts, t)
			}
			continue
		}
		chat = append(chat, m)
	}
	system := strings.Join(systemParts, "\n\n")

	// currentIndex = last index of role=="user" in chat; -1 if none.
	currentIndex := -1
	for i := len(chat) - 1; i >= 0; i-- {
		if chat[i].Role == "user" {
			currentIndex = i
			break
		}
	}
	var current AgentMessage
	var history []AgentMessage
	if currentIndex >= 0 {
		current = chat[currentIndex]
		history = chat[:currentIndex]
	} else if len(chat) > 0 {
		current = chat[len(chat)-1]
		history = chat[:len(chat)-1]
	}
	userText := textFromContent(current.Content)
	if userText == "" {
		userText = "Continue."
	}

	// agent.v1.UserMessageAction.user_message (field 1: text, field 2: id).
	userMessage := concat(
		agentString(1, userText),
		agentString(2, randomAgentUUID()),
	)
	userAction := concat(agentMessage(1, userMessage))
	// agent.v1.UserMessageAction.conversation_history (field 7, optional).
	if len(history) > 0 {
		var histBuf []byte
		for _, h := range history {
			if enc := encodeHistoryMessage(h); enc != nil {
				histBuf = concat(histBuf, agentMessage(1, enc))
			}
		}
		if histBuf != nil {
			userAction = concat(userAction, agentMessage(7, histBuf))
		}
	}
	conversationAction := agentMessage(1, userAction)
	requestedModel := concat(agentString(1, model), agentBool(7, true))
	runRequest := concat(
		agentMessage(1, nil), // empty ConversationStateStructure → fresh session
		agentMessage(2, conversationAction),
	)
	if system != "" {
		runRequest = concat(runRequest, agentString(8, system))
	}
	runRequest = concat(runRequest, agentMessage(9, requestedModel))

	// agent.v1.AgentClientMessage.run_request (field 1) → Connect frame.
	return wrapConnectRPCFrame(agentMessage(1, runRequest), false)
}

// CreateRequestContextResponse builds the agent.v1.AgentClientMessage that
// acknowledges a server request_context_args with an empty RequestContext
// (9router has no IDE file context). Mirrors createRequestContextResponse.
//
// Wire shape: exec_client_message (field 10) → request_context_result (1) →
// request_context_success (1) → empty. Wrapped as AgentClientMessage field 2.
func CreateRequestContextResponse() []byte {
	requestContextSuccess := agentMessage(1, nil)
	requestContextResult := agentMessage(1, requestContextSuccess)
	execClientMessage := agentMessage(10, requestContextResult)
	return wrapConnectRPCFrame(agentMessage(2, execClientMessage), false)
}

// DecodeAgentFrames walks a Connect-RPC frame buffer, gunzipping gzip frames
// and invoking onFrame for each non-trailer frame payload. Returns the
// unconsumed trailing bytes (a partial frame split across reads). Mirrors
// decodeAgentFrames in cursor.js.
func DecodeAgentFrames(buf []byte, onFrame func(payload []byte)) []byte {
	pos := 0
	for len(buf)-pos >= 5 {
		flags := buf[pos]
		length := int(binary.BigEndian.Uint32(buf[pos+1 : pos+5]))
		if len(buf)-pos < 5+length {
			break
		}
		payload := buf[pos+5 : pos+5+length]
		pos += 5 + length
		if flags&cursorCompressGzip != 0 {
			if dec, err := gunzipPayload(payload); err == nil {
				payload = dec
			}
		}
		if flags&cursorCompressTrailer == 0 {
			onFrame(payload)
		}
	}
	return buf[pos:]
}

func gunzipPayload(payload []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// AgentEvent is one decoded AgentServerMessage event the executor acts on.
type AgentEvent struct {
	Type  string // "text", "error", "done"
	Value string
}

// DecodeAgentServerMessage decodes one agent.v1.AgentServerMessage payload into
// an event. Field 1 = interaction_update (field 1 = text delta; field 14 =
// done); field 2 = exec_request (field 10 = request_context_args → needs a
// CreateRequestContextResponse write; anything else is an unsupported IDE tool
// → error+done). Mirrors the consume() body in cursor.js executeAgent.
//
// Returns (event, needsRequestContext). When needsRequestContext is true the
// caller must write CreateRequestContextResponse() back on the duplex stream.
func DecodeAgentServerMessage(payload []byte) (AgentEvent, bool) {
	serverMessage := decodeMessage(payload)
	// interaction_update (field 1).
	if v := serverMessage.first(1); v != nil {
		update := decodeMessage(v)
		if textDelta := update.first(1); textDelta != nil {
			if s := string(textDelta); s != "" {
				return AgentEvent{Type: "text", Value: s}, false
			}
		}
		// field 14 = done marker.
		if update.has(14) {
			return AgentEvent{Type: "done"}, false
		}
	}
	// exec_request (field 2) — server asks for IDE context.
	if v := serverMessage.first(2); v != nil {
		execRequest := decodeMessage(v)
		if execRequest.has(10) {
			return AgentEvent{}, true // request_context_args → write empty context
		}
		return AgentEvent{Type: "error", Value: "Cursor AgentService requested an unsupported IDE tool"}, false
	}
	return AgentEvent{}, false
}
