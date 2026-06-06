# CLAUDE.md

> **Fork of [decolua/9router](https://github.com/decolua/9router)** with custom patches. Sync upstream periodically.

## Our Patches

| Patch | File | Upstream Status |
|-------|------|-----------------|
| Env-overridable timeouts (defaults 120s) | `open-sse/config/runtimeConfig.js` | PRs #1680, #1688 closed without merge |
| Error SSE on stream stall/abort | `open-sse/utils/streamHandler.js` | Not submitted — our fix |

### Timeout Configuration (env vars)

```
FETCH_CONNECT_TIMEOUT_MS=120000   # default 120s (upstream: 60s)
STREAM_STALL_TIMEOUT_MS=180000   # default 120s (upstream: 60s), set 180s in compose
```

Per-provider `config.timeoutMs` override also available via dashboard.

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