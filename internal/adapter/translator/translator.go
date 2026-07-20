// Package translator provides a registry-based request/response translation layer.
// It mirrors the API surface of open-sse/translator/index.js: register,
// TranslateRequest, TranslateResponse, and NeedsTranslation.
package translator

import (
	"encoding/json"
	"fmt"

	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

// Translator turns an OpenAI-shaped request body (already normalized to the
// OpenAI base format when source != target) into a target-format request body.
// The contract passes credentials and streaming flag because some translators
// need them to choose shapes; in the current port only the request side needs
// the provider identifier for a few post-processing helpers.
type Translator interface {
	TranslateRequest(model string, body json.RawMessage, stream bool, providerID string) (json.RawMessage, error)
}

// ResponseTranslator converts a single target-format response chunk into one
// or more source-format chunks. If a translator returns nil/nil the chunk is
// dropped; returning a single chunk is the common case.
type ResponseTranslator interface {
	TranslateResponse(chunk json.RawMessage, state map[string]any) ([]json.RawMessage, error)
}

// Registry stores format-pair translators. Two maps are kept: one for request
// translation and one for response translation, matching the JS design.
type Registry struct {
	request  map[pair]Translator
	response map[pair]ResponseTranslator
}

type pair struct {
	from format.Format
	to   format.Format
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		request:  make(map[pair]Translator),
		response: make(map[pair]ResponseTranslator),
	}
}

// Register adds a translator for a format pair. A nil request or response half
// is ignored; callers typically register only the request half or both halves.
func (r *Registry) Register(from, to format.Format, t any) {
	if r == nil {
		return
	}
	if rt, ok := t.(Translator); ok && rt != nil {
		r.request[pair{from, to}] = rt
	}
	if resp, ok := t.(ResponseTranslator); ok && resp != nil {
		r.response[pair{from, to}] = resp
	}
}

// RegisterRequest adds a request translator for a format pair.
func (r *Registry) RegisterRequest(from, to format.Format, t Translator) {
	if r == nil || t == nil {
		return
	}
	r.request[pair{from, to}] = t
}

// RegisterResponse adds a response translator for a format pair.
func (r *Registry) RegisterResponse(from, to format.Format, t ResponseTranslator) {
	if r == nil || t == nil {
		return
	}
	r.response[pair{from, to}] = t
}

// packageRegistry is the package-level registry used by the package functions.
var packageRegistry = NewRegistry()

// Register registers a translator on the package-level registry.
func Register(from, to format.Format, t any) {
	packageRegistry.Register(from, to, t)
}

// RegisterRequest registers a request translator on the package-level registry.
func RegisterRequest(from, to format.Format, t Translator) {
	packageRegistry.RegisterRequest(from, to, t)
}

// RegisterResponse registers a response translator on the package-level registry.
func RegisterResponse(from, to format.Format, t ResponseTranslator) {
	packageRegistry.RegisterResponse(from, to, t)
}

// TranslateRequest translates body from sourceFormat toward targetFormat. It
// follows the JS route: first try a direct source->target translator; otherwise
// pivot through OpenAI (source->openai, then openai->target). When source or
// target is already OpenAI the corresponding half is skipped.
func TranslateRequest(sourceFormat, targetFormat format.Format, model string, body json.RawMessage, stream bool, providerID string) (json.RawMessage, error) {
	if sourceFormat == targetFormat {
		return body, nil
	}

	// Direct route.
	if t, ok := packageRegistry.request[pair{sourceFormat, targetFormat}]; ok {
		return t.TranslateRequest(model, body, stream, providerID)
	}

	result := body

	// Step 1: source -> openai.
	if sourceFormat != format.Openai {
		if t, ok := packageRegistry.request[pair{sourceFormat, format.Openai}]; ok {
			var err error
			result, err = t.TranslateRequest(model, result, stream, providerID)
			if err != nil {
				return nil, fmt.Errorf("translate %s->openai: %w", sourceFormat, err)
			}
		}
	}

	// Step 2: openai -> target.
	if targetFormat != format.Openai {
		if t, ok := packageRegistry.request[pair{format.Openai, targetFormat}]; ok {
			var err error
			result, err = t.TranslateRequest(model, result, stream, providerID)
			if err != nil {
				return nil, fmt.Errorf("translate openai->%s: %w", targetFormat, err)
			}
		}
	}

	return result, nil
}

