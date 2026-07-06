// ponytail: ported from OmniRoute proxyFallback.ts, adapted to 9router's
// proxyPoolsRepo (replaces OmniRoute localDb/registry). On direct fetch fail,
// gather proxy candidates from: active proxy pools (proxyPoolsRepo) + env
// (HTTP_PROXY/HTTPS_PROXY/ALL_PROXY, honouring NO_PROXY). Test them in parallel
// against the target URL, return first working. Cache per target URL (path-
// aware) for 5 min; negative result cached too, to avoid re-probing bursts.
// Auto-selection is opt-in via PROXY_AUTO_SELECT_ENABLED env (defaults OFF —
// without the gate a single pool proxy would silently become a global fallback).

import { fetch as undiciFetch } from "undici";
import { createProxyDispatcher, normalizeProxyUrl } from "./proxyDispatcher.js";
import { isProxyReachable } from "./proxyHealth.js";

const CACHE_TTL_MS = 5 * 60 * 1000;

/** @type {Map<string, {proxyUrl: string, expiresAt: number}>} */
const PROXY_FALLBACK_CACHE = new Map();

/** @type {Map<string, number>} env proxy precedence for the test target */
const PROBE_TIMEOUT_MS = parseInt(process.env.PROXY_FALLBACK_PROBE_TIMEOUT_MS ?? "3000", 10);

function isAutoSelectEnabled() {
  const v = process.env.PROXY_AUTO_SELECT_ENABLED;
  return v === "true" || v === "1" || v === "yes";
}

function shouldBypassByNoProxy(targetUrl, noProxyValue) {
  const noProxy = (noProxyValue || "").trim();
  if (!noProxy) return false;
  let hostname;
  try { hostname = new URL(targetUrl).hostname.toLowerCase(); } catch { return false; }
  const patterns = noProxy.split(",").map((p) => p.trim().toLowerCase()).filter(Boolean);
  return patterns.some((pattern) => {
    if (pattern === "*") return true;
    if (pattern.startsWith(".")) return hostname.endsWith(pattern) || hostname === pattern.slice(1);
    return hostname === pattern || hostname.endsWith(`.${pattern}`);
  });
}

/** Resolve env proxy URL (HTTP_PROXY/HTTPS_PROXY/ALL_PROXY) for a target, honouring NO_PROXY. */
export function resolveEnvProxyUrl(targetUrl) {
  if (shouldBypassByNoProxy(targetUrl, process.env.NO_PROXY || process.env.no_proxy)) return null;
  let protocol;
  try { protocol = new URL(targetUrl).protocol; } catch { return null; }
  const proxyUrl =
    protocol === "https:"
      ? process.env.HTTPS_PROXY || process.env.https_proxy || process.env.ALL_PROXY || process.env.all_proxy
      : process.env.HTTP_PROXY || process.env.http_proxy || process.env.ALL_PROXY || process.env.all_proxy;
  if (!proxyUrl) return null;
  const parsed = normalizeProxyUrl(proxyUrl);
  return parsed ? proxyUrlForTarget(parsed) : null;
}

function proxyUrlForTarget(parsed) {
  const auth = parsed.username ? `${parsed.username}:${parsed.password || ""}@` : "";
  const port = parsed.port ? `:${parsed.port}` : "";
  return `${parsed.protocol}//${auth}${parsed.host}${port}`;
}

function cacheKeyForTarget(targetHostname, targetUrl) {
  try {
    const url = new URL(targetUrl);
    const normalizedPath = `${url.pathname || "/"}${url.search}`;
    return `${url.protocol}//${url.host}${normalizedPath}`;
  } catch {
    return String(targetHostname || "").toLowerCase();
  }
}

/**
 * Collect proxy candidates from active proxy pools + env proxy.
 * @param {string} [targetUrl] - Required to resolve env proxy (protocol-specific).
 * @returns {Promise<string[]>} deduplicated normalized proxy URLs.
 */
export async function getProxyCandidates(targetUrl) {
  const candidates = new Set();

  // Active proxy pools (9router proxyPoolsRepo).
  try {
    const { getProxyPools } = await import("../../src/lib/db/repos/proxyPoolsRepo.js");
    const pools = await getProxyPools({ isActive: true });
    for (const p of pools) {
      const url = p.proxyUrl || (p.type && p.host ? `${p.type}://${p.host}${p.port ? `:${p.port}` : ""}` : null);
      if (url) {
        const parsed = normalizeProxyUrl(url);
        if (parsed) candidates.add(proxyUrlForTarget(parsed));
      }
    }
  } catch {
    // DB may not be initialized (e.g. CLI context) — skip pool source.
  }

  // Env proxy.
  if (targetUrl) {
    try {
      const envProxy = resolveEnvProxyUrl(targetUrl);
      if (envProxy) candidates.add(envProxy);
    } catch { /* ignore */ }
  }

  return Array.from(candidates);
}

