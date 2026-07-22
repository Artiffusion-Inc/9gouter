// Package cursorexec — cursorProtobuf port.
//
// protobuf.go ports open-sse/utils/cursorProtobuf.js (upstream v0.5.40) — a
// hand-rolled Cursor ConnectRPC protobuf encoder/decoder. Cursor does not
// publish .proto files for its StreamUnifiedChatRequestWithTools /
// StreamUnifiedChatResponseWithTools messages; the wire format is
// reverse-engineered from the cursor-api Rust source, so this file encodes
// field tags + varint/LEN payloads directly rather than going through
// google.golang.org/protobuf. The JS original uses 32-bit unsigned shifts
// (>>>= 7) for varints; the Go port uses uint64, which is a superset for the
// field-tag/length values this codec ever encodes (< 2^32), and is safe for
// decoding 64-bit varints from upstream responses.
//
// Encoder: encodeVarint, encodeField, encodeMessage/encodeModel/etc.,
// encodeRequest, buildChatRequest, buildToolResultRequest, wrapConnectRPCFrame,
// generateCursorBody, generateToolResultBody, plus the MCP tool-result/call
// sub-encoders.
//
// Decoder: decodeVarint, decodeField, decodeMessage, parseConnectRPCFrame,
// extractTextFromResponse (text + thinking + tool call), extractToolCall.
package cursorexec

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"

	"github.com/google/uuid"
)

// protobufSchemaVersion mirrors the JS PROTOBUF_SCHEMA_VERSION; bumped when
// the reverse-engineered wire format is refreshed against a Cursor protocol
// update.
const protobufSchemaVersion = "1.1.3"

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireLen     = 2
	wireFixed32 = 5
)

const (
	roleUser      = 1
	roleAssistant = 2
)

const (
	unifiedModeChat  = 1
	unifiedModeAgent = 2
)

const (
	thinkingUnspecified = 0
	thinkingMedium      = 1
	thinkingHigh        = 2
)

// clientSideToolV2MCP is the ClientSideToolV2 tool discriminator for MCP (19),
// matching CLIENT_SIDE_TOOL_V2_MCP in the JS source.
const clientSideToolV2MCP = 19

// Field numbers for every nested message Cursor uses. Mirrors the JS FIELD
// object verbatim; grouped by the message they belong to.
const (
	// StreamUnifiedChatRequestWithTools (top level).
	fieldRequest = 1

	// StreamUnifiedChatRequest.
	fieldMessages        = 1
	fieldUnknown2        = 2
	fieldInstruction     = 3
	fieldUnknown4        = 4
	fieldModel           = 5
	fieldWebTool         = 8
	fieldUnknown13       = 13
	fieldCursorSetting   = 15
	fieldUnknown19       = 19
	fieldConversationID  = 23
	fieldMetadata        = 26
	fieldIsAgentic       = 27
	fieldSupportedTools  = 29
	fieldMessageIDs      = 30
	fieldMCPTools        = 34
	fieldLargeContext    = 35
	fieldUnknown38       = 38
	fieldUnifiedMode     = 46
	fieldUnknown47       = 47
	fieldDisableTools    = 48
	fieldThinkingLevel   = 49
	fieldUnknown51       = 51
	fieldUnknown53       = 53
	fieldUnifiedModeName = 54

	// ConversationMessage.
	fieldMsgContent        = 1
	fieldMsgRole          = 2
	fieldMsgID            = 13
	fieldMsgToolResults   = 18
	fieldMsgIsAgentic     = 29
	fieldMsgServerBubble  = 32
	fieldMsgUnifiedMode   = 47
	fieldMsgSupportedTool = 51

	// ConversationMessage.ToolResult.
	fieldToolResultCallID      = 1
	fieldToolResultName        = 2
	fieldToolResultIndex        = 3
	fieldToolResultRawArgs      = 5
	fieldToolResultResult      = 8
	fieldToolResultToolCall     = 11
	fieldToolResultModelCallID = 12

	// ClientSideToolV2Result (nested inside ToolResult.result).
	fieldCV2RTool         = 1
	fieldCV2RMCPResult     = 28
	fieldCV2RCallID       = 35
	fieldCV2RModelCallID  = 48
	fieldCV2RToolIndex    = 49
	// Aliases used by the JS encodeClientSideToolV2Result.
	fieldMCPRSelectedTool = 1
	fieldMCPRResult       = 2

	// ClientSideToolV2Call (nested inside ToolResult.tool_call).
	fieldCV2CTool         = 1
	fieldCV2CMCPParams    = 27
	fieldCV2CCallID       = 3
	fieldCV2CName         = 9
	fieldCV2CRawArgs      = 10
	fieldCV2CToolIndex    = 48
	fieldCV2CModelCallID  = 49

	// Model.
	fieldModelName  = 1
	fieldModelEmpty = 4

	// Instruction.
	fieldInstructionText = 1

	// CursorSetting.
	fieldSettingPath     = 1
	fieldSettingUnknown3 = 3
	fieldSettingUnknown6 = 6
	fieldSettingUnknown8 = 8
	fieldSettingUnknown9 = 9
	fieldSetting6Field1   = 1
	fieldSetting6Field2   = 2

	// Metadata.
	fieldMetaPlatform  = 1
	fieldMetaArch      = 2
	fieldMetaVersion   = 3
	fieldMetaCwd       = 4
	fieldMetaTimestamp = 5

	// MessageId.
	fieldMsgIDID     = 1
	fieldMsgIDSum    = 2
	fieldMsgIDRole   = 3

	// MCPTool.
	fieldMCPToolName   = 1
	fieldMCPToolDesc   = 2
	fieldMCPToolParams = 3
	fieldMCPToolServer = 4

	// StreamUnifiedChatResponseWithTools (response).
	fieldRespToolCall = 1
	fieldRespResponse = 2

	// ClientSideToolV2Call (response side).
	fieldToolID          = 3
	fieldToolName        = 9
	fieldToolRawArgs     = 10
	fieldToolIsLast      = 11
	fieldToolIsLastAlt   = 15
	fieldToolMCPParams   = 27

	// MCPParams.
	fieldMCPToolsList = 1

	// MCPParams.Tool (nested).
	fieldMCPNestedName   = 1
	fieldMCPNestedParams = 3

	// StreamUnifiedChatResponse.
	fieldRespText     = 1
	fieldRespThinking = 25

	// Thinking.
	fieldThinkingText = 1
)

