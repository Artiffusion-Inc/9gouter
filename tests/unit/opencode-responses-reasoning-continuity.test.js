import { describe, expect, it } from "vitest";
import { OpenCodeGoExecutor } from "../../open-sse/executors/opencode-go.js";
import { OpenCodeZenExecutor } from "../../open-sse/executors/opencode-zen.js";

const credentials = { runtimeTransport: { format: "openai-responses" } };

function history() {
  return [
    { type: "message", role: "user", content: [{ type: "input_text", text: "Read the file" }] },
    {
      type: "reasoning", id: "rs_previous", encrypted_content: "continuity-token",
      summary: [{ type: "summary_text", text: "Short summary" }],
      content: [{ type: "reasoning_text", text: "Full reasoning needed for tool continuation" }],
    },
    { type: "message", role: "assistant", content: [{ type: "output_text", text: "Reading it." }], reasoning_encrypted_content: "message-token" },
    { type: "function_call", call_id: "call_read", name: "read", arguments: "{\"path\":\"a.txt\"}" },
    { type: "function_call_output", call_id: "call_read", output: "file content" },
  ];
}

for (const [provider, Executor] of [["opencode-go", OpenCodeGoExecutor], ["opencode-zen", OpenCodeZenExecutor]]) {
  describe(`${provider} Responses reasoning continuity`, () => {
    for (const model of ["deepseek-v4.1-flash", "deepseek-v4-flash(high)", "grok-4.6"]) {
      it(`preserves complete prior reasoning and tool history for ${model}`, () => {
        const input = history();
        const expected = structuredClone(input);
        const out = new Executor().transformRequest(model, { input }, true, credentials);
        expect(out.input).toEqual(expected);
        expect(input).toEqual(expected);
      });
    }

    for (const model of ["muse-spark-1.3-contributor", "muse-spark-1.3-contributor-free(high)"]) {
      it(`retains the encrypted-history workaround for ${model}`, () => {
        const out = new Executor().transformRequest(model, { input: history() }, true, credentials);
        expect(out.input.some(item => item.type === "reasoning")).toBe(false);
        expect(out.input.map(item => item.type)).toEqual(["message", "message", "function_call", "function_call_output"]);
        expect(JSON.stringify(out.input)).not.toContain("continuity-token");
        expect(JSON.stringify(out.input)).not.toContain("message-token");
      });
    }
  });
}
