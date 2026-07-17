// Package chat defines core chat request/response entities and pass-through
// types used by the proxy use case.
package chat

import (
	"encoding/json"
	"time"
)

// Message is a chat message. Content is left as json.RawMessage so that nested
// string/array/object shapes are preserved byte-for-byte.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Name    string          `json:"name,omitempty"`
}

// Tool describes a callable tool in the request.
type Tool struct {
	Type     string          `json:"type"`
	Function json.RawMessage `json:"function"`
}

// Usage reports token counts and inferred cost for a completed request.
type Usage struct {
	PromptTokens     int     `json:"promptTokens"`
	CompletionTokens int     `json:"completionTokens"`
	Cost             float64 `json:"cost"`
}

// FinishReason records why a generation stopped.
type FinishReason string

// Common finish reason values used across providers.
const (
	FinishReasonStop          FinishReason = "stop"
	FinishReasonLength        FinishReason = "length"
	FinishReasonToolCalls     FinishReason = "tool_calls"
	FinishReasonContentFilter FinishReason = "content_filter"
	FinishReasonOther         FinishReason = "other"
)

// ChatRequest is the normalized request passed into the proxychat use case.
// RawBody preserves the original request bytes for passthrough and
// translator fallback.
type ChatRequest struct {
	Model    string          `json:"model"`
	Messages []Message       `json:"messages,omitempty"`
	Tools    []Tool          `json:"tools,omitempty"`
	Stream   bool            `json:"stream,omitempty"`
	RawBody  json.RawMessage `json:"-"`
}

// ChatResponse is a non-streaming response envelope returned by the provider
// layer. For streaming responses the provider returns an io.ReadCloser of
// SSE frames instead.
type ChatResponse struct {
	Model        string       `json:"model"`
	Content      string       `json:"content"`
	Reasoning    string       `json:"reasoning_content,omitempty"`
	ToolCalls    json.RawMessage `json:"tool_calls,omitempty"`
	Usage        Usage        `json:"usage"`
	FinishReason FinishReason `json:"finish_reason"`
}

// StreamFrame is a single server-sent event decoded from an upstream stream.
type StreamFrame struct {
	Event string
	Data  []byte
	ID    string
	Retry time.Duration
}