// knownResponseFields mirrors the JS KNOWN_RESPONSE_FIELDS set — used to flag
// unknown field numbers from protocol updates. Several field names share the
// same number across Cursor's nested messages (e.g. TOOL_CALL and
// RESPONSE_TEXT are both 1); the set is keyed by number, so we build it from a
// slice to avoid duplicate-key map-literal errors.
var knownResponseFields = func() map[int]bool {
	nums := []int{
		fieldRespToolCall, fieldRespResponse, fieldToolID, fieldToolName,
		fieldToolRawArgs, fieldToolIsLast, fieldToolMCPParams, fieldRespText,
		fieldRespThinking,
	}
	m := make(map[int]bool, len(nums))
	for _, n := range nums {
		m[n] = true
	}
	return m
}()

// =====================================================================
// Primitive encoding
// =====================================================================

// encodeVarint encodes an unsigned value as a base-128 varint.
func encodeVarint(value uint64) []byte {
	var buf []byte
	for value >= 0x80 {
		buf = append(buf, byte((value&0x7F)|0x80))
		value >>= 7
	}
	return append(buf, byte(value&0x7F))
}

// encodeField encodes a single protobuf field. LEN fields accept either a
// string (UTF-8 encoded) or a raw byte payload.
func encodeField(fieldNum, wireType int, value any) []byte {
	tag := uint64(fieldNum)<<3 | uint64(wireType)
	tagBytes := encodeVarint(tag)

	switch wireType {
	case wireVarint:
		var n uint64
		switch v := value.(type) {
		case int:
			n = uint64(v)
		case uint64:
			n = v
		case int64:
			n = uint64(v)
		case int32:
			n = uint64(v)
		case uint32:
			n = uint64(v)
		case bool:
			if v {
				n = 1
			}
		default:
			n = 0
		}
		return append(tagBytes, encodeVarint(n)...)
	case wireLen:
		var data []byte
		switch v := value.(type) {
		case string:
			data = []byte(v)
		case []byte:
			data = v
		case nil:
			data = nil
		default:
			data = nil
		}
		return append(append(tagBytes, encodeVarint(uint64(len(data)))...), data...)
	default:
		return nil
	}
}

// concat joins byte slices into one.
func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// =====================================================================
// Tool-name helpers
// =====================================================================

// formatToolName normalizes a tool name to the "mcp_<server>_<tool>" shape
// Cursor expects: "Write" → "mcp_custom_Write", "mcp__srv__tool" →
// "mcp_srv_tool", "mcp_foo" → "mcp_foo".
func formatToolName(name string) string {
	base := name
	if base == "" {
		base = "tool"
	}
	if strings.HasPrefix(base, "mcp__") {
		rest := base[len("mcp__"):]
		if idx := strings.Index(rest, "__"); idx >= 0 {
			server := rest[:idx]
			if server == "" {
				server = "custom"
			}
			toolName := rest[idx+2:]
			if toolName == "" {
				toolName = "tool"
			}
			return "mcp_" + server + "_" + toolName
		}
		if rest == "" {
			rest = "tool"
		}
		return "mcp_custom_" + rest
	}
	if strings.HasPrefix(base, "mcp_") {
		return base
	}
	return "mcp_custom_" + base
}

// parsedToolName is the result of splitting a formatted "mcp_<server>_<tool>".
type parsedToolName struct {
	serverName   string
	selectedTool string
}

