# CLAUDE.md

> **Fork of [decolua/9router](https://github.com/decolua/9router)** with custom patches. Sync upstream periodically.

## Our Patches

| Patch | File | Upstream Status |
|-------|------|-----------------|
| Env-overridable timeouts (defaults 120s) | `open-sse/config/runtimeConfig.js` | PRs #1680, #1688 closed without merge |
| Error SSE on stream stall/abort | `open-sse/utils/streamHandler.js` | Not submitted — our fix |
| Reasoning model stall timeout extension | `open-sse/handlers/chatCore.js`, `streamHandler.js` | Not submitted — our fix |
| Adaptive stream-readiness timeout (body-size/reasoning-aware) | `open-sse/utils/streamReadinessPolicy.js`, `handlers/chatCore/streamingHandler.js` | Ported from OmniRoute (#3825) |
| JSON→SSE synthesis for non-streaming upstreams | `open-sse/utils/jsonToSse.js`, `finishReason.js`, `reasoningFields.js`, `handlers/chatCore/streamingHandler.js` | Ported from OmniRoute (#3089) |
| Upstream response header strip helper (future-use guard) | `open-sse/utils/upstreamResponseHeaders.js` | Ported from OmniRoute — unmounted |
| Proxy stack: SOCKS5, timeout config, fast-fail, fallback, round-robin | `open-sse/utils/proxy*.js`, `socksConnector.js`, `fetchCause.js`, `proxyFetch.js` | Ported from OmniRoute |

### Timeout Configuration (env vars)

```
FETCH_CONNECT_TIMEOUT_MS=120000                     # default 120s (upstream: 60s)
STREAM_STALL_TIMEOUT_MS=180000                     # default 120s (upstream: 60s), set 180s in compose
STREAM_STALL_TIMEOUT_REASONING_MS=600000            # default 300s (5min), set 600s (10min) in compose
STREAM_READINESS_MAX_TIMEOUT_MS=900000              # default 900s (15min), cap for adaptive readiness bump
FETCH_HEADERS_TIMEOUT_MS=60000                      # default 60s, undici headersTimeout (proxy + direct)
FETCH_BODY_TIMEOUT_MS=600000                         # default 600s, undici bodyTimeout
FETCH_KEEPALIVE_TIMEOUT_MS=4000                      # default 4s, clamps upstream Keep-Alive: header (zombie-socket fix)
SOCKS_HANDSHAKE_TIMEOUT_MS=10000                     # default 10s, ceiling 120s, SOCKS5 connect handshake
PROXY_DISPATCHER_CONNECTIONS=1                       # 0/1 = single Agent; >1 = round-robin fan-out (Node 24 SSE-serialization mitigation), ceiling 256
PROXY_FAST_FAIL_TIMEOUT_MS=2000                      # TCP reachability check timeout for dead-proxy fast-fail
PROXY_HEALTH_CACHE_TTL_MS=30000                      # cache TTL for healthy proxy probe results
PROXY_HEALTH_UNHEALTHY_CACHE_TTL_MS=2000            # cache TTL for unhealthy (dead) probe results (shorter)
PROXY_FALLBACK_PROBE_TIMEOUT_MS=3000                  # HEAD-probe timeout for proxyFallback candidate testing
PROXY_AUTO_SELECT_ENABLED=false                      # opt-in: auto-select a working proxy from the pool as global fallback
```

Reasoning/thinking models (detected via `isThinkingEnabled()`) get `STREAM_STALL_TIMEOUT_REASONING_MS` instead of `STREAM_STALL_TIMEOUT_MS`. Detection: `Anthropic-Beta` header, `thinking.type=enabled`, `reasoning_effort`, model name contains `thinking` or `-reason`.

Adaptive readiness bump (`resolveStreamReadinessTimeout`): adds +20s/+45s for large/very-large history, +15s for tool-heavy (≥15 tools), +20s/+45s for large/very-large payload, +30s for Codex GPT-5.x high-reasoning cold-start. Bumps stack on the reasoning base and are clamped to `STREAM_READINESS_MAX_TIMEOUT_MS`. Only increases, never decreases.

Per-provider `config.timeoutMs` override also available via dashboard.

### Proxy Stack (ported from OmniRoute)

`open-sse/utils/proxyFetch.js` now orchestrates a layered proxy pipeline:

1. **Vercel relay** — `proxyOptions.vercelRelayUrl` forwards via `x-relay-target`/`x-relay-path` headers (unchanged).
2. **Connection proxy** — `proxyOptions.connectionProxyUrl` / `connectionProxyEnabled` (per-connection, dashboard), else env proxy (`HTTP(S)_PROXY`/`ALL_PROXY`, honouring `NO_PROXY`).
3. **Dispatcher** — `proxyDispatcher.js` builds HTTP/HTTPS (`ProxyAgent`) or SOCKS5 (`socksConnector.js` with family pinning) dispatchers, all with undici timeout config (`headersTimeout`/`bodyTimeout`/`connectTimeout`/`keepAliveTimeout`/`keepAliveMaxTimeout`) so upstream `Keep-Alive:` header cannot clamp keepAlive UP to 600s and leak zombie sockets.
4. **Fast-fail** — `proxyHealth.js` TCP check (<2s) skips dispatcher creation for dead proxies; result cached (healthy 30s, unhealthy 2s), inflight dedup.
5. **Fallback** — on proxy failure (non-strict), `proxyFallback.js` collects candidates from active proxy pools (`src/lib/db/repos/proxyPoolsRepo.js`) + env, tests in parallel (fast-fail then HEAD probe), returns first working; cached per target URL 5 min. `strictProxy=true` fails hard instead.
6. **MITM DNS bypass** — for `MITM_BYPASS_HOSTS`, resolves real IP via Google DNS (bypass `/etc/hosts` spoof); proxy path preferred (resolves DNS externally).
7. **Round-robin direct** — `PROXY_DISPATCHER_CONNECTIONS>1` fans out across N one-connection Agents (mitigates Node 24 undici same-origin SSE serialization where one stream blocks the next).
8. **Diagnostics** — `fetchCause.js` flattens undici `TypeError: fetch failed` `.cause` chain into `code/syscall/errno/address:port` for proxy-failure logs.

New files: `fetchCause.js`, `proxyFamily.js`, `proxyHealth.js`, `proxyDispatcherCache.js`, `socksConnector.js`, `proxyDispatcher.js`, `proxyFallback.js`. All self-contained (deps: `undici`, `socks`, `node:net`/`node:dns` — already installed).

### JSON→SSE Synthesis

Some OpenAI-compatible "reasoning" upstreams ignore `stream:true` and reply with a single `application/json` chat-completion body. `synthesizeOpenAiSseFromJson()` (in `open-sse/utils/jsonToSse.js`) converts that body into an OpenAI SSE stream, preserving `content` + `reasoning_content` + `tool_calls` + `usage`. Applied in `streamingHandler.js` only when `targetFormat` is `openai`/`openai-responses`. Returns "" for non-parseable bodies → callers fall back to existing non-SSE handling.

### Stream Error SSE

When a stream stalls or aborts, regular chat/completions clients now receive a structured error SSE event + `[DONE]` instead of an empty HTTP 200. Previously only Responses API got terminal events.

## Architecture

```
Next.js app (dashboard + API)
  └── open-sse/              ← Core LLM proxy logic
      ├── config/             ← runtimeConfig.js (timeouts, retry, error config)
      ├── executors/          ← Provider-specific executors (base, groq, gemini, codex, etc.)
      ├── handlers/           ← Request routing (chat, embeddings, search, TTS, image)
      ├── translator/         ← Request/response format translation (OpenAI ↔ provider formats)
      └── utils/              ← streamHandler.js (stall detection, disconnect awareness)
  └── src/                    ← Next.js app (dashboard UI, API routes, auth)
  └── cli/                    ← CLI tools
```

## Key Files to Edit

| What | Where |
|------|-------|
| Timeouts | `open-sse/config/runtimeConfig.js` |
| Stream stall/abort handling | `open-sse/utils/streamHandler.js` |
| Connect timeout (fetch) | `open-sse/executors/base.js` (line ~126) |
| Provider-specific logic | `open-sse/executors/*.js` |
| Chat streaming handler | `open-sse/handlers/chatCore/streamingHandler.js` |
| Error formatting | `open-sse/utils/error.js` |

## Upstream Sync

```bash
cd ~/Github/9router
git fetch upstream
git merge upstream/main --no-edit
# Resolve conflicts, then:
git push origin main
git tag v0.4.XX-artiffusion.N
git push --tags
```

Conflicts likely in `runtimeConfig.js` (our env-var patch) and `streamHandler.js` (our error SSE patch).

## Docker Build

- **Trigger**: push tag `v*` or manual `workflow_dispatch`
- **Images**: `ghcr.io/artiffusion/9router:latest` + `artiffusion/9router:latest`
- **Platforms**: linux/amd64, linux/arm64
- **Secrets needed**: `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN` (GHCR uses `GITHUB_TOKEN`)

## Tech Stack

- **Runtime**: Node.js 22 (Alpine)
- **Framework**: Next.js (standalone build)
- **Build**: `npm run build` → `.next/standalone`
- **MITM**: Optional proxy for CLI tools (separate process)