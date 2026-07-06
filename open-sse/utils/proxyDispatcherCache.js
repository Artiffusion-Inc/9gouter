// ponytail: ported from OmniRoute proxyDispatcherCache.ts.
// Direct upstream fan-out: a single undici Agent with connections>1 should
// suffice, but real Codex streams on Node 24 have been observed queuing every
// subsequent same-origin request until the previous stream emits trailers.
// Several one-connection Agents give each long SSE stream an independent pool
// and prevent one stream from monopolizing the queue (pipelining disabled).
// Cache lives on globalThis so it survives across HMR/reload and is shared.

const DISPATCHER_CACHE_KEY = Symbol.for("9router.proxyDispatcher.cache");
const DEFAULT_DISPATCHER_KEY = Symbol.for("9router.proxyDispatcher.default");
const RETRY_DISPATCHER_KEY = Symbol.for("9router.proxyDispatcher.retry");

class RoundRobinDispatcher {
  constructor(dispatchers) {
    this.dispatchers = dispatchers;
    this.nextIndex = 0;
  }

  dispatch(options, handler) {
    const dispatcher = this.dispatchers[this.nextIndex % this.dispatchers.length];
    this.nextIndex = (this.nextIndex + 1) % this.dispatchers.length;
    return dispatcher.dispatch(options, handler);
  }

  close(callback) {
    const done = Promise.all(this.dispatchers.map((d) => d.close())).then(() => undefined);
    if (callback) { done.then(callback); return; }
    return done;
  }

  destroy(errorOrCallback, callback) {
    const callbackFn = typeof errorOrCallback === "function" ? errorOrCallback : callback;
    const error = typeof errorOrCallback === "function" ? null : (errorOrCallback ?? null);
    const done = Promise.all(this.dispatchers.map((d) => d.destroy(error))).then(() => undefined);
    if (callbackFn) { done.then(callbackFn); return; }
    return done;
  }
}

export function createRoundRobinDispatcher(dispatchers) {
  return new RoundRobinDispatcher(dispatchers);
}

export function getDispatcherCache() {
  const g = globalThis;
  if (!g[DISPATCHER_CACHE_KEY]) g[DISPATCHER_CACHE_KEY] = new Map();
  return g[DISPATCHER_CACHE_KEY];
}

export function getDefaultCachedDispatcher() {
  return globalThis[DEFAULT_DISPATCHER_KEY];
}

export function setDefaultCachedDispatcher(dispatcher) {
  globalThis[DEFAULT_DISPATCHER_KEY] = dispatcher;
}

export function getRetryCachedDispatcher() {
  return globalThis[RETRY_DISPATCHER_KEY];
}

export function setRetryCachedDispatcher(dispatcher) {
  globalThis[RETRY_DISPATCHER_KEY] = dispatcher;
}

function closeDispatcher(dispatcher) {
  if (!dispatcher) return;
  try {
    const result = dispatcher.close();
    if (result && typeof result.catch === "function") void result.catch(() => {});
  } catch { /* best-effort */ }
}

/** Clear all cached proxy dispatchers. Call when proxy config changes. */
export function clearDispatcherCache() {
  const cache = getDispatcherCache();
  for (const dispatcher of cache.values()) closeDispatcher(dispatcher);
  cache.clear();
  closeDispatcher(globalThis[DEFAULT_DISPATCHER_KEY]);
  closeDispatcher(globalThis[RETRY_DISPATCHER_KEY]);
  delete globalThis[DEFAULT_DISPATCHER_KEY];
  delete globalThis[RETRY_DISPATCHER_KEY];
}