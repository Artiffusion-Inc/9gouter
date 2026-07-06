// ponytail: ported from OmniRoute proxyDispatcher.ts (subset).
// Central proxy dispatcher factory: HTTP/HTTPS via undici ProxyAgent, SOCKS5
// via socksConnector with family pinning. All dispatchers get undici timeout
// config (headers/body/connect/keepAlive) so upstream Keep-Alive: header
// cannot clamp keepAliveTimeout UP to 600s and leak zombie sockets. Round-
// robin fan-out available for Node 24 SSE-serialization mitigation.
// Deps: undici (ProxyAgent, Agent) + socksConnector + proxyFamily + proxyDispatcherCache.

import { ProxyAgent, Agent } from "undici";
import { createSocksDispatcherWithFamily } from "./socksConnector.js";
import { detectIpLiteralFamily, parseProxyFamily } from "./proxyFamily.js";
import {
  createRoundRobinDispatcher,
  getDispatcherCache,
  setDefaultCachedDispatcher,
  getDefaultCachedDispatcher,
  clearDispatcherCache,
} from "./proxyDispatcherCache.js";
import {
  FETCH_HEADERS_TIMEOUT_MS,
  FETCH_BODY_TIMEOUT_MS,
  FETCH_CONNECT_TIMEOUT_MS,
  FETCH_KEEPALIVE_TIMEOUT_MS,
  PROXY_DISPATCHER_CONNECTIONS,
} from "../config/runtimeConfig.js";

export { clearDispatcherCache };

const SUPPORTED_PROTOCOLS = new Set(["http:", "https:", "socks5:", "socks5h:"]);

/** @typedef {{ type?: string, host?: string, port?: string|number|null, username?: string, password?: string, family?: string }} ProxyConfigObject */

function getDispatcherOptions() {
  return {
    headersTimeout: FETCH_HEADERS_TIMEOUT_MS,
    bodyTimeout: FETCH_BODY_TIMEOUT_MS,
    connectTimeout: FETCH_CONNECT_TIMEOUT_MS,
    keepAliveTimeout: FETCH_KEEPALIVE_TIMEOUT_MS,
    // Without this, an upstream Keep-Alive: timeout=N header clamps
    // keepAliveTimeout UP to undici's default keepAliveMaxTimeout (600s),
    // completely overriding the configured timeout and restoring zombie-socket risk.
    keepAliveMaxTimeout: FETCH_KEEPALIVE_TIMEOUT_MS,
  };
}

/** @param {string} proxyUrl @returns {{protocol: string, host: string, port: number|null, username?: string, password?: string}} */
export function normalizeProxyUrl(proxyUrl) {
  if (!proxyUrl || typeof proxyUrl !== "string") return null;
  const trimmed = proxyUrl.trim();
  if (!trimmed) return null;
  try {
    const u = new URL(trimmed);
    if (!SUPPORTED_PROTOCOLS.has(u.protocol)) return null;
    return {
      protocol: u.protocol,
      host: u.hostname,
      port: u.port ? Number.parseInt(u.port, 10) : null,
      username: decodeURIComponent(u.username || ""),
      password: decodeURIComponent(u.password || ""),
    };
  } catch {
    // Allow "127.0.0.1:7890" style
    try {
      const u = new URL(`http://${trimmed}`);
      return {
        protocol: "http:",
        host: u.hostname,
        port: u.port ? Number.parseInt(u.port, 10) : null,
        username: "",
        password: "",
      };
    } catch {
      return null;
    }
  }
}

/** @param {ProxyConfigObject} cfg @returns {string} */
export function proxyConfigToUrl(cfg) {
  const proto = (cfg.type || "http").replace(/:$/, "");
  const auth = cfg.username
    ? `${encodeURIComponent(cfg.username)}:${encodeURIComponent(cfg.password || "")}@`
    : "";
  const port = cfg.port ? `:${cfg.port}` : "";
  return `${proto}://${auth}${cfg.host || ""}${port}`;
}

/** @param {ProxyConfigObject|string} proxy @returns {string} proxy URL for logs (creds masked) */
export function proxyUrlForLogs(proxy) {
  const url = typeof proxy === "string" ? proxy : proxyConfigToUrl(proxy || {});
  try {
    const u = new URL(url);
    if (u.username || u.password) u.password = u.username ? "****" : "";
    u.username = "";
    return u.toString();
  } catch {
    return url;
  }
}

function isSocks5(protocol) {
  return protocol === "socks5:" || protocol === "socks5h:";
}

/**
 * Build a single undici dispatcher for one proxy URL (HTTP or SOCKS5).
 * @param {string} proxyUrl
 * @param {object} [extraAgentOptions]
 * @returns {import("undici").Dispatcher|null}
 */
export function createProxyDispatcher(proxyUrl, extraAgentOptions = {}) {
  const parsed = normalizeProxyUrl(proxyUrl);
  if (!parsed) return null;

  const family = parseProxyFamily(typeof extraAgentOptions.family === "string" ? extraAgentOptions.family : "auto");
  const familyPin = family === "auto" ? null : detectIpLiteralFamily(parsed.host) ?? (family === "ipv4" ? 4 : 6);
  const dispatcherOpts = { ...getDispatcherOptions(), ...extraAgentOptions };

  if (isSocks5(parsed.protocol)) {
    const socksProxy = {
      type: 5,
      host: parsed.host,
      port: parsed.port ?? 1080,
      userId: parsed.username || undefined,
      password: parsed.password || undefined,
    };
    return createSocksDispatcherWithFamily(socksProxy, familyPin, dispatcherOpts);
  }

  // HTTP/HTTPS proxy via ProxyAgent
  const auth = parsed.username
    ? `${parsed.username}:${parsed.password || ""}`
    : undefined;
  return new ProxyAgent({
    uri: proxyConfigToUrl({ type: parsed.protocol.replace(":", ""), host: parsed.host, port: parsed.port, username: parsed.username, password: parsed.password }),
    ...dispatcherOpts,
  });
}

/**
 * Build a direct (no-proxy) dispatcher. With PROXY_DISPATCHER_CONNECTIONS > 1
 * returns a RoundRobinDispatcher of N one-connection Agents to mitigate Node
 * 24 undici same-origin SSE serialization; otherwise a single Agent.
 * @returns {import("undici").Dispatcher}
 */
export function createDirectDispatcher() {
  const opts = getDispatcherOptions();
  if (PROXY_DISPATCHER_CONNECTIONS > 1) {
    const agents = Array.from({ length: PROXY_DISPATCHER_CONNECTIONS }, () => new Agent({ ...opts, connections: 1, pipelining: 0 }));
    return createRoundRobinDispatcher(agents);
  }
  return new Agent(opts);
}

/**
 * Get-or-create a cached dispatcher keyed by normalized proxy URL.
 * @param {string} proxyUrl
 * @returns {import("undici").Dispatcher|null}
 */
export function getCachedProxyDispatcher(proxyUrl) {
  const parsed = normalizeProxyUrl(proxyUrl);
  if (!parsed) return null;
  const cache = getDispatcherCache();
  const key = proxyUrlForLogs(parsed);
  if (cache.has(key)) return cache.get(key);
  const dispatcher = createProxyDispatcher(proxyUrl);
  if (dispatcher) cache.set(key, dispatcher);
  return dispatcher;
}

export function getDefaultDispatcher() {
  return getDefaultCachedDispatcher();
}

export function setDefaultDispatcher(dispatcher) {
  setDefaultCachedDispatcher(dispatcher);
}