// execute.go ports the executeAgent dispatch + loop from
// open-sse/executors/cursor.js (upstream v0.5.40, commit 6994cd1f). The legacy
// ChatService (api2.cursor.sh / StreamUnifiedChatWithTools) was retired by the
// gateway; the AgentService at agent.api5.cursor.sh answers plain text turns.
//
// Execute dispatches:
//   - plain text turn (isAgentTextRequest) → executeAgent over the duplex h2
//     session (OpenAgentSession), emitting OpenAI SSE (stream) or
//     chat.completion JSON (non-stream).
//   - anything else (tool_calls / role=="tool" / non-text content) → the
//     inherited BaseExecutor.Execute, which keeps hitting the legacy
//     ChatService URL until its AgentService tool protocol is ported.
//
// The executor mirrors the commandcode executor shape: it returns a synthetic
// *http.Response whose Body is already the OpenAI-shaped output, so the
// proxychat streaming/non-streaming pipe can consume it like any other
// upstream. TransformedBody is the original client body (AgentService takes
// its own protobuf, not a translated one).
package cursorexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/http2"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/provider"
)

// agentEndpoint is the Cursor AgentService host (HTTP/2-only). The legacy
// ChatService lives on api2.cursor.sh; only the AgentService Run RPC and the
// GetUsableModels resolver use agent.api5.
const agentEndpoint = "https://agent.api5.cursor.sh"

// agentRunPath is the Connect RPC path for the streaming Run call.
const agentRunPath = "/agent.v1.AgentService/Run"

// agentBase returns the AgentService endpoint base URL, honoring an
// executor-level override (used by tests to target an in-process h2 server).
func (e *Executor) agentBase() string {
	if e.agentBaseURL != "" {
		return e.agentBaseURL
	}
	return agentEndpoint
}

// agentSSEHeaders is the Content-Type header set for a synthesized OpenAI SSE
// response body, matching the JS SSE_HEADERS.
var agentSSEHeaders = http.Header{"Content-Type": []string{"text/event-stream"}, "Cache-Control": []string{"no-cache"}}

// agentJSONHeaders is the Content-Type for a synthesized non-streaming
// chat.completion JSON response.
var agentJSONHeaders = http.Header{"Content-Type": []string{"application/json"}}

// Execute dispatches a Cursor request to the AgentService (text turns) or the
// legacy ChatService (tool turns), mirroring execute() in cursor.js.
func (e *Executor) Execute(ctx context.Context, req provider.ExecRequest) (provider.Resp, error) {
	messages, ok := parseAgentMessages(req.Body)
	if ok && IsAgentTextRequest(messages) {
		resp, err := e.executeAgent(ctx, req, messages)
		if err != nil {
			// Mirror the JS catch: surface a connection_error JSON rather than a
			// raw Go error, so the client gets a structured OpenAI-style body.
			return agentErrorResp(agentEndpoint+agentRunPath, req.Body, http.StatusInternalServerError, "connection_error", err.Error(), "", nil), nil
		}
		return resp, nil
	}
	// Non-text / tool turn → legacy ChatService via the inherited executor.
	return e.BaseExecutor.Execute(ctx, req)
}

// parseAgentMessages decodes the client request body.messages into the codec's
// normalized AgentMessage slice. Returns ok=false when the body is not a
// chat/completions-style object with a messages array (e.g. it is already a
// translated cursor body for the legacy path).
func parseAgentMessages(body json.RawMessage) ([]AgentMessage, bool) {
	if len(body) == 0 {
		return nil, false
	}
	var obj struct {
		Messages []AgentMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, false
	}
	if obj.Messages == nil {
		return nil, false
	}
	return obj.Messages, true
}

