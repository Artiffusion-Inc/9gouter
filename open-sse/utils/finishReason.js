// ponytail: ported from OmniRoute finishReason.ts.
// Normalize unknown vendor finish_reason strings to the OpenAI canonical set.
// Distinct from translator/concerns/finishReason.js (which maps known formats).

const OPENAI_FINISH_REASONS = new Set([
  "stop",
  "length",
  "tool_calls",
  "content_filter",
  "function_call",
]);

const SAFETY_FINISH_REASONS = new Set([
  "safety",
  "recitation",
  "blocklist",
  "prohibited_content",
  "content_filtered",
  "policy_violation",
  "malformed_response",
]);

export function normalizeOpenAICompatibleFinishReason(value) {
  if (typeof value !== "string") return value;
  const normalized = value.toLowerCase();
  if (OPENAI_FINISH_REASONS.has(normalized)) return normalized;
  if (normalized === "max_tokens") return "length";
  if (SAFETY_FINISH_REASONS.has(normalized)) return "content_filter";
  return normalized;
}

export function normalizeOpenAICompatibleFinishReasonString(value, fallback = "stop") {
  const normalized = normalizeOpenAICompatibleFinishReason(value);
  return typeof normalized === "string" && normalized ? normalized : fallback;
}