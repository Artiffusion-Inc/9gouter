import { OPENAI_BLOCK } from "../schema/index.js";

// Collapse an OpenAI content-part array: pure-text arrays become a `\n`-joined
// string (matches the upstream's string-content convention for text-only
// messages). Mixed arrays pass through unchanged.
export function collapseTextParts(parts) {
  if (parts.length === 0) return "";
  const allText = parts.every((p) => p && p.type === OPENAI_BLOCK.TEXT && typeof p.text === "string");
  return allText ? parts.map((p) => p.text).join("\n") : parts;
}
