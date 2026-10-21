import { describe, expect, it } from "vitest";
import { OpenCodeGoExecutor } from "../../open-sse/executors/opencode-go.js";
import { OpenCodeZenExecutor } from "../../open-sse/executors/opencode-zen.js";

const credentials = { runtimeTransport: { format: "openai-responses" } };

describe("OpenCode Go DeepSeek saved summary compatibility", () => {
  const summary = [{ type: "summary_text", text: "Saved reasoning from a Pi signature." }];

  for (const content of [undefined, [], [{ type: "reasoning_text", text: "" }]]) {
    it(`restores actual summary text when content is ${JSON.stringify(content)}`, () => {
      const item = { type: "reasoning", id: "rs_pi", summary, encrypted_content: "preserved", ...(content === undefined ? {} : { content }) };
      const expected = structuredClone(item);
      const out = new OpenCodeGoExecutor().transformRequest("deepseek-v4.1-flash(high)", { input: [item] }, true, credentials);
      expect(out.input[0]).toEqual({ ...expected, content: [{ type: "reasoning_text", text: summary[0].text }] });
      expect(item).toEqual(expected);
    });
  }

  for (const item of [
    { type: "reasoning", encrypted_content: "opaque-only" },
    { type: "reasoning", summary: [{ type: "summary_text", text: "  " }] },
    { type: "reasoning", summary, content: [{ type: "reasoning_text", text: "Original complete reasoning" }] },
  ]) {
    it(`does not invent or overwrite reasoning for ${JSON.stringify(item)}`, () => {
      const out = new OpenCodeGoExecutor().transformRequest("deepseek-v4.1-flash", { input: [structuredClone(item)] }, true, credentials);
      expect(out.input).toEqual([item]);
    });
  }

  it("does not reinterpret another model's reasoning summaries", () => {
    const item = { type: "reasoning", summary };
    const input = [item, { type: "function_call_output", call_id: "old", output: "done" },
      { type: "function_call", call_id: "new", name: "read", arguments: "{}" }];
    const out = new OpenCodeGoExecutor().transformRequest("grok-4.6", { input: structuredClone(input) }, true, credentials);
    expect(out.input).toEqual(input);
  });

  it("represents missing reasoning explicitly for completed assistant tool groups without losing history", () => {
    const input = history();
    input.push(
      { type: "message", role: "assistant", content: [{ type: "output_text", text: "Next check" }] },
      { type: "function_call", call_id: "next1", name: "read", arguments: "{}" },
      { type: "function_call", call_id: "next2", name: "read", arguments: "{}" },
      { type: "function_call_output", call_id: "next1", output: "one" },
      { type: "function_call_output", call_id: "next2", output: "two" },
      { type: "custom_tool_call", call_id: "patch", name: "apply_patch", input: "patch text" },
      { type: "custom_tool_call_output", call_id: "patch", output: "done" },
    );
    const original = structuredClone(input);
    const empty = { type: "reasoning", summary: [], content: [{ type: "reasoning_text", text: "" }] };
    const expected = [...original.slice(0, 5), empty, ...original.slice(5, 10), empty, ...original.slice(10)];
    const executor = new OpenCodeGoExecutor();
    const out = executor.transformRequest("deepseek-v4.1-flash", { input }, true, credentials);
    expect(out.input).toEqual(expected);
    expect(input).toEqual(original);
    const repeated = executor.transformRequest("deepseek-v4.1-flash", { input: out.input }, true, credentials);
    expect(repeated.input).toEqual(expected);
  });

  it("resets missing-reasoning groups at user, developer and system boundaries", () => {
    for (const role of ["user", "developer", "system"]) {
      const reasoning = { type: "reasoning", encrypted_content: "keep-existing" };
      const boundary = { role, content: "Continue" };
      const reply = { role: "assistant", content: "Answer" };
      const out = new OpenCodeGoExecutor().transformRequest("deepseek-v4.1-flash", { input: [reasoning, boundary, reply] }, true, credentials);
      expect(out.input).toEqual([reasoning, boundary, { type: "reasoning", summary: [], content: [{ type: "reasoning_text", text: "" }] }, reply]);
    }
  });
});

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