// executeAgent opens the duplex AgentService session, drives the run loop, and
// returns a synthetic OpenAI-shaped response. Mirrors executeAgent in cursor.js.
func (e *Executor) executeAgent(ctx context.Context, req provider.ExecRequest, messages []AgentMessage) (provider.Resp, error) {
	accessToken, machineID, ghostMode := cursorCreds(req.Credentials)
	if accessToken == "" {
		return provider.Resp{}, fmt.Errorf("cursor AgentService: accessToken is required")
	}
	headersMap := BuildCursorHeaders(CursorHeadersOpts{
		AccessToken: accessToken,
		MachineID:   machineID,
		GhostMode:   ghostMode,
	})
	headers := http.Header{}
	for k, v := range headersMap {
		headers.Set(k, v)
	}

	endpoint, err := url.Parse(e.agentBase() + agentRunPath)
	if err != nil {
		return provider.Resp{}, fmt.Errorf("cursor agent endpoint: %w", err)
	}
	runRequest := BuildAgentRunFrame(messages, req.Model)

	// A direct (non-proxied) h2 transport. A future proxy-aware dial would set
	// DialTLSContext on this transport to route the AgentService call through a
	// SOCKS5/HTTP proxy; for now the AgentService honors the same network path
	// as the resolver. agentTransport/agentBaseURL are injectable for tests.
	transport := e.agentTransport
	if transport == nil {
		transport = &http2.Transport{}
	}
	session, err := OpenAgentSession(ctx, transport, endpoint, headers, runRequest)
	if err != nil {
		return provider.Resp{}, err
	}

	status := session.Status()
	if status != 200 {
		errBody := drainSession(session)
		return agentErrorResp(endpoint.String(), req.Body, firstNonZero(status, http.StatusBadGateway), "api_error",
			fmt.Sprintf("Cursor AgentService %d: %s", status, firstNonEmpty(errBody, "request failed")), "", headersMap), nil
	}

	responseID := "chatcmpl-msg_" + nowTimestamp()
	created := nowUnix()

	if !req.Stream {
		return e.executeAgentNonStream(session, req, responseID, created, endpoint.String(), headersMap)
	}
	return e.executeAgentStream(session, req, responseID, created, endpoint.String(), headersMap)
}

// executeAgentNonStream drains the session into a single chat.completion JSON
// response. Mirrors the stream===false branch in cursor.js.
func (e *Executor) executeAgentNonStream(session AgentSession, req provider.ExecRequest, responseID string, created int, url string, headersMap map[string]string) (provider.Resp, error) {
	var content strings.Builder
	var agentError string
	agentLoop(session, func(ev AgentEvent) {
		switch ev.Type {
		case "text":
			content.WriteString(ev.Value)
		case "error":
			agentError = ev.Value
		}
	})

	if agentError != "" {
		return agentErrorResp(url, req.Body, http.StatusBadRequest, "api_error", agentError, "", headersMap), nil
	}

	body := map[string]any{
		"id":      responseID,
		"object":  "chat.completion",
		"created": created,
		"model":   req.Model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": stringOrNil(content.String()),
				},
				"finish_reason": "stop",
			},
		},
		"usage": estimateUsage(req.Body, content.Len()),
	}
	raw, _ := json.Marshal(body)
	return synthResp(url, req.Body, http.StatusOK, agentJSONHeaders, raw, headersMap), nil
}

// executeAgentStream drains the session into an OpenAI SSE stream. Mirrors the
// ReadableStream branch in cursor.js. The whole session is buffered before the
// first SSE byte — same as the commandcode executor — which is fine for the
// AgentService (the server emits the full turn then closes); a true streaming
// pipe would require the proxychat pipe to consume concurrently.
func (e *Executor) executeAgentStream(session AgentSession, req provider.ExecRequest, responseID string, created int, url string, headersMap map[string]string) (provider.Resp, error) {
	var sseLines []byte
	agentLoop(session, func(ev AgentEvent) {
		switch ev.Type {
		case "text":
			sseLines = append(sseLines, sseChunk(responseID, created, req.Model, map[string]any{"content": ev.Value}, nil)...)
		case "error":
			sseLines = append(sseLines, sseChunk(responseID, created, req.Model, map[string]any{"content": "\n[" + ev.Value + "]"}, nil)...)
		case "done":
			sseLines = append(sseLines, sseChunk(responseID, created, req.Model, map[string]any{}, "stop")...)
			sseLines = append(sseLines, []byte("data: [DONE]\n\n")...)
		}
	})
	// If the stream ended without an explicit done frame, emit a terminal chunk
	// + [DONE] so the client never hangs on a half-open SSE.
	if !strings.Contains(string(sseLines), "[DONE]") {
		sseLines = append(sseLines, sseChunk(responseID, created, req.Model, map[string]any{}, "stop")...)
		sseLines = append(sseLines, []byte("data: [DONE]\n\n")...)
	}
	return synthResp(url, req.Body, http.StatusOK, agentSSEHeaders, sseLines, headersMap), nil
}