// parseToolName splits a formatted "mcp_<server>_<tool>" back into its parts.
func parseToolName(formatted string) parsedToolName {
	if !strings.HasPrefix(formatted, "mcp_") {
		tool := formatted
		if tool == "" {
			tool = "tool"
		}
		return parsedToolName{serverName: "custom", selectedTool: tool}
	}
	tail := formatted[len("mcp_"):]
	idx := strings.Index(tail, "_")
	if idx < 0 {
		tool := tail
		if tool == "" {
			tool = "tool"
		}
		return parsedToolName{serverName: "custom", selectedTool: tool}
	}
	server := tail[:idx]
	if server == "" {
		server = "custom"
	}
	tool := tail[idx+1:]
	if tool == "" {
		tool = "tool"
	}
	return parsedToolName{serverName: server, selectedTool: tool}
}

// parsedToolID is the result of splitting a Cursor tool_call_id.
type parsedToolID struct {
	toolCallID  string
	modelCallID string
}

// parseToolID splits a Cursor tool_call_id at the "\nmc_" delimiter into
// {toolCallId, modelCallId}; modelCallId is "" when absent.
func parseToolID(id string) parsedToolID {
	const delimiter = "\nmc_"
	if idx := strings.Index(id, delimiter); idx >= 0 {
		return parsedToolID{toolCallID: id[:idx], modelCallID: id[idx+len(delimiter):]}
	}
	return parsedToolID{toolCallID: id}
}

// =====================================================================
// Sub-message encoders
// =====================================================================

// encodeMCPResult encodes MCPResult { selected_tool, result }.
func encodeMCPResult(selectedTool, resultContent string) []byte {
	return concat(
		encodeField(fieldMCPRSelectedTool, wireLen, selectedTool),
		encodeField(fieldMCPRResult, wireLen, resultContent),
	)
}

// encodeClientSideToolV2Result encodes the tool-result message.
func encodeClientSideToolV2Result(toolCallID, modelCallID, selectedTool, resultContent string, toolIndex int) []byte {
	parts := [][]byte{
		encodeField(fieldCV2RTool, wireVarint, clientSideToolV2MCP),
		encodeField(fieldCV2RMCPResult, wireLen, encodeMCPResult(selectedTool, resultContent)),
		encodeField(fieldCV2RCallID, wireLen, toolCallID),
	}
	if modelCallID != "" {
		parts = append(parts, encodeField(fieldCV2RModelCallID, wireLen, modelCallID))
	}
	if toolIndex <= 0 {
		toolIndex = 1
	}
	parts = append(parts, encodeField(fieldCV2RToolIndex, wireVarint, toolIndex))
	return concat(parts...)
}

// encodeMCPParamsForCall encodes the MCPParams.Tool nested inside a
// ClientSideToolV2Call.
func encodeMCPParamsForCall(toolName, rawArgs, serverName string) []byte {
	tool := concat(
		encodeField(fieldMCPToolName, wireLen, toolName),
		encodeField(fieldMCPToolParams, wireLen, rawArgs),
		encodeField(fieldMCPToolServer, wireLen, serverName),
	)
	return encodeField(fieldMCPToolsList, wireLen, tool)
}

// encodeClientSideToolV2Call encodes the tool-call definition message.
func encodeClientSideToolV2Call(toolCallID, toolName, selectedTool, serverName, rawArgs, modelCallID string, toolIndex int) []byte {
	parts := [][]byte{
		encodeField(fieldCV2CTool, wireVarint, clientSideToolV2MCP),
		encodeField(fieldCV2CMCPParams, wireLen, encodeMCPParamsForCall(selectedTool, rawArgs, serverName)),
		encodeField(fieldCV2CCallID, wireLen, toolCallID),
		encodeField(fieldCV2CName, wireLen, toolName),
		encodeField(fieldCV2CRawArgs, wireLen, rawArgs),
	}
	if toolIndex <= 0 {
		toolIndex = 1
	}
	parts = append(parts, encodeField(fieldCV2CToolIndex, wireVarint, toolIndex))
	if modelCallID != "" {
		parts = append(parts, encodeField(fieldCV2CModelCallID, wireLen, modelCallID))
	}
	return concat(parts...)
}

// toolResult is the OpenAI→Cursor tool-result payload.
type toolResult struct {
	toolCallID    string
	toolName      string // raw name (tool_name || name)
	rawArgs       string // raw_args || "{}"
	resultContent string // result_content || result
	toolIndex     int
}

