// ponytail: ported from OmniRoute src/lib/proxyHealth.ts (T14 Proxy Fast-Fail).
// When a configured HTTP/SOCKS5 proxy is unreachable, every request through
// the gateway used to wait for the full connect timeout before failing.
// This detects dead proxies in <2s via a quick TCP connection check, caching
// the result to avoid overhead per request. Inflight dedup collapses
// concurrent probes for the same proxy into one.
// Self-contained: node:net + proxyFamily stripIpv6Brackets.

import { createConnection } from "node:net";
import { stripIpv6Brackets } from "./proxyFamily.js";

const FAST_FAIL_TIMEOUT_MS = parseInt(process.env.PROXY_FAST_FAIL_TIMEOUT_MS ?? "2000", 10);
const HEALTH_CACHE_TTL_MS = parseInt(process.env.PROXY_HEALTH_CACHE_TTL_MS ?? "30000", 10);
const UNHEALTHY_CACHE_TTL_MS = parseInt(process.env.PROXY_HEALTH_UNHEALTHY_CACHE_TTL_MS ?? "2000", 10);

/** @type {Map<string, {healthy: boolean, checkedAt: number, ttlMs: number})} */
const proxyHealthCache = new Map();
/** @type {Map<string, Promise<boolean>>} */
const proxyHealthInflight = new Map();

/** @typedef {(host: string, port: number, timeoutMs: number) => Promise<boolean>} TcpCheck */
/** @type {TcpCheck} */
let tcpCheckImpl = tcpCheck;

/**
 * Perform a fast TCP check to see if a proxy host:port is reachable.
 * Results are cached to avoid checking every request.
 * @param {string} proxyUrl - Full proxy URL, e.g. http://user:pass@1.2.3.4:8080
 * @param {number} [timeoutMs] - TCP connection timeout (default PROXY_FAST_FAIL_TIMEOUT_MS)
 * @param {number} [cacheTtlMs] - How long to cache a healthy result (default PROXY_HEALTH_CACHE_TTL_MS)
 * @returns {Promise<boolean>}
 */
export async function isProxyReachable(proxyUrl, timeoutMs = FAST_FAIL_TIMEOUT_MS, cacheTtlMs = HEALTH_CACHE_TTL_MS) {
  const cached = proxyHealthCache.get(proxyUrl);
  if (cached && Date.now() - cached.checkedAt < cached.ttlMs) return cached.healthy;

  let url;
  try {
    url = new URL(proxyUrl);
  } catch {
    proxyHealthCache.set(proxyUrl, { healthy: false, checkedAt: Date.now(), ttlMs: cacheTtlMs });
    return false;
  }

  const host = stripIpv6Brackets(url.hostname);
  const port = parseInt(url.port || defaultPortForScheme(url.protocol), 10);
  if (!host || Number.isNaN(port)) {
    proxyHealthCache.set(proxyUrl, { healthy: false, checkedAt: Date.now(), ttlMs: cacheTtlMs });
    return false;
  }

  const existingProbe = proxyHealthInflight.get(proxyUrl);
  if (existingProbe) return existingProbe;

  const probe = tcpCheckImpl(host, port, timeoutMs).then((healthy) => {
    proxyHealthCache.set(proxyUrl, {
      healthy,
      checkedAt: Date.now(),
      ttlMs: healthy ? cacheTtlMs : Math.min(cacheTtlMs, UNHEALTHY_CACHE_TTL_MS),
    });
    return healthy;
  });

  proxyHealthInflight.set(proxyUrl, probe);
  try {
    return await probe;
  } finally {
    if (proxyHealthInflight.get(proxyUrl) === probe) proxyHealthInflight.delete(proxyUrl);
  }
}

/** Cached health status without re-checking. null if no fresh entry. */
export function getCachedProxyHealth(proxyUrl) {
  const cached = proxyHealthCache.get(proxyUrl);
  if (!cached) return null;
  if (Date.now() - cached.checkedAt >= cached.ttlMs) return null;
  return cached.healthy;
}

/** Force re-check on next call. */
export function invalidateProxyHealth(proxyUrl) {
  proxyHealthCache.delete(proxyUrl);
  proxyHealthInflight.delete(proxyUrl);
}

/** All cached entries (dashboard display). */
export function getAllProxyHealthStatuses() {
  const now = Date.now();
  return [...proxyHealthCache.entries()].map(([proxyUrl, entry]) => ({
    proxyUrl,
    healthy: entry.healthy,
    checkedAt: entry.checkedAt,
    stale: now - entry.checkedAt >= entry.ttlMs,
  }));
}

// ─── Internals ────────────────────────────────────────────────────────────────

function defaultPortForScheme(protocol) {
  switch (protocol.replace(":", "").toLowerCase()) {
    case "https": return "443";
    case "socks5":
    case "socks5h": return "1080";
    case "http":
    default: return "8080";
  }
}

/** @type {TcpCheck} */
function tcpCheck(host, port, timeoutMs) {
  return new Promise((resolve) => {
    const socket = createConnection({ host, port }, () => {
      socket.destroy();
      resolve(true);
    });
    socket.setTimeout(timeoutMs);
    socket.on("error", () => resolve(false));
    socket.on("timeout", () => {
      socket.destroy();
      resolve(false);
    });
  });
}