// agentLoop drives the duplex session: read frames, decode AgentServerMessage
// events, write CreateRequestContextResponse when the server asks for IDE
// context, and forward text/error/done events to onEvent. Mirrors consume() in
// cursor.js.
func agentLoop(session AgentSession, onEvent func(AgentEvent)) {
	var pending []byte
	for {
		data, err := session.Read()
		if len(data) > 0 {
			pending = append(pending, data...)
			pending = DecodeAgentFrames(pending, func(payload []byte) {
				ev, needCtx := DecodeAgentServerMessage(payload)
				if needCtx {
					_ = session.Write(CreateRequestContextResponse())
					return
				}
				if ev.Type != "" {
					onEvent(ev)
				}
			})
		}
		if err != nil {
			break
		}
		if data == nil && err == nil {
			break
		}
	}
	_ = session.Close()
}

// sseChunk renders one OpenAI chat.completion.chunk SSE line.
func sseChunk(id string, created int, model string, delta map[string]any, finishReason any) []byte {
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			},
		},
	}
	raw, _ := json.Marshal(chunk)
	return append([]byte("data: "), append(raw, '\n', '\n')...)
}

// estimateUsage mirrors the JS estimateUsage for the OpenAI format: input
// tokens ≈ len(JSON(body))/4, output tokens ≈ contentLen/4, with a 2000-token
// buffer added to prompt/total (mirrors addBufferToUsage). The result carries
// estimated:true so downstream consumers know this is not a server-reported
// count.
func estimateUsage(body json.RawMessage, contentLen int) map[string]any {
	inputTokens := (len(body) + 3) / 4
	if inputTokens < 0 {
		inputTokens = 0
	}
	outputTokens := 0
	if contentLen > 0 {
		outputTokens = contentLen / 4
		if outputTokens < 1 {
			outputTokens = 1
		}
	}
	const bufferTokens = 2000
	inputTokens += bufferTokens
	return map[string]any{
		"prompt_tokens":     inputTokens,
		"completion_tokens": outputTokens,
		"total_tokens":      inputTokens + outputTokens,
		"estimated":         true,
	}
}

// cursorCreds extracts accessToken, machineId, and ghostMode from the
// connection credentials, mirroring buildHeaders in cursor.js. ghostMode
// defaults to true (the JS default is `providerSpecificData.ghostMode !== false`).
func cursorCreds(creds provider.Credentials) (accessToken, machineID string, ghostMode bool) {
	ghostMode = true
	if creds.AccessToken != "" {
		accessToken = creds.AccessToken
	}
	if creds.ProviderSpecificData != nil {
		if v, ok := creds.ProviderSpecificData["machineId"].(string); ok {
			machineID = v
		}
		if g, ok := creds.ProviderSpecificData["ghostMode"].(bool); ok && !g {
			ghostMode = false
		}
	}
	return accessToken, machineID, ghostMode
}

// drainSession reads any remaining bytes off a non-200 session for the error
// body, then closes it.
func drainSession(session AgentSession) string {
	var b strings.Builder
	for {
		data, err := session.Read()
		if len(data) > 0 {
			b.Write(data)
		}
		if err != nil || data == nil {
			break
		}
	}
	_ = session.Close()
	return b.String()
}

// synthResp builds the synthetic *http.Response returned to proxychat.
func synthResp(url string, transformedBody json.RawMessage, status int, header http.Header, body []byte, headersMap map[string]string) provider.Resp {
	return provider.Resp{
		Response: &http.Response{
			StatusCode: status,
			Header:     header,
			Body:       io.NopCloser(bytes.NewReader(body)),
		},
		URL:             url,
		Headers:         headerFromMap(headersMap),
		TransformedBody: transformedBody,
	}
}

// agentErrorResp builds a synthetic error response shaped like the JS
// { error: { message, type, code } } OpenAI error body.
func agentErrorResp(url string, transformedBody json.RawMessage, status int, errType, message, code string, headersMap map[string]string) provider.Resp {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	})
	return synthResp(url, transformedBody, status, agentJSONHeaders, body, headersMap)
}

func headerFromMap(m map[string]string) http.Header {
	h := http.Header{}
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}

func stringOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// nowUnix returns the current Unix timestamp. Wrapped so tests can stub it.
var nowUnix = func() int { return int(time.Now().Unix()) }

// nowTimestamp returns a millisecond timestamp string for the chat completion
// id. Wrapped so tests can stub it.
var nowTimestamp = func() string { return fmt.Sprintf("%d", time.Now().UnixMilli()) }