// encodeToolResult encodes a ConversationMessage.ToolResult with the full
// nested ClientSideToolV2Result + ClientSideToolV2Call structure.
func encodeToolResult(tr toolResult) []byte {
	toolName := formatToolName(tr.toolName)
	if tr.toolName == "" {
		toolName = ""
	}
	rawArgs := tr.rawArgs
	if rawArgs == "" {
		rawArgs = "{}"
	}
	parsedID := parseToolID(tr.toolCallID)
	toolIndex := tr.toolIndex
	if toolIndex == 0 {
		toolIndex = 1
	}
	parsedName := parseToolName(toolName)

	parts := [][]byte{
		encodeField(fieldToolResultCallID, wireLen, parsedID.toolCallID),
		encodeField(fieldToolResultName, wireLen, toolName),
		encodeField(fieldToolResultIndex, wireVarint, toolIndex),
	}
	if parsedID.modelCallID != "" {
		parts = append(parts, encodeField(fieldToolResultModelCallID, wireLen, parsedID.modelCallID))
	}
	parts = append(parts,
		encodeField(fieldToolResultRawArgs, wireLen, rawArgs),
		encodeField(fieldToolResultResult, wireLen,
			encodeClientSideToolV2Result(parsedID.toolCallID, parsedID.modelCallID, parsedName.selectedTool, tr.resultContent, toolIndex)),
		encodeField(fieldToolResultToolCall, wireLen,
			encodeClientSideToolV2Call(parsedID.toolCallID, toolName, parsedName.selectedTool, parsedName.serverName, rawArgs, parsedID.modelCallID, toolIndex)),
	)
	return concat(parts...)
}

// encodeMessage encodes a ConversationMessage.
func encodeMessage(content string, role int, messageID string, isLast, hasTools bool, toolResults []toolResult, serverBubbleID string) []byte {
	parts := [][]byte{
		encodeField(fieldMsgContent, wireLen, content),
		encodeField(fieldMsgRole, wireVarint, role),
		encodeField(fieldMsgID, wireLen, messageID),
	}
	if serverBubbleID != "" {
		parts = append(parts, encodeField(fieldMsgServerBubble, wireLen, serverBubbleID))
	}
	for _, tr := range toolResults {
		parts = append(parts, encodeField(fieldMsgToolResults, wireLen, encodeToolResult(tr)))
	}
	parts = append(parts,
		encodeField(fieldMsgIsAgentic, wireVarint, hasTools),
		encodeField(fieldMsgUnifiedMode, wireVarint, boolToInt(hasTools, unifiedModeAgent, unifiedModeChat)),
	)
	if isLast && hasTools {
		parts = append(parts, encodeField(fieldMsgSupportedTool, wireLen, encodeVarint(1)))
	}
	return concat(parts...)
}

func boolToInt(cond bool, ifTrue, ifFalse int) int {
	if cond {
		return ifTrue
	}
	return ifFalse
}

// encodeInstruction encodes an Instruction { text } message; empty text → empty.
func encodeInstruction(text string) []byte {
	if text == "" {
		return nil
	}
	return encodeField(fieldInstructionText, wireLen, text)
}

// encodeModel encodes a Model { name, empty }.
func encodeModel(modelName string) []byte {
	return concat(
		encodeField(fieldModelName, wireLen, modelName),
		encodeField(fieldModelEmpty, wireLen, []byte{}),
	)
}

// encodeCursorSetting encodes the static CursorSetting blob.
func encodeCursorSetting() []byte {
	unknown6 := concat(
		encodeField(fieldSetting6Field1, wireLen, []byte{}),
		encodeField(fieldSetting6Field2, wireLen, []byte{}),
	)
	return concat(
		encodeField(fieldSettingPath, wireLen, "cursor\\aisettings"),
		encodeField(fieldSettingUnknown3, wireLen, []byte{}),
		encodeField(fieldSettingUnknown6, wireLen, unknown6),
		encodeField(fieldSettingUnknown8, wireVarint, 1),
		encodeField(fieldSettingUnknown9, wireVarint, 1),
	)
}

// metaEnv supplies the platform/arch/version/cwd values for encodeMetadata.
// Defaults mirror the JS process.* fallbacks (linux/x64/v20.0.0/"/").
type metaEnv struct {
	Platform string
	Arch     string
	Version  string
	Cwd      string
	NowISO   string
}

func defaultMetaEnv() metaEnv {
	return metaEnv{Platform: "linux", Arch: "x64", Version: "v20.0.0", Cwd: "/"}
}

// encodeMetadata encodes the Metadata { platform, arch, version, cwd, timestamp }.
func encodeMetadata(env metaEnv) []byte {
	if env.Platform == "" {
		env.Platform = "linux"
	}
	if env.Arch == "" {
		env.Arch = "x64"
	}
	if env.Version == "" {
		env.Version = "v20.0.0"
	}
	if env.Cwd == "" {
		env.Cwd = "/"
	}
	ts := env.NowISO
	if ts == "" {
		ts = "1970-01-01T00:00:00.000Z"
	}
	return concat(
		encodeField(fieldMetaPlatform, wireLen, env.Platform),
		encodeField(fieldMetaArch, wireLen, env.Arch),
		encodeField(fieldMetaVersion, wireLen, env.Version),
		encodeField(fieldMetaCwd, wireLen, env.Cwd),
		encodeField(fieldMetaTimestamp, wireLen, ts),
	)
}

