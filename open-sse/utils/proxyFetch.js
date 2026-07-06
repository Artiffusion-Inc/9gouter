import { Readable } from "stream";
import { MEMORY_CONFIG } from "../config/runtimeConfig.js";
import { dbg } from "./debugLog.js";
import { describeFetchCause } from "./fetchCause.js";
import { createProxyDispatcher, createDirectDispatcher, normalizeProxyUrl as dispatcherNormalize } from "./proxyDispatcher.js";
import { isProxyReachable, invalidateProxyHealth } from "./proxyHealth.js";
import { findWorkingProxy } from "./proxyFallback.js";

const originalFetch = globalThis.fetch;
// ponytail: per-URL dispatcher cache moved to proxyDispatcherCache.js (shared
// on globalThis, survives HMR). Keep a thin local proxyDispatchers map only
// as a legacy fallback for callers that import getDispatcher directly.
const proxyDispatchers = new Map();

// DNS cache — use Map to avoid prototype pollution via malformed hostnames
const DNS_CACHE = new Map();
const MITM_BYPASS_HOSTS = [
  "cloudcode-pa.googleapis.com",
  "daily-cloudcode-pa.googleapis.com",
  "api.individual.githubcopilot.com",
  "q.us-east-1.amazonaws.com",
  "codewhisperer.us-east-1.amazonaws.com",
  "api2.cursor.sh",
];
const GOOGLE_DNS_SERVERS = ["8.8.8.8", "8.8.4.4"];
const HTTPS_PORT = 443;
const HTTP_SUCCESS_MIN = 200;
const HTTP_SUCCESS_MAX = 300;

function normalizeString(value) {
  if (value === undefined || value === null) return "";
  return String(value).trim();
}

/**
 * Resolve real IP using Google DNS (bypass system DNS)
 */
async function resolveRealIP(hostname) {
  const cached = DNS_CACHE.get(hostname);
  if (cached && Date.now() < cached.expiry) return cached.ip;

  try {
    const dns = await import("dns");
    const { promisify } = await import("util");
    const resolver = new dns.Resolver();
    resolver.setServers(GOOGLE_DNS_SERVERS);
    const resolve4 = promisify(resolver.resolve4.bind(resolver));
    const addresses = await resolve4(hostname);
    DNS_CACHE.set(hostname, { ip: addresses[0], expiry: Date.now() + MEMORY_CONFIG.dnsCacheTtlMs });
    return addresses[0];
  } catch (error) {
    console.warn(`[ProxyFetch] DNS resolve failed for ${hostname}:`, error.message);
    return null;
  }
}

/**
 * Check if request should bypass MITM DNS redirect
 */
function shouldBypassMitmDns(url) {
  try {
    const hostname = new URL(url).hostname;
    return MITM_BYPASS_HOSTS.some(host => hostname.includes(host));
  } catch { return false; }
}

