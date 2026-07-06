// ponytail: ported from OmniRoute upstreamResponseHeaders.ts.
// 9router builds outbound Responses with its own SSE_HEADERS, so upstream
// encoding headers do NOT leak today. This helper is a guard for any future
// path that forwards upstream headers verbatim, plus keeps fetch-logged
// headers clean.

const STRIP_HEADER_NAMES = new Set([
  "content-encoding",
  "content-length",
  "transfer-encoding",
]);

// Return a new Headers instance with stale encoding/length headers removed.
// Does not mutate the input.
export function stripStaleEncodingHeaders(input) {
  const out = new Headers(input);
  for (const name of STRIP_HEADER_NAMES) out.delete(name);
  return out;
}

// Return a new entries array with stale encoding/length headers removed and
// (optionally) additional header names removed. Case-insensitive.
export function filterUpstreamResponseHeaderEntries(entries, extraToStrip = []) {
  const drop = new Set(STRIP_HEADER_NAMES);
  for (const h of extraToStrip) drop.add(h.toLowerCase());
  const result = [];
  for (const [k, v] of entries) {
    if (!drop.has(k.toLowerCase())) result.push([k, v]);
  }
  return result;
}

export const STRIP_UPSTREAM_HEADER_NAMES = STRIP_HEADER_NAMES;