// encodeMessageID encodes a MessageId { id, summary?, role }.
func encodeMessageID(messageID string, role int, summaryID string) []byte {
	parts := [][]byte{encodeField(fieldMsgIDID, wireLen, messageID)}
	if summaryID != "" {
		parts = append(parts, encodeField(fieldMsgIDSum, wireLen, summaryID))
	}
	parts = append(parts, encodeField(fieldMsgIDRole, wireVarint, role))
	return concat(parts...)
}

// mcpTool is the OpenAI-shaped tool definition the client sends.
type mcpTool struct {
	Name        string
	Description string
	InputSchema map[string]any // parameters || input_schema
}

// encodeMCPTool encodes an MCPTool { name?, desc?, params, server }.
func encodeMCPTool(tool mcpTool) []byte {
	parts := [][]byte{}
	if tool.Name != "" {
		parts = append(parts, encodeField(fieldMCPToolName, wireLen, tool.Name))
	}
	if tool.Description != "" {
		parts = append(parts, encodeField(fieldMCPToolDesc, wireLen, tool.Description))
	}
	if len(tool.InputSchema) > 0 {
		if b, err := json.Marshal(tool.InputSchema); err == nil {
			parts = append(parts, encodeField(fieldMCPToolParams, wireLen, b))
		}
	}
	parts = append(parts, encodeField(fieldMCPToolServer, wireLen, "custom"))
	return concat(parts...)
}

// =====================================================================
// Request building
// =====================================================================

// chatMessage is the normalized OpenAI message shape the encoder consumes.
type chatMessage struct {
	Role        string         `json:"role"`
	Content     any            `json:"content"`
	ToolCalls   []chatToolCall `json:"tool_calls"`
	ToolResults []toolResult   `json:"tool_results"`
}

// chatToolCall is an OpenAI tool_calls entry.
type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// encodeRequest builds a StreamUnifiedChatRequest.
func encodeRequest(messages []chatMessage, modelName string, tools []mcpTool, reasoningEffort string, forceAgentMode bool, env metaEnv) []byte {
	hasTools := len(tools) > 0
	isAgentic := hasTools || forceAgentMode

	normalized := normalizeMessages(messages)

	type formatted struct {
		content     string
		role        int
		messageID   string
		isLast      bool
		hasTools    bool
		toolResults []toolResult
	}
	type msgID struct {
		messageID string
		role      int
	}
	formattedMessages := make([]formatted, 0, len(normalized))
	messageIDs := make([]msgID, 0, len(normalized))

	for i := range normalized {
		msg := normalized[i]
		role := roleAssistant
		if msg.Role == "user" {
			role = roleUser
		}
		id := uuid.NewString()
		formattedMessages = append(formattedMessages, formatted{
			content:     textFromContent(msg.Content),
			role:        role,
			messageID:   id,
			isLast:      i == len(normalized)-1,
			hasTools:    hasTools,
			toolResults: msg.ToolResults,
		})
		messageIDs = append(messageIDs, msgID{messageID: id, role: role})
	}

	// Map reasoning effort to thinking level.
	thinkingLevel := thinkingUnspecified
	switch reasoningEffort {
	case "medium":
		thinkingLevel = thinkingMedium
	case "high":
		thinkingLevel = thinkingHigh
	}

	parts := make([][]byte, 0, 32)
	for _, fm := range formattedMessages {
		parts = append(parts, encodeField(fieldMessages, wireLen,
			encodeMessage(fm.content, fm.role, fm.messageID, fm.isLast, fm.hasTools, fm.toolResults, "")))
	}

	parts = append(parts,
		encodeField(fieldUnknown2, wireVarint, 1),
		encodeField(fieldInstruction, wireLen, encodeInstruction("")),
		encodeField(fieldUnknown4, wireVarint, 1),
		encodeField(fieldModel, wireLen, encodeModel(modelName)),
		encodeField(fieldWebTool, wireLen, ""),
		encodeField(fieldUnknown13, wireVarint, 1),
		encodeField(fieldCursorSetting, wireLen, encodeCursorSetting()),
		encodeField(fieldUnknown19, wireVarint, 1),
		encodeField(fieldConversationID, wireLen, uuid.NewString()),
		encodeField(fieldMetadata, wireLen, encodeMetadata(env)),
		encodeField(fieldIsAgentic, wireVarint, isAgentic),
	)
	if isAgentic {
		parts = append(parts, encodeField(fieldSupportedTools, wireLen, encodeVarint(1)))
	}
	for _, mid := range messageIDs {
		parts = append(parts, encodeField(fieldMessageIDs, wireLen, encodeMessageID(mid.messageID, mid.role, "")))
	}
	for _, tool := range tools {
		parts = append(parts, encodeField(fieldMCPTools, wireLen, encodeMCPTool(tool)))
	}
	parts = append(parts,
		encodeField(fieldLargeContext, wireVarint, 0),
		encodeField(fieldUnknown38, wireVarint, 0),
		encodeField(fieldUnifiedMode, wireVarint, boolToInt(isAgentic, unifiedModeAgent, unifiedModeChat)),
		encodeField(fieldUnknown47, wireLen, ""),
		encodeField(fieldDisableTools, wireVarint, boolToInt(isAgentic, 0, 1)),
		encodeField(fieldThinkingLevel, wireVarint, thinkingLevel),
		encodeField(fieldUnknown51, wireVarint, 0),
		encodeField(fieldUnknown53, wireVarint, 1),
		encodeField(fieldUnifiedModeName, wireLen, boolToStr(isAgentic, "Agent", "Ask")),
	)
	return concat(parts...)
}

