// ponytail: ported from OmniRoute reasoningFields.ts (minimal subset).
// Only the helpers jsonToSse.js needs. For full reasoning extraction see
// translator/concerns/reasoning.js (extractReasoningText).

function asReasoningRecord(value) {
  return value && typeof value === "object" && !Array.isArray(value) ? value : {};
}

function nonEmptyString(value) {
  return typeof value === "string" && value.length > 0 ? value : "";
}

// Reasoning value from less-common vendor fields, used as fallback when
// reasoning_content / reasoning are absent.
export function getUnsupportedReasoningValue(value) {
  const record = asReasoningRecord(value);
  return (
    nonEmptyString(record.reasoning_text) ||
    nonEmptyString(record.reasoning_content_polyfill) ||
    nonEmptyString(record.thinking) ||
    ""
  );
}

export function asReasoningRecordPublic(value) {
  return asReasoningRecord(value);
}

export function nonEmptyStringPublic(value) {
  return nonEmptyString(value);
}