/**
 * Test a single proxy against a target URL via a lightweight HEAD request.
 * Any response (including 4xx) means the proxy can reach the target.
 * @param {string} proxyUrl
 * @param {string} targetUrl
 * @param {number} [timeoutMs]
 * @returns {Promise<{ok: boolean, latencyMs: number|null}>}
 */
export async function testSingleProxy(proxyUrl, targetUrl, timeoutMs = PROBE_TIMEOUT_MS) {
  const start = Date.now();
  try {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), timeoutMs);
    const dispatcher = createProxyDispatcher(proxyUrl);
    await undiciFetch(targetUrl, {
      method: "HEAD",
      signal: controller.signal,
      dispatcher,
      headers: { "User-Agent": "9router/1.0" },
    });
    clearTimeout(timeout);
    return { ok: true, latencyMs: Date.now() - start };
  } catch {
    return { ok: false, latencyMs: null };
  }
}

/**
 * Bulk-test proxies against a target. No caching (manual API use).
 * @param {string} targetUrl
 * @param {string[]} proxyUrls
 */
export async function testProxiesAgainstTarget(targetUrl, proxyUrls) {
  if (proxyUrls.length === 0) return [];
  const results = await Promise.allSettled(
    proxyUrls.map(async (proxyUrl) => ({ proxyUrl, ...(await testSingleProxy(proxyUrl, targetUrl)) }))
  );
  return results.map((r) => (r.status === "fulfilled" ? r.value : { proxyUrl: "unknown", ok: false, latencyMs: null }));
}

/**
 * Find a working proxy for the given target. Candidates tested in parallel;
 * first responder wins. Cached per target URL for CACHE_TTL_MS.
 * @param {string} targetHostname
 * @param {string} targetUrl
 * @returns {Promise<string|null>}
 */
export async function findWorkingProxy(targetHostname, targetUrl) {
  if (!targetHostname) return null;
  const cacheKey = cacheKeyForTarget(targetHostname, targetUrl);

  const cached = PROXY_FALLBACK_CACHE.get(cacheKey);
  if (cached) {
    if (cached.expiresAt > Date.now()) return cached.proxyUrl || null;
    PROXY_FALLBACK_CACHE.delete(cacheKey);
  }

  const candidates = await getProxyCandidates(targetUrl);
  if (candidates.length === 0) return null;

  const results = await Promise.allSettled(
    candidates.map(async (proxyUrl) => {
      // Fast-fail: dead proxies (TCP unreachable) skip the HEAD probe.
      if (!(await isProxyReachable(proxyUrl))) return { proxyUrl, ok: false };
      const { ok } = await testSingleProxy(proxyUrl, targetUrl);
      return { proxyUrl, ok };
    })
  );

  const working = results.find((r) => r.status === "fulfilled" && r.value.ok);
  if (working && working.status === "fulfilled") {
    const proxyUrl = working.value.proxyUrl;
    PROXY_FALLBACK_CACHE.set(cacheKey, { proxyUrl, expiresAt: Date.now() + CACHE_TTL_MS });
    return proxyUrl;
  }

  // All failed — cache negative result.
  PROXY_FALLBACK_CACHE.set(cacheKey, { proxyUrl: "", expiresAt: Date.now() + CACHE_TTL_MS });
  return null;
}

/**
 * Last-resort auto-selection fallback. Returns a proxy URL reachable to a
 * well-known AI endpoint, or null. Opt-in via PROXY_AUTO_SELECT_ENABLED.
 * @param {string} [connectionId] reserved
 * @returns {Promise<string|null>}
 */
export async function selectWorkingProxyFallback(connectionId) {
  if (!isAutoSelectEnabled()) return null;
  const candidates = await getProxyCandidates();
  if (candidates.length === 0) return null;
  const targetUrl = "https://api.openai.com/v1/models";
  const targetHostname = "api.openai.com";
  return findWorkingProxy(targetHostname, targetUrl);
}

/** Invalidate the fallback cache (force re-probe on next call). */
export function invalidateProxyFallbackCache() {
  PROXY_FALLBACK_CACHE.clear();
}