// TranslateResponse translates a target-format response chunk back toward the
// source format, pivoting through OpenAI when no direct translator is registered.
func TranslateResponse(targetFormat, sourceFormat format.Format, chunk json.RawMessage, state map[string]any) ([]json.RawMessage, error) {
	if sourceFormat == targetFormat {
		return []json.RawMessage{chunk}, nil
	}

	// Direct route.
	if t, ok := packageRegistry.response[pair{targetFormat, sourceFormat}]; ok {
		out, err := t.TranslateResponse(chunk, state)
		if err != nil {
			return nil, err
		}
		if out == nil {
			return nil, nil
		}
		return out, nil
	}

	results := []json.RawMessage{chunk}
	var openaiIntermediate []json.RawMessage

	// Step 1: target -> openai.
	if targetFormat != format.Openai {
		if t, ok := packageRegistry.response[pair{targetFormat, format.Openai}]; ok {
			out, err := t.TranslateResponse(chunk, state)
			if err != nil {
				return nil, fmt.Errorf("translate response %s->openai: %w", targetFormat, err)
			}
			if out == nil {
				results = nil
			} else {
				results = out
				openaiIntermediate = out
			}
		}
	}

	// Step 2: openai -> source.
	if sourceFormat != format.Openai {
		if t, ok := packageRegistry.response[pair{format.Openai, sourceFormat}]; ok {
			final := make([]json.RawMessage, 0, len(results))
			for _, r := range results {
				out, err := t.TranslateResponse(r, state)
				if err != nil {
					return nil, fmt.Errorf("translate response openai->%s: %w", sourceFormat, err)
				}
				if out == nil {
					continue
				}
				final = append(final, out...)
			}
			results = final
		}
	}

	// Preserve OpenAI intermediate results for logging, mirroring the JS property.
	if openaiIntermediate != nil && sourceFormat != format.Openai && targetFormat != format.Openai {
		if len(results) > 0 {
			// Store as a synthetic field on the first result; not used for wire.
			var first map[string]any
			if err := json.Unmarshal(results[0], &first); err == nil {
				first["_openaiIntermediate"] = openaiIntermediate
				if b, err := json.Marshal(first); err == nil {
					results[0] = b
				}
			}
		}
	}

	return results, nil
}

// NeedsTranslation reports whether source and target differ.
func NeedsTranslation(sourceFormat, targetFormat format.Format) bool {
	return sourceFormat != targetFormat
}

// InitState returns an initial per-stream state map. The JS function returns an
// object with bookkeeping fields; in Go we keep it as a generic map and let
// individual translators add their own fields when needed.
func InitState(sourceFormat format.Format) map[string]any {
	state := map[string]any{
		"messageId":          nil,
		"model":              nil,
		"textBlockStarted":   false,
		"thinkingBlockStarted": false,
		"inThinkingBlock":    false,
		"currentBlockIndex":  nil,
		"toolCalls":          map[string]any{},
		"finishReason":       nil,
		"finishReasonSent":   false,
		"usage":              nil,
		"contentBlockIndex":  -1,
	}
	if sourceFormat == format.OpenaiResponses {
		state["seq"] = 0
		state["responseId"] = "resp_placeholder"
		state["created"] = 0
		state["started"] = false
		state["msgTextBuf"] = map[string]any{}
		state["msgItemAdded"] = map[string]any{}
		state["msgContentAdded"] = map[string]any{}
		state["msgItemDone"] = map[string]any{}
		state["reasoningId"] = ""
		state["reasoningIndex"] = -1
		state["reasoningBuf"] = ""
		state["reasoningPartAdded"] = false
		state["reasoningDone"] = false
		state["inThinking"] = false
		state["funcArgsBuf"] = map[string]any{}
		state["funcNames"] = map[string]any{}
		state["funcCallIds"] = map[string]any{}
		state["funcArgsDone"] = map[string]any{}
		state["funcItemDone"] = map[string]any{}
		state["completedSent"] = false
	}
	return state
}