function shouldBypassByNoProxy(targetUrl, noProxyValue) {
  const noProxy = normalizeString(noProxyValue);
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

/**
 * Get proxy URL from environment
 */
function getEnvProxyUrl(targetUrl) {
  const noProxy = process.env.NO_PROXY || process.env.no_proxy;
  if (shouldBypassByNoProxy(targetUrl, noProxy)) return null;

  let protocol;
  try { protocol = new URL(targetUrl).protocol; } catch { return null; }

  if (protocol === "https:") {
    return process.env.HTTPS_PROXY || process.env.https_proxy ||
      process.env.ALL_PROXY || process.env.all_proxy;
  }

  return process.env.HTTP_PROXY || process.env.http_proxy ||
    process.env.ALL_PROXY || process.env.all_proxy;
}

/**
 * Normalize proxy URL (allow host:port). Delegates to proxyDispatcher for
 * protocol/auth parsing; returns the original string on valid parse.
 */
function normalizeProxyUrl(proxyUrl) {
  const parsed = dispatcherNormalize(proxyUrl);
  if (!parsed) return null;
  const auth = parsed.username ? `${parsed.username}:${parsed.password || ""}@` : "";
  const port = parsed.port ? `:${parsed.port}` : "";
  return `${parsed.protocol}//${auth}${parsed.host}${port}`;
}

function resolveConnectionProxyUrl(targetUrl, proxyOptions) {
  const enabled = proxyOptions?.enabled === true || proxyOptions?.connectionProxyEnabled === true;
  if (!enabled) return null;

  const proxyUrlRaw = normalizeString(proxyOptions?.url ?? proxyOptions?.connectionProxyUrl);
  if (!proxyUrlRaw) return null;

  const noProxy = normalizeString(proxyOptions?.noProxy ?? proxyOptions?.connectionNoProxy);
  if (noProxy && shouldBypassByNoProxy(targetUrl, noProxy)) return null;

  return normalizeProxyUrl(proxyUrlRaw);
}

/**
 * Create a proxy dispatcher lazily (undici-compatible). HTTP/HTTPS via
 * ProxyAgent with undici timeout config; SOCKS5 via socksConnector with family
 * pinning. Cached per normalized proxy URL on globalThis (proxyDispatcherCache).
 */
async function getDispatcher(proxyUrl) {
  const normalized = normalizeProxyUrl(proxyUrl);
  if (!normalized) return null;
  return createProxyDispatcher(normalized);
}

/**
 * Create HTTPS request with manual socket connection (bypass DNS)
 */
async function createBypassRequest(parsedUrl, realIP, options) {
  const httpsModule = await import("https");
  const netModule = await import("net");
  // CJS modules expose exports via .default in ESM dynamic import context
  const https = httpsModule.default ?? httpsModule;
  const net = netModule.default ?? netModule;

  return new Promise((resolve, reject) => {
    const socket = new net.Socket();

    socket.connect(HTTPS_PORT, realIP, () => {
      const reqOptions = {
        socket,
        // SNI + cert hostname are validated against the hostname the caller
        // asked for, not the IP we connected to. This keeps the DNS-bypass
        // (avoiding /etc/hosts MITM) while still rejecting on-path attackers
        // that present a different cert. The MITM_BYPASS_HOSTS targets are
        // all public-CA-issued (Google / GitHub / AWS / Cursor) so default
        // verification works without any extra trust store.
        servername: parsedUrl.hostname,
        path: parsedUrl.pathname + parsedUrl.search,
        method: options.method || "POST",
        headers: {
          ...options.headers,
          Host: parsedUrl.hostname,
        },
      };

      const req = https.request(reqOptions, (res) => {
        const response = {
          ok: res.statusCode >= HTTP_SUCCESS_MIN && res.statusCode < HTTP_SUCCESS_MAX,
          status: res.statusCode,
          statusText: res.statusMessage,
          headers: new Map(Object.entries(res.headers)),
          body: Readable.toWeb(res),
          text: async () => {
            const chunks = [];
            for await (const chunk of res) chunks.push(chunk);
            return Buffer.concat(chunks).toString();
          },
          json: async () => JSON.parse(await response.text()),
        };
        resolve(response);
      });

      req.on("error", reject);
      if (options.body) {
        req.write(typeof options.body === "string" ? options.body : JSON.stringify(options.body));
      }
      req.end();
    });

    socket.on("error", reject);
  });
}

/**
 * Fetch through a proxy URL. Fast-fails on dead proxies (TCP unreachable, <2s)
 * via proxyHealth cache. On proxy failure, falls back to proxyFallback
 * candidate probing unless strictProxy is set.
 */
async function fetchWithProxy(url, options, proxyUrl, proxyOptions) {
  const strict = proxyOptions?.strictProxy === true;

  // Fast-fail: skip dispatcher creation entirely if the proxy TCP port is dead.
  if (!(await isProxyReachable(proxyUrl))) {
    if (strict) throw new Error(`[ProxyFetch] Proxy unreachable (strictProxy=true): ${proxyUrl}`);
    console.warn(`[ProxyFetch] Proxy unreachable, trying fallback: ${proxyUrl}`);
    const fallback = await fetchWithFallback(url, options, proxyOptions);
    return fallback || (await directFetch(url, options));
  }

  try {
    const dispatcher = await getDispatcher(proxyUrl);
    return await originalFetch(url, { ...options, dispatcher });
  } catch (proxyError) {
    // Invalidate health cache — the cached "healthy" entry is now stale.
    invalidateProxyHealth(proxyUrl);
    const cause = describeFetchCause(proxyError);
    if (strict) {
      throw new Error(`[ProxyFetch] Proxy required but failed (strictProxy=true): ${cause}`);
    }
    console.warn(`[ProxyFetch] Proxy failed, trying fallback: ${cause}`);
    const fallback = await fetchWithFallback(url, options, proxyOptions);
    return fallback || (await directFetch(url, options));
  }
}

/**
 * Try the proxyFallback candidate pool. Returns null when no working proxy is
 * found (caller then decides direct vs strict-fail).
 */
async function fetchWithFallback(url, options, proxyOptions) {
  let targetHostname = "";
  let targetUrl = "";
  try {
    const u = new URL(typeof url === "string" ? url : url.toString());
    targetHostname = u.hostname;
    targetUrl = u.toString();
  } catch { /* non-URL input — skip fallback */ }

  if (targetUrl) {
    try {
      const working = await findWorkingProxy(targetHostname, targetUrl);
      if (working) {
        dbg("PROXY", `fallback selected working proxy for ${targetHostname}`);
        const dispatcher = await getDispatcher(working);
        return originalFetch(url, { ...options, dispatcher });
      }
    } catch (e) {
      dbg("PROXY", `fallback failed: ${describeFetchCause(e)}`);
    }
  }
  return null;
}

/**
 * Direct native fetch. With PROXY_DISPATCHER_CONNECTIONS>1 uses a round-robin
 * direct dispatcher; otherwise plain originalFetch.
 */
async function directFetch(url, options) {
  return originalFetch(url, options);
}

export async function proxyAwareFetch(url, options = {}, proxyOptions = null) {
  const targetUrl = typeof url === "string" ? url : url.toString();

  // Vercel relay: forward request via relay headers
  const vercelRelayUrl = normalizeString(proxyOptions?.vercelRelayUrl);
  if (vercelRelayUrl) {
    const parsed = new URL(targetUrl);
    const relayHeaders = {
      ...options.headers,
      "x-relay-target": `${parsed.protocol}//${parsed.host}`,
      "x-relay-path": `${parsed.pathname}${parsed.search}`,
    };
    return originalFetch(vercelRelayUrl, { ...options, headers: relayHeaders });
  }

  const connectionProxyUrl = resolveConnectionProxyUrl(targetUrl, proxyOptions);
  const envProxyUrl = connectionProxyUrl ? null : normalizeProxyUrl(getEnvProxyUrl(targetUrl));
  const proxyUrl = connectionProxyUrl || envProxyUrl;

  // MITM DNS bypass: for known MITM-intercepted hosts, resolve real IP to avoid DNS spoof
  if (shouldBypassMitmDns(targetUrl)) {
    if (proxyUrl) {
      // Proxy resolves DNS externally (not affected by /etc/hosts) — use proxy directly
      try {
        return await fetchWithProxy(url, options, proxyUrl, proxyOptions);
      } catch (proxyError) {
        if (proxyOptions?.strictProxy === true) throw proxyError;
        console.warn(`[ProxyFetch] MITM proxy failed, falling back to direct bypass: ${describeFetchCause(proxyError)}`);
      }
    }
    // No proxy / proxy failed non-strict — fall through to MITM DNS bypass below.
    // No proxy — manually resolve real IP to bypass DNS spoof
    try {
      const parsedUrl = new URL(targetUrl);
      const realIP = await resolveRealIP(parsedUrl.hostname);
      if (realIP) return await createBypassRequest(parsedUrl, realIP, options);
    } catch (error) {
      console.warn(`[ProxyFetch] MITM bypass failed: ${describeFetchCause(error)}`);
    }
  }

  if (proxyUrl) {
    const res = await fetchWithProxy(url, options, proxyUrl, proxyOptions);
    // fetchWithProxy only returns null when the dead-proxy path exhausted
    // fallback without strictProxy — degrade to direct.
    if (res) return res;
  }

  // got-scraping disabled — use native fetch directly.
  return directFetch(url, options);
}

/**
 * Patched global fetch with env-proxy support and MITM DNS bypass
 */
async function patchedFetch(url, options = {}) {
  return proxyAwareFetch(url, options, null);
}

// Idempotency guard — only patch once to avoid wrapping multiple times
if (globalThis.fetch !== patchedFetch) {
  globalThis.fetch = patchedFetch;
}

export default patchedFetch;
export { getDispatcher };