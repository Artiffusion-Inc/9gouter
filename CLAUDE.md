# CLAUDE.md

> **Fork of [decolua/9gouter](https://github.com/decolua/9gouter)** with custom patches. Sync upstream periodically.

## Current State: Go Rewrite (branch `main`)

The `main` branch is the **Go rewrite** — a single static binary serving the OpenAI-compatible `/v1/*` API, the dashboard `/api/*` management surface, and the embedded Next.js static-export UI. The legacy Node.js / Next.js / `open-sse` backend lives on branch `legacy/js-backend` and is kept only for rollback; do not edit it on `main`.

> **Legacy tree still tracked on `main` (pending cutover deletion):** the old JS
> directories — `open-sse/`, `src/`, `tests/`, `public/`, `gitbook/`, `cli/`,
> `i18n/`, `skills/`, `images/` — are **still committed on `main`** (≈1167
> files). They are NOT active code; the runtime is the Go binary only. Do not
> edit them — they are slated for deletion at cutover (T021). The *working*
> tree on `main` is just: `cmd/`, `internal/`, `tools/`, `docs/`, `scripts/`,
> `.github/`. `src/app/**` (dashboard UI) is the one legacy path still built
> into the binary via `scripts/build-dashboard.sh`; the rest of `src/` is
> dead legacy pending deletion.

- **Single binary**: `cmd/9gouter` → listens on `:20127`
- **Storage**: SQLite via `modernc.org/sqlite` (pure Go, CGO_ENABLED=0), default `./data/9gouter.db` (`DB_PATH`)
- **Dashboard**: Next.js `output:export` static build, embedded into the binary via `//go:embed all:dashboard_assets` and served by `internal/adapter/transport/http/static.go` with SPA fallback
- **Clean architecture**: `internal/{domain, usecase, adapter}` + composition root `internal/app/wire.go`

### Architecture

```
cmd/9gouter/                      ← entrypoint: config.Load → app.Wire → http.Server
internal/
  adapter/
    config/                      ← envconfig Config + DurationMs setter (timeouts)
    auth/                        ← session cookie store (HMAC), OIDC
    db/                          ← sqlite.Open, migrations.Run, SyncSchema, repo/*
    provider/                    ← per-provider adapters (ollama, codex, gemini, qwen, grok-web, cursor, kiro, ...)
    translator/                  ← request/response format translation (openai, claude, gemini, codex, ollama, ...)
    transport/
      http/                      ← /v1 routes, static dashboard serving, api/* handlers
        api/                      ← dashboard /api handlers (auth, keys, combos, models, settings, backup, ...)
      proxy/                     ← proxy stack (SOCKS5, fast-fail, fallback, round-robin)
    rtk/                         ← runtime kit helpers
  domain/                        ← domain types (auth, chat, format, provider, settings, usage)
  usecase/                       ← proxychat, auth, managedashboard
  app/                           ← composition root (Wire, repos, handler adapters)
```

### Key Files to Edit

| What | Where |
|------|-------|
| Config / env vars / timeouts | `internal/adapter/config/config.go` |
| Composition root / wiring | `internal/app/wire.go` |
| Entrypoint | `cmd/9gouter/main.go` |
| DB schema / migrations | `internal/adapter/db/migrations/`, `internal/adapter/db/schema.go` |
| Repositories | `internal/adapter/db/repo/*.go` |
| Provider adapters | `internal/adapter/provider/<name>/` |
| Translators | `internal/adapter/translator/<name>/` |
| /v1 chat routing | `internal/adapter/transport/http/` + `internal/usecase/proxychat/` |
| Account fallback / per-model locks | `internal/adapter/transport/http/accountfallback/` |
| Dashboard /api handlers | `internal/adapter/transport/http/api/*.go` |
| Static dashboard serving | `internal/adapter/transport/http/static.go` (`//go:embed all:dashboard_assets`) |
| Proxy stack | `internal/adapter/transport/proxy/*.go` |
| Backup import/export | `internal/adapter/transport/http/api/settings_backup.go`, `settings_extra.go` |
| Dashboard UI source | `src/app/**` (Next.js, `output:export`; rest of `src/` is legacy pending deletion) |
| Dashboard build script | `scripts/build-dashboard.sh` |

### Configuration (env vars)

Defined in `internal/adapter/config/config.go` via `envconfig`. Timeout fields use the `DurationMs` setter, which accepts either a bare integer (milliseconds, matching the JS `*_MS` env names) or a Go duration string (`"60s"`). Defaults match the legacy compose values.

```
PORT=20127
DB_PATH=./data/9gouter.db
DASHBOARD_PASSWORD_HASH=         # bcrypt hash; backup settings.password is one
DASHBOARD_SESSION_SECRET=change-me
SESSION_SECRET=change-me

# Timeouts (ms or Go duration). Defaults shown.
FETCH_CONNECT_TIMEOUT_MS=60000
STREAM_STALL_TIMEOUT_MS=180000
STREAM_STALL_TIMEOUT_REASONING_MS=600000
STREAM_READINESS_MAX_TIMEOUT_MS=900000
FETCH_HEADERS_TIMEOUT_MS=60000
FETCH_BODY_TIMEOUT_MS=600000
FETCH_KEEPALIVE_TIMEOUT_MS=4000
SOCKS_HANDSHAKE_TIMEOUT_MS=10000
PROXY_DISPATCHER_CONNECTIONS=1          # >1 = round-robin fan-out
PROXY_FAST_FAIL_TIMEOUT_MS=2000
PROXY_HEALTH_CACHE_TTL_MS=30000
PROXY_HEALTH_UNHEALTHY_CACHE_TTL_MS=2000
PROXY_FALLBACK_PROBE_TIMEOUT_MS=3000
PROXY_AUTO_SELECT_ENABLED=false
```

Reasoning/thinking models get `STREAM_STALL_TIMEOUT_REASONING_MS` instead of `STREAM_STALL_TIMEOUT_MS` (detection mirrors the JS `isThinkingEnabled()`).

### Backup / Restore (Go API)

Backup import/export is implemented in the Go API, mirroring the legacy JS `exportDb()`/`importDb()` 1:1:

- `GET /api/settings/database` → `ExportDb` (full config payload)
- `POST /api/settings/database` → `ImportDb` (wipes + inserts; session-auth protected)

Payload shape (`api.BackupPayload`): `settings, providerConnections, providerNodes, proxyPools, apiKeys, combos, modelAliases, customModels, mitmAlias, pricing` — identical to the JS dashboard "Download backup" / "Restore" buttons, so a JS-era backup JSON imports directly.

### #2703 Fix Status (route-aware proxy + account selection)

The `decolua/9gouter` #2703 fix series ports the JS account-selection + proxy
pipeline into Go. Fix 1 (strictProxy propagation) landed earlier.

| Fix | What | Status |
|-----|------|--------|
| 1 | strictProxy propagation through the proxy stack | DONE (earlier) |
| 5 | Route diagnostics — typed `FailureSource` on `proxy.FetchError`, structured "route selected" / "proxy fallback to direct" logs | DONE (`2e035f2`) |
| 4 | Sticky round-robin selection — stay-vs-rotate, `lastUsedAt`/`consecutiveUseCount` persisted, `ErrNoActiveCredentials` → 503 | DONE (`8d57c09`) |
| 3 | Structured failure types + account fallback loop — `accountfallback` package (ErrorRules, ModelLock\*, typed ProxyRouteError), `ConnectionRepo.ApplyConnectionPatch`, `v1.handleChat` while(true) loop; proxy/relay outage fails hard WITHOUT locking the account | DONE (`3354986`) |
| 2a | Route-aware `TokenRefresher.Refresh` (proxy opts → `ProxyAwareFetch`) | DONE (`67c6ba0`) |
| 2b | Refresh dedup + per-connection refresh mutex (`SharedRefreshDedup`, singleflight + 10s TTL) | DONE (`121b22e`) |
| 2c | Proactive `ShouldRefreshCredentials` + `MergeRefreshedCredentials` (per-provider policy: codex 5d lead/8d maxAge/trackRefreshAt, kimi-coding 5m, default 5m) + wire in `resolveCredentialsWithOpts` | DONE (`f9f901c`) |
| 2d | Reactive 401/403 refresh-retry in the chat path (refresh once, retry same connection, then fallback); `V1Deps.TokenRefreshers` injection | DONE (`89209b7`) |
| 2e | `getProjectIdForConnection` (antigravity/gemini-cli) — `projectid.Fetcher` (loadCodeAssist → onboardUser poll, 1h cache, inflight dedup) wired via `V1Deps.ProjectIDFetcher` | DONE (`daf6bf2`) |

### Cutover Tooling

- `tools/shadowdiff/` — reverse-proxy shadow-diff harness (fans out to the Go backend, diffs status/headers/normalized SSE/usage). Mismatches logged to `tools/shadowdiff/mismatches.jsonl`.
- `docs/cutover-runbook.md` — pre-flight, go-live, rollback, monitoring, deletion criteria.
- `Dockerfile` — multi-stage: Bun static export → Go `CGO_ENABLED=0` static build → distroless.

## Legacy JS Backend (branch `legacy/js-backend`)

The Node.js 22 / Next.js / `open-sse` backend is preserved on `legacy/js-backend` for rollback reference. The custom patches below were ported into the Go rewrite where noted; the rest remain JS-side only. When syncing upstream, conflicts land here.

### Our Patches (JS)

| Patch | File | Upstream Status |
|-------|------|-----------------|
| Env-overridable timeouts (defaults 120s) | `open-sse/config/runtimeConfig.js` | PRs #1680, #1688 closed without merge |
| Error SSE on stream stall/abort | `open-sse/utils/streamHandler.js` | Not submitted — our fix |
| Reasoning model stall timeout extension | `open-sse/handlers/chatCore.js`, `streamHandler.js` | Not submitted — our fix |
| Adaptive stream-readiness timeout (body-size/reasoning-aware) | `open-sse/utils/streamReadinessPolicy.js`, `handlers/chatCore/streamingHandler.js` | Ported from OmniRoute (#3825) |
| JSON→SSE synthesis for non-streaming upstreams | `open-sse/utils/jsonToSse.js`, `finishReason.js`, `reasoningFields.js`, `handlers/chatCore/streamingHandler.js` | Ported from OmniRoute (#3089) |
| Upstream response header strip helper (future-use guard) | `open-sse/utils/upstreamResponseHeaders.js` | Ported from OmniRoute — unmounted |
| Proxy stack: SOCKS5, timeout config, fast-fail, fallback, round-robin | `open-sse/utils/proxy*.js`, `socksConnector.js`, `fetchCause.js`, `proxyFetch.js` | Ported from OmniRoute → Go `internal/adapter/transport/proxy/` |

### Proxy Stack (JS, ported to Go)

`open-sse/utils/proxyFetch.js` orchestrated a layered proxy pipeline; the Go rewrite reproduces the same layers in `internal/adapter/transport/proxy/`:

1. **Vercel relay** — `x-relay-target`/`x-relay-path` headers.
2. **Connection proxy** — per-connection `connectionProxyUrl` / `connectionProxyEnabled`, else env `HTTP(S)_PROXY`/`ALL_PROXY` (honouring `NO_PROXY`).
3. **Dispatcher** — HTTP/HTTPS (`ProxyAgent`) or SOCKS5 (family pinning) with undici timeout config so upstream `Keep-Alive:` cannot clamp keepAlive up to 600s.
4. **Fast-fail** — TCP check (<2s) skips dispatcher creation for dead proxies; cached (healthy 30s, unhealthy 2s), inflight dedup.
5. **Fallback** — on proxy failure (non-strict), collect candidates from active proxy pools + env, test in parallel, return first working; cached per target 5 min. `strictProxy=true` fails hard.
6. **MITM DNS bypass** — for `MITM_BYPASS_HOSTS`, resolve real IP via Google DNS.
7. **Round-robin direct** — `PROXY_DISPATCHER_CONNECTIONS>1` fans out across N one-connection agents.
8. **Diagnostics** — flatten `TypeError: fetch failed` `.cause` chain into `code/syscall/errno/address:port`.

### JSON→SSE Synthesis

OpenAI-compatible "reasoning" upstreams that ignore `stream:true` and reply with a single `application/json` body are converted into an OpenAI SSE stream, preserving `content` + `reasoning_content` + `tool_calls` + `usage`. In Go this lives in `internal/adapter/translator/`; in JS it was `open-sse/utils/jsonToSse.js`.

### Stream Error SSE

On stream stall/abort, chat/completions clients receive a structured error SSE event + `[DONE]` instead of an empty HTTP 200. Ported to the Go proxychat usecase.

## Build & Run

```bash
# Go binary (dashboard assets must be present for embed — see .gitkeep fallback)
CGO_ENABLED=0 go build -o /tmp/9gouter ./cmd/9gouter
DB_PATH=./data/9gouter.db /tmp/9gouter

# Dashboard static export → internal/adapter/transport/http/dashboard_assets/
scripts/build-dashboard.sh      # bun build → cp out/ into dashboard_assets

# Tests
go test -race ./...
```

`internal/adapter/transport/http/dashboard_assets/.gitkeep` is committed so a clean clone compiles; the real export is gitignored and produced by `scripts/build-dashboard.sh` (or the Dockerfile's first stage).

## Upstream Sync

```bash
cd ~/Github/9gouter
git fetch upstream
git merge upstream/main --no-edit
# Resolve conflicts, then:
git push origin main
git tag v0.4.XX-artiffusion.N
git push --tags
```

Legacy JS conflicts land on `legacy/js-backend` in `runtimeConfig.js` (env-var patch) and `streamHandler.js` (error SSE patch).

## Docker Build

- **Trigger**: push tag `v*` or manual `workflow_dispatch`
- **Images**: `ghcr.io/artiffusion/9gouter:latest` + `artiffusion/9gouter:latest`
- **Platforms**: linux/amd64, linux/arm64
- **Secrets needed**: `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN` (GHCR uses `GITHUB_TOKEN`)
- **Compose**: `docker-compose.yml` → port 20127, volume `9gouter-data`, `.env` file

## Tech Stack

- **Backend**: Go 1.26, `net/http` + SSE, `modernc.org/sqlite` (pure Go, CGO=0)
- **Dashboard**: Next.js (`output:export`), built with `bun`, embedded via `//go:embed`
- **Auth**: session cookie (HMAC `auth_token`), bcrypt password hash, OIDC
- **Config**: `kelseyhightower/envconfig` with `DurationMs` ms-or-duration setter