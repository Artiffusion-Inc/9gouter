// Package proxychat — request-detail observability.
//
// This ports the JS saveRequestDetail path (open-sse/handlers/chatCore.js +
// handlers/chatCore/requestDetail.js): every chat request, success or
// failure, streaming or not, is recorded into the requestDetails table when
// observability is enabled in settings. The dashboard "Request Details" tab
// reads that table via /api/usage/request-details — without these writes the
// table stays empty and the UI shows "No request details found".
//
// Unlike the JS implementation (which buffered writes in memory and flushed
// every 5s / 20 records), the Go port saves synchronously. The table is
// bounded by observabilityMaxRecords (enforced by a separate retention
// trim), and each row's payload fields are clamped by observabilityMaxJsonSize
// to prevent unbounded growth from large tool payloads. Observability can be
// toggled live from the dashboard profile page (enableObservability), so the
// gate is read from settings on each request.
package proxychat

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"strings"
	"time"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/db/repo"
	"github.com/Artiffusion-Inc/9gouter/internal/adapter/pricing"
)

// cryptoRandRead is crypto/rand.Read, aliased for testability.
var cryptoRandRead = rand.Read

// RequestDetailSaver is the subset of repo.RequestDetailRepo the usecase
// needs. Declared locally so tests can substitute a fake without importing
// the sqlite repo.
type RequestDetailSaver interface {
	Save(ctx context.Context, d repo.RequestDetail) error
}

// ObservabilityGate reads the live enableObservability flag + the retention
// knobs (maxRecords, maxJsonSize). nil → observability disabled (tests /
// legacy wiring).
type ObservabilityGate interface {
	Enabled(ctx context.Context) (maxRecords int, maxJsonSize int, ok bool)
}

// requestDetailBuilder is a closure over the per-request fixed fields so the
// streaming/non-streaming/error paths can emit a detail row without
// re-threading every argument. The ID is generated once and reused so a
// streaming request that updates its row (start → completion) stays one row.
type requestDetailBuilder struct {
	saver    RequestDetailSaver
	gate     ObservabilityGate
	maxRec   int
	maxSize  int
	enabled  bool
	id       string
	provider string
	model    string
	connID   string
}

func newRequestDetailBuilder(saver RequestDetailSaver, gate ObservabilityGate, provider, model, connID string) *requestDetailBuilder {
	return &requestDetailBuilder{
		saver:    saver,
		gate:     gate,
		id:       generateDetailID(model),
		provider: provider,
		model:    model,
		connID:   connID,
	}
}

// maybeEnabled pre-fetches the gate once per request (cheap; the gate caches
// settings internally per the JS 5s CONFIG_CACHE_TTL). Returns false when
// observability is off or no saver/gate is wired.
func (b *requestDetailBuilder) maybeEnabled(ctx context.Context) bool {
	if b.saver == nil || b.gate == nil {
		return false
	}
	maxRec, maxSize, ok := b.gate.Enabled(ctx)
	if !ok {
		return false
	}
	b.maxRec = maxRec
	b.maxSize = maxSize
	b.enabled = true
	return true
}

// save writes a detail row, clamping each payload field to maxSize and
// guarding the whole record against a nil saver / disabled observability.
// Errors are swallowed (the JS path `.catch(() => {})`): observability must
// never break the chat request.
func (b *requestDetailBuilder) save(ctx context.Context, status string, latencyMs int, streamMs *int, tps *float64, tokens map[string]int, request, providerRequest, providerResponse, response, pxpipe json.RawMessage) {
	if !b.enabled {
		return
	}
	d := repo.RequestDetail{
		ID:               b.id,
		Timestamp:        time.Now().UTC().Format(time.RFC3339Nano),
		Provider:         b.provider,
		Model:            b.model,
		ConnectionID:     b.connID,
		Status:           status,
		Request:          clampRaw(request, b.maxSize),
		ProviderRequest:  clampRaw(providerRequest, b.maxSize),
		ProviderResponse: clampRaw(providerResponse, b.maxSize),
		Response:         clampRaw(response, b.maxSize),
		Pxpipe:           clampRaw(pxpipe, b.maxSize),
	}
	d.Latency = clampRaw(mustMarshal(map[string]any{
		"total":    latencyMs,
		"streamMs": streamMs,
		"tps":      tps,
	}), b.maxSize)
	if len(tokens) > 0 {
		d.Tokens = clampRaw(mustMarshal(tokens), b.maxSize)
	}
	_ = b.saver.Save(ctx, d)
}

