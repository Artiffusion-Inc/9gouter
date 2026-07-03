import { describe, it, expect } from "vitest";
import { isOllamaSubscriptionError } from "open-sse/utils/ollamaSubscriptionError.js";

describe("isOllamaSubscriptionError", () => {
  it("matches Ollama 403 subscription JSON body", () => {
    expect(isOllamaSubscriptionError(403, '{"error":"this model requires a subscription"}')).toBe(true);
  });

  it("matches plain-string Ollama 403 subscription body", () => {
    expect(isOllamaSubscriptionError(403, "this model requires a subscription")).toBe(true);
  });

  it("ignores 403 with different message", () => {
    expect(isOllamaSubscriptionError(403, "rate limit exceeded")).toBe(false);
  });

  it("ignores non-403 status", () => {
    expect(isOllamaSubscriptionError(429, "this model requires a subscription")).toBe(false);
    expect(isOllamaSubscriptionError(200, "this model requires a subscription")).toBe(false);
  });

  it("handles null / undefined / object errorText", () => {
    expect(isOllamaSubscriptionError(403, null)).toBe(false);
    expect(isOllamaSubscriptionError(403, undefined)).toBe(false);
    expect(isOllamaSubscriptionError(403, { error: "this model requires a subscription" })).toBe(true);
  });
});
