// ponytail: ported from OmniRoute proxyFamily.ts + proxyFamilyResolve.ts.
// IPv4/IPv6 family helpers + fail-closed guarantee for family-pinned proxy
// egress (refuse early if hostname has no record in required family).
// Self-contained: node:net + node:dns/promises.

import { isIP } from "node:net";
import dns from "node:dns/promises";

/** @typedef {"auto"|"ipv4"|"ipv6"} ProxyFamily */

/** Remove surrounding brackets from an IPv6 literal host (`[::1]` -> `::1`). */
export function stripIpv6Brackets(host) {
  if (typeof host !== "string") return "";
  if (host.startsWith("[") && host.endsWith("]")) return host.slice(1, -1);
  return host;
}

/** 4 / 6 if the host is an IP literal (brackets tolerated), null if hostname. */
export function detectIpLiteralFamily(host) {
  const bare = stripIpv6Brackets(host);
  const v = isIP(bare);
  return v === 0 ? null : v;
}

/** Normalize a stored family directive; anything unknown means "auto". */
export function parseProxyFamily(value) {
  return value === "ipv4" || value === "ipv6" ? value : "auto";
}

/**
 * Fail-closed guarantee for an IPv6-only (or IPv4-only) proxy given as a
 * hostname: refuse early if the hostname has no record in the required family.
 * No-op for IP literals (their family is intrinsic).
 *
 * @param {string} host
 * @param {4|6} family
 * @param {(hostname: string) => Promise<Array<{address: string, family: number}>>} [lookupFn]
 * @returns {Promise<void>}
 */
export async function assertHostnameSupportsFamily(host, family, lookupFn) {
  const lookup = lookupFn || ((hostname) => dns.lookup(hostname, { all: true }));
  if (detectIpLiteralFamily(host) !== null) return;
  let records;
  try {
    records = await lookup(stripIpv6Brackets(host));
  } catch (err) {
    throw new Error(
      `[ProxyFamily] DNS resolution failed for ${host}; refusing to egress (fail-closed): ${
        err instanceof Error ? err.message : String(err)
      }`
    );
  }
  const hasFamily = records.some((r) => r.family === family);
  if (!hasFamily) {
    throw new Error(
      `[ProxyFamily] Proxy host ${host} has no ${family === 6 ? "IPv6 (AAAA)" : "IPv4 (A)"} record; refusing ${
        family === 6 ? "IPv6" : "IPv4"
      }-only egress (fail-closed)`
    );
  }
}