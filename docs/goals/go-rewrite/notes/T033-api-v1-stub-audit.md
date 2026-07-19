# T033 — `/api/v1/*` stub audit

## Background

The Go rewrite ships a dashboard-side `/api/v1/*` surface in
`internal/adapter/transport/http/api/v1_dashboard.go`. Every route except
`GET /api/v1` (the version ping) is a **stub** that returns the constant body
`{"success":false,"message":"Dashboard /api/v1 proxy passthrough not available in Go build; use /v1 directly"}`.

The real client-facing API lives under `/v1/*` (registered by
`httptransport.RegisterV1`). The `/api/v1/*` surface existed in the legacy JS
build as a dashboard proxy that re-dispatched the same handlers (so the static
frontend could call `/api/v1/chat/completions` and hit the same code as
`/v1/chat/completions`).

## Truth table — current Go state

Routes are grouped by what the Go `/v1/*` surface already implements.

### Already implemented in `/v1/*` (stub is a pure passthrough gap)

| `/api/v1/*` stub route | `/v1/*` implementation | Status |
|-----------------------|------------------------|--------|
| `POST /api/v1/chat/completions` | `POST /v1/chat/completions` | ✅ implemented — stub should passthrough |
| `POST /api/v1/messages` | `POST /v1/messages` | ✅ implemented — stub should passthrough |
| `POST /api/v1/messages/count_tokens` | `POST /v1/messages/count_tokens` | ✅ implemented — stub should passthrough |
| `POST /api/v1/responses` | `POST /v1/responses` | ✅ implemented — stub should passthrough |
| `GET /api/v1/models` | `GET /v1/models` | ✅ implemented — stub should passthrough |
| `GET /api/v1/models/{kind}` | `GET /v1/models/{kind}` | ✅ implemented — stub should passthrough |

These 6 can be converted from stubs to internal reverse-proxy passthrough
to `/v1/*` (or directly invoke the same handlers) with no new logic. This is
the cheap win in this audit.

### Not implemented anywhere in Go (stub AND `/v1/*` both absent)

| `/api/v1/*` stub route | Legacy JS source | Go `/v1/*` | Notes |
|------------------------|------------------|------------|-------|
| `POST /api/v1/api/chat` | `src/app/api/v1/api/chat/route.js` | ❌ absent | internal-only chat variant |
| `GET /api/v1/models/info` | `src/app/api/v1/models/info/route.js` | ✅ `/v1/models/info` (T033b) | per-model capability metadata (static catalog subset) |
| `POST /api/v1/audio/speech` | TTS handlers | ❌ absent | Gemini/OpenAI TTS pipeline |
| `POST /api/v1/audio/transcriptions` | STT handlers | ❌ absent | Gemini/OpenAI STT pipeline |
| `GET /api/v1/audio/voices` | TTS voices list | ❌ absent | static voice catalog |
| `POST /api/v1/embeddings` | `src/app/api/v1/embeddings/route.js` → `embeddingsCore.js` | ❌ absent | tracked as T031b (chat-class pipeline) |
| `POST /api/v1/images/generations` | image gen handlers | ❌ absent | Gemini/OpenAI image pipeline |
| `POST /api/v1/search` | web-search handlers | ❌ absent | Gemini searchViaChat |
| `POST /api/v1/videos/generations` | video gen handlers | ❌ absent | Veo/video pipeline |
| `POST /api/v1/videos/edits` | video edit handlers | ❌ absent | video pipeline |
| `POST /api/v1/videos/extensions` | video extension handlers | ❌ absent | video pipeline |
| `GET /api/v1/videos/{id}` | video status/poll | ❌ absent | video pipeline |
| `POST /api/v1/web/fetch` | web-fetch handlers | ✅ passthrough | webFetch service kind (T033b-2 ported) |
| `POST /api/v1/responses/compact` | responses compact variant | ❌ absent | responses sub-variant |

These 14 require **new** `/v1/*` implementations first (each is a distinct
media/modality pipeline: TTS, STT, image, video, web-fetch, embeddings,
search, models/info, responses/compact). The `/api/v1/*` stub then becomes a
passthrough on top, same as the first group.

## Replacement plan

### Tier 1 — passthrough (no new logic): 6 routes

Convert the 6 stubs in the "already implemented" table to internal dispatch.
Two implementation options:

1. **Reverse-proxy to `/v1/*`** — rewrite the request path and re-dispatch
   through the same `*http.ServeMux`. Cheapest, keeps a single code path.
2. **Direct handler call** — register the same `v1Handler` methods on the
   `/api/v1/*` paths. Avoids an extra mux hop but duplicates route wiring.

Recommended: option 1 (reverse-proxy) — single source of truth, the
`/api/v1/*` surface becomes a thin alias.

### Tier 2 — new modality pipelines: 14 routes

Each is a distinct chat-class pipeline requiring its own usecase +
provider adapter + translator. Ordered roughly by dependency/leverage:

1. **`/v1/embeddings`** (T031b, already tracked) — needed by RAG users; has
   the cleanest spec (OpenAI + Gemini + openaiCompatNode adapters in JS).
2. **`/v1/models/info`** — capability metadata; depends on a capabilities
   subsystem (also unported — see porting-completeness audit T025). Could
   be a static-ish first pass off the catalog.
3. **`/v1/search`** — Gemini `searchViaChat`; reuses the chat pipeline
   with a search-system-instruction wrapper.
4. **`/v1/web/fetch`** — webFetch service kind; independent fetch+extract.
   **PORTED (T033b-2):** `internal/adapter/provider/webfetch` (firecrawl/jina-reader/tavily/exa adapters),
   `internal/usecase/proxyfetch` (Handler, buildResponseJSON mirroring JS buildData, no usage persistence),
   `internal/adapter/transport/http/v1webfetch.go` (handler + SSRF guard `assertPublicURL`),
   wiring in `internal/app/wire.go` (`newProxyWebFetchHandler`), dashboard passthrough. 12 handler tests + 11 adapter/helper tests + 5 usecase tests.
5. **`/v1/audio/speech` (TTS)** — Gemini-tts + OpenAI TTS; needs provider
   tts adapters (JS has `gemini.js` TTS branch).
6. **`/v1/audio/transcriptions` (STT)** — Gemini-stt + OpenAI STT.
7. **`/v1/audio/voices`** — static catalog, no upstream.
8. **`/v1/images/generations`** — image gen; Gemini image models are in the
   catalog now (T032), but the generation pipeline is unported.
9. **`/v1/videos/*`** (generations, edits, extensions, GET status) — Veo
   pipeline; async poll model.
10. **`/v1/api/chat`** — internal chat variant; needs spec check vs
    `/v1/chat/completions`.
11. **`/v1/responses/compact`** — responses sub-variant; needs spec.

After each `/v1/*` lands, the matching `/api/v1/*` stub flips to passthrough
(Tier 1 pattern).

## Recommendation

- **Do Tier 1 now** (cheap, 6 routes, single commit) — it removes the most
  user-visible stubs (the chat + models surface the dashboard calls).
- **Tier 2 is the real remaining work** — it is the same shape as the
  porting-completeness gap (T025): entire modality subsystems were not in the
  Go rewrite's original layer plan. Embeddings (T031b) is the next bounded
  slice; the rest are larger and should be scoped individually, not batched.

## Decision

This audit is read-only. No code changed. The actionable output is:
1. Tier 1 passthrough (bounded Worker task).
2. T031b embeddings (already tracked).
3. Each Tier 2 modality as its own future Worker task, sized like T026.