// generateDetailID mirrors the JS generateDetailId: <isoTimestamp>-<rand>-<model>.
func generateDetailID(model string) string {
	modelPart := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, model)
	if modelPart == "" {
		modelPart = "unknown"
	}
	return time.Now().UTC().Format("20060102T150405.000000") + "-" + randID(6) + "-" + modelPart
}

// randID returns a short lowercase-alnum id. Uses crypto/rand via the shared
// helper rather than Math.random (the JS path) for uniqueness under load.
func randID(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	_, _ = cryptoRandRead(b)
	for i := range b {
		b[i] = alphabet[b[i]%byte(len(alphabet))]
	}
	return string(b)
}

// clampRaw returns the raw JSON if it fits within maxBytes, else a truncated
// marker preserving the original size + a 200-byte preview (mirrors the JS
// truncateField). maxBytes<=0 disables clamping.
func clampRaw(raw json.RawMessage, maxBytes int) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if maxBytes <= 0 || len(raw) <= maxBytes {
		return raw
	}
	preview := raw
	if len(preview) > 200 {
		preview = preview[:200]
	}
	marker, _ := json.Marshal(map[string]any{
		"_truncated":    true,
		"_originalSize": len(raw),
		"_preview":      string(preview),
	})
	return marker
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// extractRequestConfig mirrors the JS extractRequestConfig: the request body
// stripped to the fields the observability UI renders (messages + optional
// sampling/params). Avoids recording the full body verbatim (which can be
// large) while preserving enough to debug a request.
func extractRequestConfig(body json.RawMessage, stream bool) json.RawMessage {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	cfg := map[string]any{}
	if msgs, ok := obj["messages"]; ok {
		cfg["messages"] = msgs
	}
	if m, ok := obj["model"]; ok {
		cfg["model"] = m
	}
	cfg["stream"] = stream
	for _, p := range optionalRequestParams {
		if v, ok := obj[p]; ok {
			cfg[p] = v
		}
	}
	out, _ := json.Marshal(cfg)
	return out
}

var optionalRequestParams = []string{
	"temperature", "top_p", "top_k",
	"max_tokens", "max_completion_tokens",
	"thinking", "reasoning", "enable_thinking",
	"presence_penalty", "frequency_penalty",
	"seed", "stop", "tools", "tool_choice",
	"response_format", "prediction", "store", "metadata",
	"n", "logprobs", "top_logprobs", "logit_bias",
	"user", "parallel_tool_calls",
}

// streamTokensMap builds the detail `tokens` field for the streaming path
// from the collected usage breakdown. Returns nil when nothing was collected.
func streamTokensMap(tok *pricing.Tokens, prompt, completion int) map[string]int {
	if tok == nil && prompt == 0 && completion == 0 {
		return nil
	}
	m := map[string]int{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
	if tok != nil {
		m["cached_tokens"] = tok.CachedTokens
		m["reasoning_tokens"] = tok.ReasoningTokens
		m["cache_creation_input_tokens"] = tok.CacheCreationTokens
	}
	return m
}

// tokensMapFromTokens builds the detail `tokens` field for the non-streaming
// path from the extracted usage breakdown.
func tokensMapFromTokens(tok *pricing.Tokens, prompt, completion int) map[string]int {
	m := map[string]int{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
	if tok != nil {
		m["cached_tokens"] = tok.CachedTokens
		m["reasoning_tokens"] = tok.ReasoningTokens
		m["cache_creation_input_tokens"] = tok.CacheCreationTokens
	}
	return m
}

// bodyBytesOrNil wraps a non-empty upstream body as a json.RawMessage so the
// detail's ProviderResponse field captures the raw upstream reply (clamped by
// save). nil when the body is empty.
func bodyBytesOrNil(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}
