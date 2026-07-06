// ponytail: ported from OmniRoute streamReadinessPolicy.ts.
// Adaptive stream-readiness (TTFT) timeout: bumps base timeout for large
// history / tool-heavy / large payload, and for high-reasoning Codex GPT-5.x
// cold-starts. Capped at maxTimeoutMs.

const DEFAULT_MAX_TIMEOUT_MS = 180_000;
const LARGE_ITEM_THRESHOLD = 150;
const VERY_LARGE_ITEM_THRESHOLD = 400;
const TOOL_HEAVY_THRESHOLD = 15;
const LARGE_CHAR_THRESHOLD = 250_000;
const VERY_LARGE_CHAR_THRESHOLD = 750_000;

function countArrayField(body, field) {
  const value = body?.[field];
  return Array.isArray(value) ? value.length : 0;
}

function estimateBodyChars(body) {
  if (!body) return 0;
  try {
    return JSON.stringify(body).length;
  } catch {
    return 0;
  }
}

function isCodexGpt5x(provider, model) {
  const p = (provider || "").toLowerCase();
  const m = (model || "").toLowerCase();
  return p === "codex" && /gpt-5(\.\d+)?/.test(m);
}

// High-reasoning Codex GPT-5.x cold-starts at ~78s TTFB even for tiny prompts.
// Detect "high" via model alias suffix or body reasoning effort field.
function isHighReasoningEffort(model, body) {
  const m = (model || "").toLowerCase();
  if (/-high\b/.test(m) || m.endsWith("-high")) return true;

  const direct = body?.["reasoning_effort"];
  if (typeof direct === "string") return direct.toLowerCase() === "high";

  const reasoning = body?.["reasoning"];
  if (reasoning && typeof reasoning === "object") {
    const nested = reasoning["effort"];
    if (typeof nested === "string") return nested.toLowerCase() === "high";
  }
  return false;
}

/**
 * Resolve the stream-readiness (TTFT) timeout for a request.
 * @param {object} input
 * @param {number} input.baseTimeoutMs   - base readiness budget (0 = disabled)
 * @param {string} [input.provider]
 * @param {string} [input.model]
 * @param {object} [input.body]          - request body (messages/input/tools inspected)
 * @param {number} [input.maxTimeoutMs]  - hard cap; defaults to 180s
 * @returns {{ timeoutMs: number, baseTimeoutMs: number, reasons: string[] }}
 */
export function resolveStreamReadinessTimeout(input) {
  const baseTimeoutMs = Math.max(0, Math.floor(input.baseTimeoutMs || 0));
  if (baseTimeoutMs <= 0) {
    return { timeoutMs: baseTimeoutMs, baseTimeoutMs, reasons: ["disabled"] };
  }

  const maxTimeoutMs = Math.max(baseTimeoutMs, input.maxTimeoutMs ?? DEFAULT_MAX_TIMEOUT_MS);
  const reasons = [];
  let timeoutMs = baseTimeoutMs;

  const inputCount = countArrayField(input.body, "input");
  const messageCount = countArrayField(input.body, "messages");
  const itemCount = Math.max(inputCount, messageCount);
  const toolCount = countArrayField(input.body, "tools");
  const estimatedChars = estimateBodyChars(input.body);
  const codexGpt5x = isCodexGpt5x(input.provider, input.model);
  const codexHighReasoning = codexGpt5x && isHighReasoningEffort(input.model, input.body);

  if (itemCount > VERY_LARGE_ITEM_THRESHOLD) {
    timeoutMs += 45_000;
    reasons.push("very_large_history");
  } else if (itemCount > LARGE_ITEM_THRESHOLD) {
    timeoutMs += 20_000;
    reasons.push("large_history");
  }

  if (toolCount >= TOOL_HEAVY_THRESHOLD) {
    timeoutMs += 15_000;
    reasons.push("tool_heavy");
  }

  if (estimatedChars > VERY_LARGE_CHAR_THRESHOLD) {
    timeoutMs += 45_000;
    reasons.push("very_large_payload");
  } else if (estimatedChars > LARGE_CHAR_THRESHOLD) {
    timeoutMs += 20_000;
    reasons.push("large_payload");
  }

  // #3825: high-reasoning Codex GPT-5.x +30s budget must fire unconditionally;
  // 80s base alone produced intermittent 504s at the readiness window.
  if (codexHighReasoning) {
    timeoutMs += 30_000;
    reasons.push("codex_gpt_5_5_high_reasoning");
  } else if (
    codexGpt5x &&
    (itemCount > LARGE_ITEM_THRESHOLD || toolCount >= TOOL_HEAVY_THRESHOLD)
  ) {
    timeoutMs += 30_000;
    reasons.push("codex_gpt_5_5_large_responses");
  }

  timeoutMs = Math.min(timeoutMs, maxTimeoutMs);
  if (timeoutMs === baseTimeoutMs) reasons.push("base");

  return { timeoutMs, baseTimeoutMs, reasons };
}