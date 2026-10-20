import { describe, expect, it } from "vitest";
import { OpenCodeGoExecutor } from "../../open-sse/executors/opencode-go.js";
import { applyThinking } from "../../open-sse/translator/concerns/thinkingUnified.js";
import { resolveTransport } from "../../open-sse/services/provider.js";

const MODEL = "deepseek-v4-flash";
const executor = new OpenCodeGoExecutor();

function request() {
  const body = { model: MODEL, input: "Say OK", reasoning: { effort: "high", summary: "auto" } };
  applyThinking("openai-responses", MODEL, body, "opencode-go");
  return body;
}

describe("OpenCode Go DeepSeek reasoning on the selected endpoint", () => {
  it("restores Responses reasoning after provider thinking normalization", () => {
    const credentials = { runtimeTransport: resolveTransport("opencode-go", "openai-responses") };
    const out = executor.transformRequest(MODEL, request(), true, credentials);
    expect(out.reasoning).toEqual({ effort: "high", summary: "auto" });
    expect(out).not.toHaveProperty("thinking");
    expect(out).not.toHaveProperty("reasoning_effort");
  });

  it("preserves DeepSeek chat thinking on the chat endpoint", () => {
    const body = request();
    delete body.input;
    body.messages = [{ role: "user", content: "Say OK" }];
    const out = executor.transformRequest(MODEL, body, true, {
      runtimeTransport: resolveTransport("opencode-go", "openai"),
    });
    expect(out.thinking).toEqual({ type: "enabled" });
    expect(out.reasoning_effort).toBe("high");
    expect(out).not.toHaveProperty("reasoning");
  });

  it("removes chat thinking from a statically Responses-only model", () => {
    const out = executor.transformRequest("muse-spark-1.3-contributor", {
      input: "Say OK", thinking: { type: "enabled" }, reasoning_effort: "high",
    }, true, {});
    expect(out.reasoning).toEqual({ effort: "high", summary: "auto" });
    expect(out).not.toHaveProperty("thinking");
    expect(out).not.toHaveProperty("reasoning_effort");
  });
});
