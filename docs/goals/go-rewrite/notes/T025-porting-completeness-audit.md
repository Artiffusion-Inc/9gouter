# T025 — JS→Go Porting Completeness Audit

Source: Scout agent + PM ground-truth verification (mechanical route extraction).
Date: 2026-07-19.

## Method

Two passes:
1. Scout agent (goal-scout) classified JS surface into ported/partial/missing.
2. PM rejected scout as unreliable (subagent_tokens:0, name-matching only) and
   rebuilt ground truth mechanically: extracted ALL Go `mux.HandleFunc` route
   patterns (`grep -rhoE '"(GET|POST|...) /..."'`), all JS `route.js` paths, and
   spot-checked handler bodies for real vs stub behavior.

## Findings: scout was unreliable on `/api/*`, reliable on `/v1/*`

Scout reported ~57 missing + 16 partial. PM verification shows **most `/api/*`
"missing" were false negatives** — scout looked for handler functions in
specific files but Go registers routes spread across `models.go`,
`headroom.go`, `tunnel.go`, `proxypools.go`, `clitools.go`. Those routes ARE
registered. Confirmed ported (scout said missing):
- `/api/models`, `/api/models/alias|custom|disabled|availability|test`
- `/api/proxy-pools/{id}` GET/PUT/DELETE
- `/api/headroom/{status,extras,start,stop,restart,proxy/*}`
- `/api/tunnel/{enable,disable,status,...}`
- `/api/cli-tools/*` (all 14+ tools, GET/POST/DELETE/PATCH)
- `/api/shutdown`, `/api/version/shutdown`, `/api/version/update`

Go has **213 registered route patterns** vs JS **147**. Route-count parity is
not the issue.

## The real gap: handler bodies are STUBS, not missing routes

The deeper problem scout under-reported: many Go `/api/*` handlers exist but
return hardcoded JSON without doing the work. Spot-confirmed stubs:

- `tunnel.enable` / `tunnel.disable` → `{"success":true,"enabled":false}`,
  no shell exec (scout said "doesn't execute shell command" — CORRECT).
- `v1_dashboard.proxy` (ALL `/api/v1/*` — 20 routes) →
  `{"success":false,"message":"not available in Go build; use /v1 directly"}`.
  These are intentional passthrough stubs; the real client surface is `/v1/*`.

So the matrix needs a third classification beyond missing/partial/ported:
- `stub` — route + handler registered, returns hardcoded JSON, does nothing.
This requires reading every handler body, not grepping registrations. Scout
cannot do this reliably in one pass.

## Ground-truth: the OpenAI client surface `/v1/*` (the actual regression)

This is the clean, verified result. JS `/v1/*` routes vs Go real (`v1.go`)
vs Go stub (`v1_dashboard.go` returns "not available"):

