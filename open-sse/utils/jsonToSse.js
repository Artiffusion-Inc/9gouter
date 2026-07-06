// ponytail: ported from OmniRoute jsonToSse.ts (#3089).
// Some OpenAI-compatible "reasoning" upstreams ignore stream:true and reply
// with a single application/json chat-completion body. The streaming pipeline
// only recognizes SSE `data:` frames, so such a body produced a spurious
// STREAM_EARLY_EOF / 502 despite carrying valid content / reasoning_content.
// This synthesizes an OpenAI SSE (chat.completion.chunk) stream from that JSON,
// preserving both content and reasoning_content. Returns "" when the text is
// not a parseable chat-completion object with at least one choice — callers
// then fall back to the original (error) handling.

import { normalizeOpenAICompatibleFinishReasonString } from "./finishReason.js";
import { getUnsupportedReasoningValue } from "./reasoningFields.js";

function isRecord(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function nonEmptyString(value) {
  return typeof value === "string" && value.length > 0 ? value : "";
}

function addReadableReasoning(message, delta) {
  const reasoningContent = nonEmptyString(message.reasoning_content);
  if (reasoningContent) {
    delta.reasoning_content = reasoningContent;
    return true;
  }
  const reasoning = nonEmptyString(message.reasoning);
  if (reasoning) {
    delta.reasoning = reasoning;
    return true;
  }
  return false;
}

function addUnsupportedReasoning(message, delta) {
  const reasoningContent = getUnsupportedReasoningValue(message);
  if (reasoningContent) delta.reasoning_content = reasoningContent;
}

function buildReasoningDelta(message) {
  const delta = {};
  if (Array.isArray(message.reasoning_details)) {
    delta.reasoning_details = message.reasoning_details;
  }
  if (!addReadableReasoning(message, delta)) {
    addUnsupportedReasoning(message, delta);
  }
  return Object.keys(delta).length > 0 ? delta : null;
}

function sseEvent(payload) {
  return `data: ${JSON.stringify(payload)}\n\n`;
}

/**
 * Convert a complete OpenAI chat-completion JSON body into an equivalent
 * OpenAI SSE (`chat.completion.chunk`) stream. Returns "" if `jsonText` is not
 * a parseable chat-completion with at least one choice.
 * @param {string} jsonText
 * @returns {string}
 */
export function synthesizeOpenAiSseFromJson(jsonText) {
  let parsed;
  try {
    parsed = JSON.parse(jsonText);
  } catch {
    return "";
  }
  if (!isRecord(parsed)) return "";

  const choices = parsed.choices;
  if (!Array.isArray(choices) || choices.length === 0) return "";

  const id = typeof parsed.id === "string" && parsed.id ? parsed.id : "chatcmpl-9router-sse";
  const created = typeof parsed.created === "number" ? parsed.created : 0;
  const model = typeof parsed.model === "string" ? parsed.model : "";
  const base = { id, object: "chat.completion.chunk", created, model };

  let out = "";
  let emittedAny = false;

  choices.forEach((choice, fallbackIndex) => {
    if (!isRecord(choice)) return;
    const index = typeof choice.index === "number" ? choice.index : fallbackIndex;
    const message = isRecord(choice.message) ? choice.message : {};

    // Emit role, reasoning_content, content and tool_calls as SEPARATE
    // sequential deltas — same shape a real reasoning model streams. Combining
    // them in one delta caused the openai→openai translator to re-split and
    // DUPLICATE reasoning_content across chunks; separate deltas pass cleanly.
    const role = typeof message.role === "string" ? message.role : "assistant";
    const emitDelta = (delta) => {
      out += sseEvent({ ...base, choices: [{ index, delta, finish_reason: null }] });
    };

    emitDelta({ role });
    const reasoningDelta = buildReasoningDelta(message);
    if (reasoningDelta) emitDelta(reasoningDelta);
    if (typeof message.content === "string" && message.content.length > 0) {
      emitDelta({ content: message.content });
    }
    if (Array.isArray(message.tool_calls) && message.tool_calls.length > 0) {
      emitDelta({ tool_calls: message.tool_calls });
    }

    const finishReason = normalizeOpenAICompatibleFinishReasonString(choice.finish_reason);
    const finalChunk = { ...base, choices: [{ index, delta: {}, finish_reason: finishReason }] };
    if (isRecord(parsed.usage)) finalChunk.usage = parsed.usage;
    out += sseEvent(finalChunk);
    emittedAny = true;
  });

  if (!emittedAny) return "";
  out += "data: [DONE]\n\n";
  return out;
}