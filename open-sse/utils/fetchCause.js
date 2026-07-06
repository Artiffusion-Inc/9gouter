// ponytail: ported from OmniRoute proxyFetch.ts describeFetchCause.
// Flatten a fetch error's .cause chain (and Happy-Eyeballs AggregateError
// sub-errors) into one diagnostic line: code/syscall/errno/address:port +
// truncated message. undici/native reject with bare `TypeError: fetch failed`
// whose real reason hides in .cause; surfacing it makes dispatcher-failure
// bursts diagnosable. Never includes a stack trace. Pure + testable.

/** @param {unknown} err @returns {string} */
export function describeFetchCause(err) {
  const parts = [];
  const seen = new Set();
  let cur = err;
  for (let depth = 0; cur && depth < 5 && !seen.has(cur); depth++) {
    seen.add(cur);
    const e = cur;
    const seg = [
      typeof e.name === "string" && e.name !== "Error" ? e.name : null,
      typeof e.message === "string" ? e.message.slice(0, 160) : null,
      e.code != null ? `code=${String(e.code)}` : null,
      e.syscall != null ? `syscall=${String(e.syscall)}` : null,
      e.errno != null ? `errno=${String(e.errno)}` : null,
      e.address != null
        ? `address=${String(e.address)}${e.port != null ? `:${String(e.port)}` : ""}`
        : null,
    ]
      .filter(Boolean)
      .join(" ");
    if (seg) parts.push(seg);
    if (Array.isArray(e.errors)) {
      for (const sub of e.errors.slice(0, 4)) {
        const s = sub || {};
        const subSeg = [
          s.code != null ? `code=${String(s.code)}` : null,
          s.syscall != null ? `syscall=${String(s.syscall)}` : null,
          s.address != null
            ? `address=${String(s.address)}${s.port != null ? `:${String(s.port)}` : ""}`
            : null,
        ]
          .filter(Boolean)
          .join(" ");
        if (subSeg) parts.push(`↳ ${subSeg}`);
        else if (typeof s.message === "string") parts.push(`↳ ${s.message.slice(0, 80)}`);
      }
    }
    cur = e.cause;
  }
  return parts.join(" | ") || String(err);
}