// ponytail: Ollama Cloud error body is stable JSON.
// {"error":"this model requires a subscription"} → HTTP 403.
const PATTERN = "this model requires a subscription";

export function isOllamaSubscriptionError(status, errorText) {
  if (status !== 403) return false;
  const text = typeof errorText === "string"
    ? errorText
    : (() => { try { return JSON.stringify(errorText) ?? ""; } catch { return ""; } })();
  return text.toLowerCase().includes(PATTERN);
}
