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
| GET /v1/models | ❌ | stub | **MISSING (P0, #2702)** |
| GET /v1/models/info | ✅ | passthrough | **ported (T033b, static-catalog subset)** |
| GET /v1/models/{kind} | ❌ | stub | **MISSING (P1)** |
| POST /v1/embeddings | ❌ | stub | **MISSING (P1)** |
| POST /v1/audio/speech | ✅ | ported | TTS: openai/gemini/elevenlabs/minimax/inworld/cartesia/playht/nvidia/deepgram (T033b-3) |
| POST /v1/audio/transcriptions | ✅ | ported | multipart STT: openai/groq/deepgram/assemblyai/gemini (T033b-4) |
| GET /v1/audio/voices | ❌ | stub | **MISSING (P2)** |
| POST /v1/images/generations | ✅ | ported | image gen: openai-compat/gemini/codex (T033b-6) |
| POST /v1/messages/count_tokens | ❌ | stub | **MISSING (P1)** |
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
| GET /v1beta/models | ❌ | — | **MISSING (P1, Gemini-compat)** |
| GET /v1beta/models/{path...} | ❌ | — | **MISSING (P1, Gemini-compat)** |

**Client `/v1/*` summary: 11 of 23 real. 12 missing (mostly stubs in dashboard proxy).**

## Services (lifecycle) — verified missing

- `open-sse/services/tokenRefresh.js` + `tokenRefresh/dedup.js` +
  `tokenRefresh/providers.js` → **MISSING (P0)**. No token refresh in Go at all.
  Confirmed by grep: zero `refreshCredentials`/`RefreshToken` in internal/.
- 6 live-model resolvers (kiro/qoder/kimchi/copilot/clinepass/grokCli) →
  **MISSING (P0)**. Block on token refresh.
- `open-sse/providers/capabilities.js` (service-kind → capability map) →
  **MISSING (P1)**. Needed by `/v1/models` kind filtering.
- `src/shared/constants/models.js` (PROVIDER_MODELS, PROVIDER_ID_TO_ALIAS,
  getModelKind) + `src/shared/constants/providers.js` (AI_PROVIDERS,
  isAnthropicCompatibleProvider, isOpenAICompatibleProvider, getProviderAlias,
  serviceKinds) → **MISSING (P0)** — `/v1/models` and kind logic depend on these
  constants. This is large: the static model catalog for every provider.

## The dominant gap cluster

**Server-side provider lifecycle**: token refresh + live-model resolvers +
provider/model constants + capabilities. This single cluster blocks
`/v1/models`, `/v1beta/models`, `/api/models/availability`, and every
OAuth provider's long-running auth. It is also exactly the #2703 Fix 2/3
surface (route-aware refresh). One coherent subsystem, not scattered bugs.

## Recommended Worker batch (largest-safe-slice ordering)

1. **T026 — Provider constants port** (models.js + providers.js → Go).
   Pure data, no I/O. Unblocks /v1/models, capabilities, kind filtering.
2. **T027 — Token refresh pipeline** (tokenRefresh.js + dedup + providers).
   Unblocks live-model resolvers, #2703 Fix 2/3, OAuth provider auth.
3. **T028 — Capabilities mapping** (capabilities.js). Unblocks kind-aware
   /v1/models.
4. **T029 — GET /v1/models + /v1/models/{kind} + /v1/models/info** (the #2702
   fix). Static + custom + alias − disabled + kind filter, no live resolvers
   yet. Depends on T026 + T028.
5. **T030 — Live-model resolvers** (kiro/qoder/kimchi/copilot/clinepass/grokCli).
   Depends on T027. Restores "updates when thinking mode changes" (#2702).
6. **T031 — /v1/embeddings + /v1/messages/count_tokens** (P1, no new deps).
7. **T032 — /v1beta/models** (Gemini-compat). Depends on T026 + T028.
8. **T033 — Stub audit: classify every `/api/*` handler as real|stub|partial.**
   Read every handler body. This is the only way to know how many dashboard
   buttons silently do nothing (like tunnel.enable). Separate read-only task.

## NOT in scope of this audit (deferred)

- `/v1/audio/*`, `/v1/images/*`, `/v1/search`, `/v1/responses/compact`,
  `GET /v1/responses` — P2, niche. Port after P0/P1 stable. (`/v1/web/fetch`
  ported — T033b-2; `/v1/api/chat` ported — T033b-8; `/v1/videos/*` ported —
  T033b-7.)
- Dashboard `/api/*` stub-vs-real classification — needs T033 first.

## Conclusion for the board

The Go rewrite shipped the chat happy path and most dashboard routes, but
**the entire server-side provider lifecycle (token refresh, live model
discovery, provider/model constants, capabilities) was never ported**, and
**the OpenAI client surface is 3 of 23 endpoints**. This is the systemic gap
behind #2702 and #2703. It is one coherent subsystem — port it as a batch in
the ordering above before cutover, or the Go binary cannot serve any OAuth
provider long-term and cannot report its model catalog to clients.