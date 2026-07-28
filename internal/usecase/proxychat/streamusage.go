// Package proxychat — stream usage extraction.
//
// streamUsageCollector parses de-framed upstream SSE/NDJSON frames as they
// flow through httpstream.Pipe and accumulates the canonical final usage
// (prompt/completion/cached/reasoning/cache-creation). It is the Go analogue
// of the JS SSE transform stream's per-frame usage accumulation that fed
// onStreamComplete (open-sse/handlers/chatCore/streamingHandler.js:156).

package proxychat

import (
	"bytes"
	"encoding/json"

	"github.com/Artiffusion-Inc/9gouter/internal/adapter/pricing"
)

// Why a collector rather than reading one final frame: providers emit usage
// in different places. OpenAI chat.completions carry it on the terminal
// chunk (finish_reason set, usage object). Claude streams an initial
// message_start (input_tokens + cache fields) and a final message_delta
// (cumulative output_tokens). Gemini streams usageMetadata on the terminal
// chunk. Ollama streams eval_count/prompt_eval_count on the final done=true
// chunk. OpenAI Responses stream a response.completed event with usage. The
// collector merges the latest value of each field across frames so the final
// state holds the canonical breakdown regardless of where the upstream put it.
//
// The collector is concurrency-safe for the OnFrame callback (invoked from
// the pipe writer goroutine) and the post-pipe read (the Handle goroutine).
type streamUsageCollector struct {
	usage map[string]any // canonical usage map, accumulated
}

func newStreamUsageCollector() *streamUsageCollector {
	return &streamUsageCollector{usage: map[string]any{}}
}

// OnFrame is the PipeOpts.OnFrame callback. It tolerates non-JSON frames,
// "data:" prefixes, "[DONE]" sentinels, and "event:" lines — all of which
// appear in real SSE streams and must not abort collection. It is
// non-blocking per the Pipe contract: only json.Unmarshal work, which is
// bounded by the frame size cap.
func (c *streamUsageCollector) OnFrame(frame []byte) {
	payload := stripSSEPrefix(frame)
	if len(payload) == 0 {
		return
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return
	}
	c.mergeUsage(obj)
}

// mergeUsage folds the usage-relevant fields of one parsed frame into the
// canonical map. It accepts usage in any of the provider shapes:
//
//   - OpenAI chat: {usage: {prompt_tokens, completion_tokens, total_tokens,
//     cached_tokens, reasoning_tokens, prompt_tokens_details,
//     completion_tokens_details}}
//   - OpenAI Responses: {type:"response.completed", response:{usage:{
//     input_tokens, output_tokens, input_tokens_details,
//     output_tokens_details}}} — the usage is nested under response.
//   - Claude: {type:"message_start", message:{usage:{input_tokens,
//     cache_read_input_tokens, cache_creation_input_tokens, output_tokens:0}}}
//     and {type:"message_delta", usage:{output_tokens}}.
//   - Gemini: {usageMetadata:{promptTokenCount, candidatesTokenCount,
//     totalTokenCount, cachedContentTokenCount, thoughtsTokenCount}}.
//   - Ollama: {done:true, prompt_eval_count, eval_count} — no usage wrapper.
//
// Latest non-zero value wins per field (Claude's message_delta carries the
// cumulative output_tokens; OpenAI's terminal chunk carries the final
// prompt/completion). Nested detail maps (prompt_tokens_details /
// completion_tokens_details / input_tokens_details / output_tokens_details)
// are merged field-by-field so a later frame extending the breakdown does not
// wipe an earlier frame's fields.
func (c *streamUsageCollector) mergeUsage(obj map[string]any) {
	// Find the usage object in any of the known locations.
	usage := locateUsage(obj)
	if usage == nil {
		return
	}
	// Promote Ollama's bare eval_count/prompt_eval_count into the usage shape.
	if ev, ok := numericInt(obj["eval_count"]); ok {
		usage = ensureUsageMap(usage)
		if _, has := usage["completion_tokens"]; !has {
			usage["completion_tokens"] = ev
		} else if existing, ok := numericInt(usage["completion_tokens"]); !ok || existing == 0 {
			usage["completion_tokens"] = ev
		}
	}
	if pc, ok := numericInt(obj["prompt_eval_count"]); ok {
		usage = ensureUsageMap(usage)
		if _, has := usage["prompt_tokens"]; !has {
			usage["prompt_tokens"] = pc
		}
	}

	// Gemini usageMetadata → normalize to OpenAI-ish keys so extractTokens
	// (which runs on the canonical map via tokens()) can read them.
	if meta, ok := obj["usageMetadata"].(map[string]any); ok {
		usage = ensureUsageMap(usage)
		copyGeminiUsage(usage, meta)
	}

	for k, v := range usage {
		if isDetailMap(k) {
			mergeDetailMap(c.usage, k, v)
			continue
		}
		// Skip zeros only if we already have a non-zero value for this field
		// (so a later "0" does not overwrite a real count).
		if n, ok := numericInt(v); ok && n == 0 {
			if prev, ok := c.usage[k]; ok {
				if pn, ok := numericInt(prev); ok && pn != 0 {
					continue
				}
			}
		}
		c.usage[k] = v
	}
}

