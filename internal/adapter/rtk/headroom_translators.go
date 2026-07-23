package rtk

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/translator"
	"github.com/Artiffusion-Inc/9gouter/internal/domain/format"
)

// headroom_translators.go ports the translator-backed Headroom compression
// branches of open-sse/rtk/headroom.js. The /v1/compress proxy only understands
// the OpenAI chat shape, so Claude and OpenAI-Responses bodies are translated
// to OpenAI, compressed, then translated back — preserving the original
// request contract. Kiro projects conversationState to OpenAI messages and
// writes the compressed text back into the original Kiro fields
// (headroom_kiro.go).
//
// These were previously stubs returning "translator not available". The rtk
// package can safely import the translator package — translator does not
// import rtk — so the edge introduces no cycle.

// translateViaRequest round-trips a body map through a source→target request
// translation via the package-level translator registry. It returns the
// translated body map or an error.
func translateViaRequest(source, target format.Format, model string, body map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal for translation: %w", err)
	}
	out, err := translator.TranslateRequest(source, target, model, raw, false, "")
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("unmarshal translated body: %w", err)
	}
	return result, nil
}

// compressClaudeViaHeadroomImpl ports the `format === "claude"` branch of
// compressWithHeadroom: Claude body → OpenAI → /v1/compress → OpenAI→Claude,
// writing compressed messages and (when present) system back onto the body.
func compressClaudeViaHeadroomImpl(body map[string]any, cfg HeadroomConfig, client *http.Client, timeoutMs int) (*HeadroomStats, error) {
	oai, err := translateViaRequest(format.Claude, format.Openai, cfg.Model, body)
	if err != nil {
		return nil, fmt.Errorf("claude→openai: %w", err)
	}
	oaiMessages, ok := oai["messages"].([]any)
	if !ok {
		return nil, fmt.Errorf("claude request did not translate to messages[]")
	}
	stats, err := callCompress(cfg.URL, oaiMessages, cfg.Model, timeoutMs, cfg.CompressUserMessages, client, cfg.Diagnostics)
	if err != nil {
		return nil, err
	}
	if stats == nil || len(stats.Messages) == 0 {
		return nil, fmt.Errorf("proxy returned no messages")
	}
	// Rebuild the Claude body from the compressed OpenAI messages. `messages`
	// is replaced; other fields are carried through so openai→claude sees the
	// same model/max_tokens/stream context.
	rebuild := map[string]any{}
	for k, v := range oai {
		if k == "messages" {
			continue
		}
		rebuild[k] = v
	}
	rebuild["messages"] = messagesToAny(stats.Messages)
	claudeBody, err := translateViaRequest(format.Openai, format.Claude, cfg.Model, rebuild)
	if err != nil {
		return nil, fmt.Errorf("openai→claude: %w", err)
	}
	if msgs, ok := claudeBody["messages"].([]any); ok {
		body["messages"] = msgs
	}
	if sys, ok := claudeBody["system"]; ok {
		body["system"] = sys
	}
	return stats, nil
}

// hasUnsafeResponsesInputForCompression mirrors hasUnsafeResponsesInputForCompression
// in headroom.js (#2132): an OpenAI-Responses body.input is unsafe to compress
// when it contains any non-`message` item (function_call / function_call_output /
// reasoning), because /v1/compress would collapse them into chat messages and
// break the Responses contract.
func hasUnsafeResponsesInputForCompression(body map[string]any) bool {
	input, ok := body["input"].([]any)
	if !ok {
		return false
	}
	for _, itemAny := range input {
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := item["type"].(string); t != "" && t != "message" {
			return true
		}
	}
	return false
}

// compressResponsesViaHeadroomImpl ports the `format === "openai-responses"`
// branch (#1998 / d4d11357 + 373850ee guard): body.input → OpenAI messages →
// /v1/compress → back to Responses input. Skips when input holds non-message
// items (#2132) so tool/reasoning history is not collapsed.
func compressResponsesViaHeadroomImpl(body map[string]any, cfg HeadroomConfig, client *http.Client, timeoutMs int) (*HeadroomStats, error) {
	if hasUnsafeResponsesInputForCompression(body) {
		setDiagnostic(cfg.Diagnostics, "skipped: openai-responses tool/reasoning input is not safe to compress")
		return nil, nil
	}
	oai, err := translateViaRequest(format.OpenaiResponses, format.Openai, cfg.Model, body)
	if err != nil {
		return nil, fmt.Errorf("responses→openai: %w", err)
	}
	oaiMessages, ok := oai["messages"].([]any)
	if !ok {
		return nil, fmt.Errorf("responses request did not translate to messages[]")
	}
	stats, err := callCompress(cfg.URL, oaiMessages, cfg.Model, timeoutMs, cfg.CompressUserMessages, client, cfg.Diagnostics)
	if err != nil {
		return nil, err
	}
	if stats == nil || len(stats.Messages) == 0 {
		return nil, fmt.Errorf("proxy returned no messages")
	}
	// `input` is omitted so openai→responses rebuilds input from the compressed
	// messages instead of returning the original input unchanged (the
	// translator short-circuits when input is already present).
	rebuild := map[string]any{}
	for k, v := range oai {
		if k == "input" {
			continue
		}
		rebuild[k] = v
	}
	rebuild["messages"] = messagesToAny(stats.Messages)
	responsesBody, err := translateViaRequest(format.Openai, format.OpenaiResponses, cfg.Model, rebuild)
	if err != nil {
		return nil, fmt.Errorf("openai→responses: %w", err)
	}
	if in, ok := responsesBody["input"].([]any); ok {
		body["input"] = in
	}
	return stats, nil
}
