# CLAUDE.md

This file provides guidance for AI agents working with the 9gouter codebase.

## What this is

9Gouter — a local AI routing gateway written in Go with an embedded Next.js
dashboard (static export). It exposes one OpenAI-compatible endpoint (`/v1/*`)
and routes traffic across 70+ upstream providers with format translation,
model-combo fallback, multi-account fallback, OAuth/API-key credential
management, token refresh, quota/usage tracking, tunnel support, and MITM
proxy interception.

The Go binary (`cmd/9gouter/main.go`) is the sole runtime: it serves both the
API (`/v1/*`, `/api/*`) and the embedded dashboard (Next.js static export via
`//go:embed`). The Next.js dashboard is build-time only — `bun run build`
produces a static `out/` that Go embeds; no JS runtime survives into the
binary or container.

## Architecture

```
cmd/9gouter/          → main.go (entry point)
internal/
  adapter/            → provider executors, translators, auth, tunnel, mitm, oauth, pxpipe, db
  app/wire.go         → composition root
  domain/             → typed interfaces (provider, usage, auth, format, settings, chat)
  usecase/            → proxychat, proxyembeddings, searchproxy, ttsproxy, sttproxy, imageproxy, videoproxy, proxyfetch, quotafetch, auth, managedashboard
```

The code lives in `internal/` (Go backend), `src/` (Next.js dashboard UI),
`open-sse/` (legacy JS provider registry data — config, capabilities, pricing,
models; consumed at build time by the static export), and `scripts/` (dashboard
build script).

## Commands

Dashboard static export (run from repo root):
```bash
cp .env.example .env
bun install
bun run build           # produces out/ static export
```

Go binary:
```bash
CGO_ENABLED=0 go build -o 9gouter ./cmd/9gouter
DB_PATH=./data/9gouter.db DASHBOARD_SESSION_SECRET=secret PORT=20127 ./9gouter
```

Tests:
```bash
go test -race ./internal/...
```

Lint:
```bash
golangci-lint run ./internal/... ./cmd/...
```

Dashboard rebuild (via go-task):
```bash
task dashboard     # rebuild Next.js static export into embedded dir
task build         # build Go binary with embedded dashboard
task run           # build + run
```

## Key conventions

- Plain JavaScript (ESM) for the dashboard; Go for all server logic.
- `@/*` path alias → `src/*` (`jsconfig.json`).
- Provider registry: `internal/adapter/provider/registry.go` — one map, auto-generated
  aliases, DefaultExecutor for OpenAI-compatible providers, specialized executors for
  non-standard ones (kiro, cursor, antigravity, codex, grok-cli, etc).
- Translators: `internal/adapter/translator/` — pivots through OpenAI as intermediate
  format. Self-register via `register(from, to, reqFn, resFn)`.
- Token refresh: `internal/adapter/provider/resolver/tokenrefresh/` — per-provider
  refreshers including Vertex RS256 JWT (go-jose).
- MITM proxy: `internal/adapter/mitm/` — Root CA generation, SNI cert signing,
  TLS intercept, DNS redirect, cert installation.
- Tunnel: `internal/adapter/tunnel/` — Cloudflare quick-tunnel + Tailscale funnel.
- OAuth: `internal/adapter/oauth/` — authorize, exchange, device-code, poll for
  PKCE and device-code providers.
- PxPipe: `internal/adapter/pxpipe/` — Node subprocess bridge for pxpipe-proxy.
- Clean architecture: domain → usecase → adapter; depguard-enforced.

## Environment

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `20127` | Go server port |
| `DB_PATH` | `./data/9gouter.db` | SQLite database location |
| `DATA_DIR` | `~/.9gouter/` | Data directory (overrides DB_PATH) |
| `JWT_SECRET` | required | Session cookie signing |
| `INITIAL_PASSWORD` | `change-me` | Default dashboard password |
| `API_KEY_SECRET` | required | API key generation salt |
| `SESSION_SECRET` | required | Dashboard session store |
| `DASHBOARD_SESSION_SECRET` | required | Dashboard cookie signing (≥16 bytes) |

## Container

```bash
podman pull ghcr.io/artiffusion-inc/9gouter:latest
podman run -d -p 20127:20127 -v 9gouter-data:/app/data ghcr.io/artiffusion-inc/9gouter:latest
```

CI: both workflows trigger on `v*` tag push → build multi-arch (amd64+arm64)
distroless container → push to GHCR only (no Docker Hub).