| JS route | Go real? | Go stub? | Status |
|----------|----------|----------|--------|
| POST /v1/chat/completions | ✅ | (also stub) | **ported** |
| POST /v1/messages | ✅ | (also stub) | **ported** |
| POST /v1/responses | ✅ | (also stub) | **ported** |
| GET /v1/models | ✅ | passthrough | **ported (static catalog MVP, #2702)** |
| GET /v1/models/info | ✅ | passthrough | **ported (T033b, static-catalog subset)** |
| GET /v1/models/{kind} | ✅ | passthrough | **ported (static catalog MVP)** |
| POST /v1/embeddings | ✅ | passthrough | **ported (T031b)** |
| POST /v1/audio/speech | ✅ | ported | TTS: openai/gemini/elevenlabs/minimax/inworld/cartesia/playht/nvidia/deepgram (T033b-3) |
| POST /v1/audio/transcriptions | ✅ | ported | multipart STT: openai/groq/deepgram/assemblyai/gemini (T033b-4) |
| GET /v1/audio/voices | ✅ | passthrough | **ported (T033b-5, static catalog)** |
| POST /v1/images/generations | ✅ | ported | image gen: openai-compat/gemini/codex (T033b-6) |
| POST /v1/messages/count_tokens | ✅ | passthrough | **ported (T031b)** |
| POST /v1/responses/compact | ✅ | passthrough | responses sub-variant (T033b-8 ported) |
| GET /v1/responses/{id} | ✅ | 501 stub | OpenAI RetrieveResponse poll — honest 501 (no upstream returns LRO Responses state; T033b-8) |
| POST /v1/search | ✅ | ported | web-search: serper/tavily/searxng dedicated + gemini/openai/perplexity-chat searchViaChat (T033b-1) |
| POST /v1/web/fetch | ✅ | ported | adapter+usecase+handler+SSRF guard (T033b-2) |
| POST /v1/api/chat | ✅ | ported | OpenAI SSE→Ollama NDJSON transform over proxychat (T033b-8) |
| POST /v1/videos/generations | ✅ | ported | xAI LRO raw-byte proxy + idempotency (T033b-7) |
| POST /v1/videos/edits | ✅ | ported | xAI LRO raw-byte proxy (T033b-7) |
| POST /v1/videos/extensions | ✅ | ported | xAI LRO raw-byte proxy (T033b-7) |
| GET /v1/videos/{id} | ✅ | ported | xAI LRO poll, provider fixed to xai (T033b-7) |
| GET /v1 (root) | — | root ok | ported (trivial) |
| GET /v1beta/models | ✅ | ported | Gemini-shaped catalog from provider.AllCatalogs (T032) |
| POST /v1beta/models/{path...} | ✅ | ported | Gemini→OpenAI req transform → handleChat → OpenAI→Gemini SSE/JSON; TTS-forward honest 501 (T032) |

**Client `/v1/*` summary: 23 of 23 real. Last client-surface gap (#38 / T032) closed.**

## Services (lifecycle) — PORTED (audit updated post-T027b/T030b)

- `open-sse/services/tokenRefresh.js` + `tokenRefresh/dedup.js` +
  `tokenRefresh/providers.js` → **PORTED (T027b)** at
  `internal/adapter/provider/resolver/tokenrefresh/` (kiro, xai/grok-cli,
  copilot refreshers; clinepass/kimchi use static creds; qoder stub).
- 6 live-model resolvers (kiro/qoder/kimchi/copilot/clinepass/grokCli) →
  **PORTED (T030b)** at `internal/adapter/provider/resolver/`, wired in
  `internal/app/wire.go` via `resolver.Register`. (qoder is a stub: COSY
  signing not yet ported.)
- `open-sse/providers/capabilities.js` (service-kind → capability map) →
  **INLINE (T026/T028)** as `ServiceKinds` on `provider.ProviderCatalog`
  in `internal/adapter/provider/registry.go`; `/v1/models` kind filtering
  consumes it directly (`kindsIntersect` in v1models.go).
- `src/shared/constants/models.js` + `providers.js` (PROVIDER_MODELS,
  AI_PROVIDERS, serviceKinds, alias/kind helpers) → **PORTED (T026)** as
  `provider.AllCatalogs` / `provider.ProviderCatalog` in
  `internal/adapter/provider/registry.go`.

## The dominant gap cluster — CLOSED

**Server-side provider lifecycle** (token refresh + live-model resolvers +
provider/model constants + capabilities) is ported. The remaining lifecycle
surface is #2703 Fix 2–5: **Fix 5 (diagnostics)**, **Fix 4 (sticky selection)**,
and **Fix 3 (structured failure types + fallback loop)** are DONE (commits
`2e035f2`, `8d57c09`, Fix 3); **Fix 2 (route-aware OAuth refresh pipeline)** is
the last open slice, tracked separately, not a cutover blocker. `/v1beta/models`
(T032 / #38) is the last client-surface gap.

## Recommended Worker batch (largest-safe-slice ordering)

1. ~~T026 — Provider constants port~~ **DONE**.
2. ~~T027 — Token refresh pipeline~~ **DONE (T027b)**.
3. ~~T028 — Capabilities mapping~~ **DONE (inline ServiceKinds)**.
4. ~~T029 — GET /v1/models + /v1/models/{kind} + /v1/models/info~~ **DONE**.
5. ~~T030 — Live-model resolvers~~ **DONE (T030b)**.
6. ~~T031 — /v1/embeddings + /v1/messages/count_tokens~~ **DONE (T031b)**.
7. ~~T032 — /v1beta/models~~ **DONE (Gemini-shaped GET list + POST generateContent/streamGenerateContent proxy; TTS-forward honest 501)**.
8. ~~T033 — Stub audit~~ **DONE (T033-api-v1-stub-audit.md)**.

## Originally deferred — now ported (T031b/T033b)

The original audit deferred these as P2/niche; all have since landed:
- `/v1/audio/speech` + `/v1/audio/transcriptions` + `/v1/audio/voices`
  (T033b-3/4/5), `/v1/images/generations` (T033b-6), `/v1/videos/*`
  (T033b-7), `/v1/search` (T033b-1), `/v1/web/fetch` (T033b-2),
  `/v1/api/chat` (T033b-8), `/v1/responses/compact` + `GET /v1/responses/{id}`
  (T033b-8), `/v1/embeddings` + `/v1/messages/count_tokens` (T031b).

## Still open

- #2703 **Fix 2** (route-aware OAuth refresh pipeline — TokenRefresher takes
  proxy opts, dedupRefresh + withCredentialRefreshLock, proactive + reactive
  401/403 refresh-retry). Fix 3/4/5 are DONE.
- Gemini-native TTS forward (raw-byte proxy to
  generativelanguage.googleapis.com with the credential fallback loop) —
  honest 501 in /v1beta POST; follow-up slice. Non-blocking (use /v1/audio/speech).

## Conclusion for the board (updated post-port)

The original audit (2026-07-19) concluded the server-side provider lifecycle
and most `/v1/*` endpoints were unported. That is **no longer accurate** as of
the T026–T033b batch:

- **Server-side provider lifecycle** (token refresh, live-model resolvers,
  provider/model constants, capabilities) is **ported** (T026/T027b/T028/T030b)
  — the systemic gap behind #2702/#2703 on the lifecycle axis is closed.
- **OpenAI client surface `/v1/*`** is **21 of 23 real endpoints** (see table
  above). The 2 remaining are both `/v1beta/models` (Gemini-native proxy,
  P1) — tracked as #38 / T032, not a cutover blocker.
- The remaining lifecycle surface is **#2703 Fix 2** (route-aware OAuth
  refresh pipeline). Fix 3 (structured failure types + fallback loop), Fix 4
  (sticky round-robin), and Fix 5 (route diagnostics) are DONE — tracked
  separately, not blocking cutover.

The Go binary can now serve OAuth providers long-term and report its model
catalog to clients. Open client-surface items: `/v1beta/models` (#38).