// tokens builds a pricing.Tokens from the accumulated canonical usage map,
// mirroring extractTokens but operating on the collected streaming usage.
// Returns nil when no usage was collected (caller falls back to headroom
// estimates), matching the non-streaming extractTokens contract.
func (c *streamUsageCollector) tokens() *pricing.Tokens {
	if len(c.usage) == 0 {
		return nil
	}
	prompt := intFromUsage(c.usage, "prompt_tokens", "input_tokens")
	completion := intFromUsage(c.usage, "completion_tokens", "output_tokens")
	t := &pricing.Tokens{PromptTokens: prompt, CompletionTokens: completion}
	if n, ok := numericInt(c.usage["cached_tokens"]); ok {
		t.CachedTokens = n
	} else if n, ok := numericInt(c.usage["cache_read_input_tokens"]); ok {
		t.CachedTokens = n
	} else if details, ok := c.usage["input_tokens_details"].(map[string]any); ok {
		if n, ok := numericInt(details["cached_tokens"]); ok {
			t.CachedTokens = n
		}
	}
	if n, ok := numericInt(c.usage["cache_creation_input_tokens"]); ok {
		t.CacheCreationTokens = n
	} else if details, ok := c.usage["prompt_tokens_details"].(map[string]any); ok {
		if n, ok := numericInt(details["cache_creation_tokens"]); ok {
			t.CacheCreationTokens = n
		}
	}
	if n, ok := numericInt(c.usage["reasoning_tokens"]); ok {
		t.ReasoningTokens = n
	} else if details, ok := c.usage["completion_tokens_details"].(map[string]any); ok {
		if n, ok := numericInt(details["reasoning_tokens"]); ok {
			t.ReasoningTokens = n
		}
	} else if details, ok := c.usage["output_tokens_details"].(map[string]any); ok {
		if n, ok := numericInt(details["reasoning_tokens"]); ok {
			t.ReasoningTokens = n
		}
	}
	return t
}

// promptCompletion exposes the flat token counts so the caller can keep the
// existing saveUsage signature shape (prompt/completion ints + a tokens
// breakdown). Zero when nothing was collected.
func (c *streamUsageCollector) promptCompletion() (int, int) {
	if len(c.usage) == 0 {
		return 0, 0
	}
	return intFromUsage(c.usage, "prompt_tokens", "input_tokens"),
		intFromUsage(c.usage, "completion_tokens", "output_tokens")
}