func boolToStr(cond bool, ifTrue, ifFalse string) string {
	if cond {
		return ifTrue
	}
	return ifFalse
}

// normalizeMessages splits mixed assistant payloads (tool calls + tool
// results in the same message) into separate assistant messages, preventing
// protobuf encoding errors. Mirrors the JS guardrail loop.
func normalizeMessages(messages []chatMessage) []chatMessage {
	out := make([]chatMessage, 0, len(messages))
	for i := range messages {
		msg := messages[i]
		hasToolCalls := len(msg.ToolCalls) > 0
		hasToolResults := len(msg.ToolResults) > 0
		if msg.Role == "assistant" && hasToolCalls && hasToolResults {
			// Keep the assistant tool-call message without embedded results.
			callOnly := msg
			callOnly.ToolResults = nil
			out = append(out, callOnly)

			// Avoid inserting a duplicate tool-result message if the next
			// message already matches.
			next := chatMessage{}
			if i+1 < len(messages) {
				next = messages[i+1]
			}
			nextHasToolResults := next.Role == "assistant" && len(next.ToolResults) > 0
			currentIDs := idSet(msg.ToolResults)
			nextIDs := idSet(next.ToolResults)
			same := len(currentIDs) > 0 && len(currentIDs) == len(nextIDs)
			if same {
				for id := range currentIDs {
					if !nextIDs[id] {
						same = false
						break
					}
				}
			}
			if !nextHasToolResults || !same {
				out = append(out, chatMessage{Role: "assistant", Content: "", ToolResults: msg.ToolResults})
			}
			continue
		}
		out = append(out, msg)
	}
	return out
}

func idSet(results []toolResult) map[string]bool {
	set := map[string]bool{}
	for _, tr := range results {
		if tr.toolCallID != "" {
			set[tr.toolCallID] = true
		}
	}
	return set
}

// buildChatRequest wraps a StreamUnifiedChatRequest in the top-level
// StreamUnifiedChatRequestWithTools field 1.
func buildChatRequest(messages []chatMessage, modelName string, tools []mcpTool, reasoningEffort string, forceAgentMode bool, env metaEnv) []byte {
	return encodeField(fieldRequest, wireLen, encodeRequest(messages, modelName, tools, reasoningEffort, forceAgentMode, env))
}

// buildToolResultRequest encodes a ClientSideToolV2Result as a separate
// StreamUnifiedChatRequestWithTools frame (field 2). tool_index is omitted
// (None per the cursor-api Rust source).
func buildToolResultRequest(tr toolResult) []byte {
	parsedID := parseToolID(tr.toolCallID)
	rawName := tr.toolName
	resultContent := tr.resultContent

	// selected_tool = raw tool name with any mcp_/mcp_custom_ prefix stripped.
	selectedTool := rawName
	if strings.HasPrefix(selectedTool, "mcp_custom_") {
		selectedTool = selectedTool[len("mcp_custom_"):]
	} else if strings.HasPrefix(selectedTool, "mcp_") {
		selectedTool = selectedTool[len("mcp_"):]
	}

	parts := [][]byte{
		encodeField(fieldCV2RTool, wireVarint, clientSideToolV2MCP),
		encodeField(fieldCV2RMCPResult, wireLen, encodeMCPResult(selectedTool, resultContent)),
		encodeField(fieldCV2RCallID, wireLen, parsedID.toolCallID),
	}
	if parsedID.modelCallID != "" {
		parts = append(parts, encodeField(fieldCV2RModelCallID, wireLen, parsedID.modelCallID))
	}
	cv2Result := concat(parts...)
	return encodeField(2, wireLen, cv2Result)
}

// =====================================================================
// Connect RPC framing
// =====================================================================

// wrapConnectRPCFrame wraps a protobuf payload in a 5-byte Connect RPC frame
// (1 flag byte + 4 big-endian length bytes + payload). Cursor does not accept
// compressed requests, so compress is always false on the request side.
func wrapConnectRPCFrame(payload []byte, compress bool) []byte {
	finalPayload := payload
	flags := byte(0x00)
	if compress {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(payload)
		_ = zw.Close()
		finalPayload = buf.Bytes()
		flags = 0x01
	}
	frame := make([]byte, 5+len(finalPayload))
	frame[0] = flags
	l := len(finalPayload)
	frame[1] = byte((l >> 24) & 0xFF)
	frame[2] = byte((l >> 16) & 0xFF)
	frame[3] = byte((l >> 8) & 0xFF)
	frame[4] = byte(l & 0xFF)
	copy(frame[5:], finalPayload)
	return frame
}