// locateUsage returns the usage object from a parsed frame across the known
// shapes (OpenAI, Responses-nested, Claude message_start/message_delta,
// Gemini usageMetadata-as-usage). Returns nil when the frame carries no
// usage.
func locateUsage(obj map[string]any) map[string]any {
	if u, ok := obj["usage"].(map[string]any); ok {
		return u
	}
	// OpenAI Responses: {type:"response.completed", response:{usage:{...}}}.
	if resp, ok := obj["response"].(map[string]any); ok {
		if u, ok := resp["usage"].(map[string]any); ok {
			return u
		}
	}
	// Claude message_start: {type:"message_start", message:{usage:{...}}}.
	if msg, ok := obj["message"].(map[string]any); ok {
		if u, ok := msg["usage"].(map[string]any); ok {
			return u
		}
	}
	// Gemini usageMetadata IS the usage object (normalized separately).
	if _, ok := obj["usageMetadata"].(map[string]any); ok {
		return map[string]any{} // non-nil sentinel; mergeUsage promotes it
	}
	// Ollama bare eval_count/prompt_eval_count handled in mergeUsage.
	if _, ok1 := obj["eval_count"]; ok1 {
		return map[string]any{}
	}
	if _, ok2 := obj["prompt_eval_count"]; ok2 {
		return map[string]any{}
	}
	return nil
}

func ensureUsageMap(u map[string]any) map[string]any {
	if u != nil {
		return u
	}
	return map[string]any{}
}

func isDetailMap(key string) bool {
	return key == "prompt_tokens_details" ||
		key == "completion_tokens_details" ||
		key == "input_tokens_details" ||
		key == "output_tokens_details"
}

func mergeDetailMap(dst map[string]any, key string, v any) {
	src, ok := v.(map[string]any)
	if !ok {
		return
	}
	existing, _ := dst[key].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
		dst[key] = existing
	}
	for k, val := range src {
		if n, ok := numericInt(val); ok && n == 0 {
			if prev, ok := existing[k]; ok {
				if pn, ok := numericInt(prev); ok && pn != 0 {
					continue
				}
			}
		}
		existing[k] = val
	}
}

// copyGeminiUsage translates Gemini usageMetadata fields into the OpenAI-ish
// canonical keys the collector reads.
func copyGeminiUsage(dst, meta map[string]any) {
	if n, ok := numericInt(meta["promptTokenCount"]); ok {
		dst["prompt_tokens"] = n
	}
	if n, ok := numericInt(meta["candidatesTokenCount"]); ok {
		dst["completion_tokens"] = n
	}
	if n, ok := numericInt(meta["totalTokenCount"]); ok {
		dst["total_tokens"] = n
	}
	if n, ok := numericInt(meta["cachedContentTokenCount"]); ok {
		dst["cached_tokens"] = n
	}
	if n, ok := numericInt(meta["thoughtsTokenCount"]); ok {
		dst["reasoning_tokens"] = n
	}
}

func intFromUsage(u map[string]any, keys ...string) int {
	for _, k := range keys {
		if n, ok := numericInt(u[k]); ok {
			return n
		}
	}
	return 0
}

// stripSSEPrefix trims a de-framed upstream payload to its JSON body. SSE
// frames arrive as "data: <json>\n" (possibly with an "event: <type>\n" line
// before, already stripped by the de-framer for "\n\n"-split streams but
// present inside auto/ndjson frames). NDJSON frames are bare JSON lines.
// "[DONE]" sentinels and non-JSON lines yield an empty result.
func stripSSEPrefix(frame []byte) []byte {
	// A de-framed SSE event may still contain multiple lines (event: + data:).
	// Find the "data:" line and return its payload.
	for _, line := range bytes.Split(frame, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			payload := bytes.TrimSpace(trimmed[len("data:"):])
			if bytes.Equal(payload, []byte("[DONE]")) {
				return nil
			}
			return payload
		}
		// A bare JSON line (NDJSON / passthrough) — return it if it parses as
		// an object. Non-JSON lines ("event: foo", keepalives) are skipped
		// here; the outer loop continues to the next line.
		if len(trimmed) > 0 && trimmed[0] == '{' {
			return trimmed
		}
	}
	return nil
}