// generateCursorBody builds a framed StreamUnifiedChatRequestWithTools body.
func generateCursorBody(messages []chatMessage, modelName string, tools []mcpTool, reasoningEffort string, forceAgentMode bool, env metaEnv) []byte {
	protobuf := buildChatRequest(messages, modelName, tools, reasoningEffort, forceAgentMode, env)
	return wrapConnectRPCFrame(protobuf, false)
}

// generateToolResultBody builds a framed ClientSideToolV2Result body to send
// as a separate request frame.
func generateToolResultBody(tr toolResult) []byte {
	return wrapConnectRPCFrame(buildToolResultRequest(tr), false)
}

// =====================================================================
// Primitive decoding
// =====================================================================

// decodeVarint decodes a base-128 varint starting at offset; returns the
// value and the next offset.
func decodeVarint(buf []byte, offset int) (uint64, int) {
	var result uint64
	shift := uint(0)
	pos := offset
	for pos < len(buf) {
		b := buf[pos]
		result |= uint64(b&0x7F) << shift
		pos++
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	return result, pos
}

// decodedField is a single decoded field occurrence. For LEN/FIXED wire types
// value holds the payload bytes; for VARINT, value holds the re-encoded
// varint (so varintValue can re-decode it).
type decodedField struct {
	wireType int
	value    []byte
}

// decodeField decodes a single (fieldNum, wireType, value) starting at offset;
// returns fieldNum, wireType, the decoded occurrence, and the next offset.
// Returns (-1, -1, nil, offset) at end-of-buffer.
func decodeField(buf []byte, offset int) (int, int, []byte, int) {
	if offset >= len(buf) {
		return -1, -1, nil, offset
	}
	tag, pos1 := decodeVarint(buf, offset)
	fieldNum := int(tag >> 3)
	wireType := int(tag & 0x07)
	pos := pos1
	var value []byte
	switch wireType {
	case wireVarint:
		// Re-encode the decoded varint back into value so callers that read
		// the raw bytes (and varintValue below) both work; varintValue uses
		// decodeVarint on this re-encoded slice.
		v, p := decodeVarint(buf, pos)
		value = encodeVarint(v)
		pos = p
	case wireLen:
		length, pos2 := decodeVarint(buf, pos)
		end := pos2 + int(length)
		if end > len(buf) {
			end = len(buf)
		}
		value = buf[pos2:end]
		pos = end
	case wireFixed64:
		end := pos + 8
		if end > len(buf) {
			end = len(buf)
		}
		value = buf[pos:end]
		pos = end
	case wireFixed32:
		end := pos + 4
		if end > len(buf) {
			end = len(buf)
		}
		value = buf[pos:end]
		pos = end
	default:
		value = nil
	}
	return fieldNum, wireType, value, pos
}

// decodedMessage holds the repeated occurrences of each field number, keyed
// by field number (mirrors the JS Map<number, {wireType,value}[]>).
type decodedMessage map[int][]decodedField

// has reports whether the field number is present.
func (m decodedMessage) has(field int) bool {
	_, ok := m[field]
	return ok
}

// first returns the first occurrence's value bytes for a field.
func (m decodedMessage) first(field int) []byte {
	if occ, ok := m[field]; ok && len(occ) > 0 {
		return occ[0].value
	}
	return nil
}

// varintValue returns the first varint occurrence for a field as uint64.
func (m decodedMessage) varintValue(field int) uint64 {
	if occ, ok := m[field]; ok && len(occ) > 0 {
		v, _ := decodeVarint(occ[0].value, 0)
		return v
	}
	return 0
}

// decodeMessage decodes a length-delimited protobuf message into a map of
// field number → repeated occurrences.
func decodeMessage(data []byte) decodedMessage {
	fields := decodedMessage{}
	pos := 0
	for pos < len(data) {
		fieldNum, wireType, value, newPos := decodeField(data, pos)
		if fieldNum < 0 {
			break
		}
		fields[fieldNum] = append(fields[fieldNum], decodedField{wireType: wireType, value: value})
		pos = newPos
	}
	return fields
}

// =====================================================================
// Response parsing
// =====================================================================

// connectFrame is a parsed Connect RPC frame.
type connectFrame struct {
	flags   byte
	length  int
	payload []byte
	consumed int
}

// parseConnectRPCFrame parses a single 5-byte-header Connect RPC frame. If
// the buffer does not yet contain a full frame, returns (nil, 0). Decompresses
// gzip payloads (flags == 0x01).
func parseConnectRPCFrame(buf []byte) (*connectFrame, int) {
	if len(buf) < 5 {
		return nil, 0
	}
	flags := buf[0]
	length := int(buf[1])<<24 | int(buf[2])<<16 | int(buf[3])<<8 | int(buf[4])
	if len(buf) < 5+length {
		return nil, 0
	}
	payload := buf[5 : 5+length]
	if flags == 0x01 {
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err == nil {
			if decompressed, err := io.ReadAll(zr); err == nil {
				payload = decompressed
			}
			_ = zr.Close()
		}
	}
	return &connectFrame{flags: flags, length: length, payload: payload, consumed: 5 + length}, 5 + length
}

// extractedToolCall is a decoded Cursor tool call, in OpenAI tool_calls shape.
type extractedToolCall struct {
	id       string
	typ      string // always "function"
	name     string
	args     string // raw arguments JSON string
	isLast   bool
}

// extractToolCall decodes a ClientSideToolV2Call field-1 payload into an
// OpenAI-shaped tool call, or nil if incomplete.
func extractToolCall(toolCallData []byte) *extractedToolCall {
	tc := decodeMessage(toolCallData)
	toolCallID := ""
	toolName := ""
	rawArgs := ""
	isLast := false

	if id := tc.first(fieldToolID); id != nil {
		// Cursor returns a multi-line ID; take the first line.
		toolCallID = strings.SplitN(string(id), "\n", 2)[0]
	}
	if name := tc.first(fieldToolName); name != nil {
		toolName = string(name)
	}
	if tc.has(fieldToolIsLast) {
		isLast = tc.varintValue(fieldToolIsLast) != 0
	}
	// MCP params carry the nested real tool info.
	if mcpRaw := tc.first(fieldToolMCPParams); mcpRaw != nil {
		mcp := decodeMessage(mcpRaw)
		if toolRaw := mcp.first(fieldMCPToolsList); toolRaw != nil {
			tool := decodeMessage(toolRaw)
			if n := tool.first(fieldMCPNestedName); n != nil {
				toolName = string(n)
			}
			if p := tool.first(fieldMCPNestedParams); p != nil {
				rawArgs = string(p)
			}
		}
	}
	if rawArgs == "" {
		if ra := tc.first(fieldToolRawArgs); ra != nil {
			rawArgs = string(ra)
		}
	}

	if toolCallID == "" || toolName == "" {
		return nil
	}
	return &extractedToolCall{
		id:     toolCallID,
		typ:    "function",
		name:   toolName,
		args:   rawArgs,
		isLast: isLast,
	}
}

// extractedResponse is the decoded Cursor response: one of text, thinking, or
// a tool call. The error/decodeError/raw fields mirror the JS fallback shape.
type extractedResponse struct {
	text       *string
	thinking   *string
	toolCall   *extractedToolCall
	decodeErr  string
}

// extractTextAndThinking decodes a StreamUnifiedChatResponse (field 2) payload
// into { text, thinking }.
func extractTextAndThinking(responseData []byte) (*string, *string) {
	nested := decodeMessage(responseData)
	var text *string
	var thinking *string
	if t := nested.first(fieldRespText); t != nil {
		s := string(t)
		text = &s
	}
	if th := nested.first(fieldRespThinking); th != nil {
		thinkingMsg := decodeMessage(th)
		if tt := thinkingMsg.first(fieldThinkingText); tt != nil {
			s := string(tt)
			thinking = &s
		}
	}
	return text, thinking
}

// extractTextFromResponse decodes a StreamUnifiedChatResponseWithTools payload
// into { text, thinking, toolCall }. Unknown response field numbers are
// reported via the optional cursorSchemaLogger so a Cursor protocol update is
// surfaced, not silently lost. A non-empty decodeErr on the returned struct
// signals a malformed payload (set by callers that wrap decode in recovery).
func extractTextFromResponse(payload []byte) extractedResponse {
	fields := decodeMessage(payload)

	// Warn about unknown field numbers — may indicate a Cursor protocol update.
	for fieldNum := range fields {
		if !knownResponseFields[fieldNum] {
			cursorSchemaLog("Unknown response field #%d detected. Schema v%s may be outdated.", fieldNum, protobufSchemaVersion)
		}
	}

	// Field 1: ClientSideToolV2Call (tool call).
	if toolRaw := fields.first(fieldRespToolCall); toolRaw != nil {
		if tc := extractToolCall(toolRaw); tc != nil {
			return extractedResponse{toolCall: tc}
		}
	}
	// Field 2: StreamUnifiedChatResponse (text + thinking).
	if respRaw := fields.first(fieldRespResponse); respRaw != nil {
		text, thinking := extractTextAndThinking(respRaw)
		if text != nil || thinking != nil {
			return extractedResponse{text: text, thinking: thinking}
		}
	}
	return extractedResponse{}
}

// cursorSchemaLogger is the pluggable logger for schema/decode warnings. It
// defaults to a no-op; the executor wires a real slog.Logger in #96.
var cursorSchemaLogger func(format string, args ...any)

func cursorSchemaLog(format string, args ...any) {
	if cursorSchemaLogger != nil {
		cursorSchemaLogger(format, args...)
	}
}

// textFromContent coerces an OpenAI message content (string or array of
// {type:"text", text} parts) into a single string.
func textFromContent(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	arr, ok := content.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, p := range arr {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] != "text" {
			continue
		}
		if t, ok := m["text"].(string); ok {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}