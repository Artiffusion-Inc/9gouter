# Agent Readiness Report

> Assessment date: 2026-07-21 (updated after #68–#72: coverage pass + `.golangci.yml` + solo-flow recalibration)
> Verdict, not score. Weighted by the project's lifecycle stage and by what actually lives in this repo.
> Framework: judgment-based agent readiness (Factory.ai-style binary checklist deliberately NOT used — see rationale in the skill).

## Headline Verdict: PARTIAL

Проект можно разрабатывать агентами, но на критичных путях они будут работать вслепую без усиления покрытия. Два load-bearing must-have присутствуют в какой-то форме, поэтому вердикт PARTIAL, а не NOT-READY: feedback loop есть и быстр (`go test -race ./...` ~30s, race-clean, 96 тест-файлов, golden contract tests из Vitest-снапшотов), observability есть (`slog` + `/health` + proxy fallback). Самый большой блокатор — не архитектура (она READY, clean arch с 0 нарушений domain→adapter/usecase/app), а **тонкое покрытие центральной трассы запроса**: после прохода #68–#70 поднято `openai translator` 12%→75.5%, `provider/base` 6%→88.5%, `provider/default` 12%→98%; ещё слабее — dashboard `api` handlers 38% и `translator` pkg 47%. Агент, меняющий оставшиеся непокрытые зоны (api handlers, translator↔provider цепочку), не получит детерминированный сигнал «оно работает» — ровно тот режим, в котором рождался недавний «malformed response» баг (Anthropic `event:` префикс), пойманный только ручным wire-capture. Второй блокатор — отсутствие correlation ID: агент не связывает логи одного запроса при дебаге мультикомпонентных багов.

> **Рабочий процесс одного разработчика (важно для интерпретации осей H/A):** пользователь один в репозитории, НЕ использует PR, пушит напрямую в `main`. Поэтому PR-CI и branch protection на `main` **не применимы** и НЕ предлагаются как рычаги. Детерминированная рельса — локальный линтер (`.golangci.yml`, закоммичен, см. Axis A), который разработчик запускает перед пушем. Оценка осей A/H откалибрована под этот solo-флоу.

## Project Context (determines what "ready" means here)

| Fact | Value |
|------|-------|
| Lifecycle stage | stabilizing (Go-rewrite готов, cutover pending, legacy JS dead-tree ~1167 файлов ещё в репо как rollback-референс) |
| System shape | single static binary (Go) + embedded Next.js static export; cross-language build-time (Go runtime, JS только `output:export` → `//go:embed`) |
| In-repo vs out-of-repo contours | user confirmed: **всё в репо / всё assessable из кода** — compliance/SAST/audit, metrics/alerting/SRE, branch policy не вынесены наружу |
| Tacit external-knowledge layers | provider OAuth credentials + per-account API keys (stored в SQLite, secrets-in-values); bcrypt dashboard hash. SaaS-аккаунты провайдеров (codex, gemini, kimi, cursor и т.д.) — per-user, вне кода |

## The Two Load-Bearing Must-Haves

| Must-have | Verdict | Why it gates everything |
|-----------|---------|-------------------------|
| 1. Automated feedback loop (scenarios) | PARTIAL | Цикл есть и быстр; контрактные тесты есть; но центральная трасса (openai translator, provider/base, api handlers) под покровом → агент там шипает непроверенное поведение. |
| 2. Observability — logs & traces | PARTIAL | Структурированные логи + health + fallback есть; но нет correlation/request ID и distributed tracing — агент не связывает логи одного запроса. |

> Оба PARTIAL (не NOT-READY), поэтому headline = PARTIAL. Не READY, потому что гэпы покрытия критичных путей и отсутствие correlation ID — это ровно те места, где агент будет отчитаться «всё ок» и отгрузить broken behavior.

## Axis Verdicts

| Axis | Verdict | One-line evidence | Highest-leverage next action |
|------|---------|-------------------|------------------------------|
| A. Deterministic rails | PARTIAL | `.golangci.yml` закоммичен (depguard для domain↮adapter/usecase/app, govet/staticcheck/errcheck/unused/ineffassign + style-линтеры green); baseline 81 known finding; НЕТ CI-гейта (solo-флоу — рельса локальная, запускается перед пушем) | держать depguard зелёным (он green); инкрементально чистить baseline (unused/errcheck/staticcheck); `govulncheck` шаг локально перед релиз-тегом |
| B. Architecture & boundaries | READY | clean arch: domain→usecase→adapter; 0 нарушений domain→adapter/usecase/app (depguard-enforced); typed domain interfaces (`Executor`,`Provider`,`Repo`,`Store`,`OIDCPort`); `internal/app/wire.go` composition root. Caveat: `adapter → usecase` ≠ 0 (~10 файлов `api/*.go` импортируют `managedashboard`/`videoproxy`) — НЕ нарушение clean arch в строгом виде (adapter может зависеть от usecase), но снижает тестируемость usecase в изоляции | вынести usecase→adapter доступ через pure ports (сейчас `managedashboard`/`rtk_bridge` импортируют adapter напрямую) |
| C. Feedback loop | PARTIAL | race-clean ~30s; golden contract tests (4 Vitest-снапшота, embed); v1 endpoint tests; shadowdiff harness; после #68–#70 openai translator 75.5%, provider/base 88.5%, provider/default 98% — но api handlers 38%, translator pkg 47% | сценарные + unit тесты на оставшиеся критичные зоны (api handlers, translator↔provider цепочка, managedashboard) |
| D. Observability | PARTIAL | `slog` (26 файлов, 90 вызовов); `/health` + `/api/health`; proxy fallback (`internal/adapter/transport/proxy/fallback.go`); НЕТ correlation ID, НЕТ tracing, НЕТ error tracking wiring | пронизывающий request ID через slog + `x-request-id` header |
| E. Explication | PARTIAL | `CLAUDE.md` отличный (architecture map, key-files table, patch tables); `docs/cutover-runbook.md`; specs/plans/goals. Гэп: `docs/ARCHITECTURE.md` устарел (2026-02-06, описывает Next.js как primary = legacy), нет ADR, нет glossary | обновить `ARCHITECTURE.md` под Go-runtime; добавить ADR для ключевых решений (format-pivot, account-fallback, event-prefix) |
| F. Environment reproducibility | READY | `.env.example`; `container-compose.yml`; `Containerfile` (distroless, multi-arch); `scripts/build-dashboard.sh`; DB migrations в репо; локально CGO=0 собирается | — |
| G. Security as architecture | PARTIAL | HMAC session + bcrypt + OIDC (`internal/adapter/auth`); proxy fallback на external calls. Log sanitization не проверен выборочно (потенциальная PII/secrets в логах) | sampled audit логов на PII/secrets; `govulncheck` локально перед релиз-тегом |
| H. SDLC alignment & autonomy | PARTIAL | CI только на tag-push (container) — solo-флоу без PR; локальная pre-push рельса = `.golangci.yml` + `go test -race`; нет multi-agent review; branch policy = convention (один разработчик, не enforced) | расширить локальную pre-push рельсу (`govulncheck`); multi-agent review на критичные изменения (translator, proxy stack) через workflow-оркестрацию по запросу |

> Verdicts: READY / PARTIAL / NOT-READY / OUT-OF-REPO (owned elsewhere — confirmed with user: none) / N/A (lifecycle-inappropriate).

## Axis Detail

### Axis A — Deterministic rails: PARTIAL

Детерминированная рельса теперь в репо: `.golangci.yml` (golangci-lint v2.12.2) закоммичен и валиден (`golangci-lint config verify` = ok). Включены depguard (архитектурное правило `domain ↮ adapter/usecase/app`, **0 нарушений**, MUST-stay-green), govet, staticcheck, errcheck, unused, ineffassign + style-линтеры misspell/copyloopvar/dupword/nakedret (все green). Baseline на 2026-07-21: 81 known finding (unused 28, staticcheck 29, errcheck 20, ineffassign 4) — это техдолг, не регрессии; новые находки из этого набора — actionable. Legacy JS dead-tree (~1167 файлов) исключён из lint через `paths`. CI-гейта НЕТ — solo-флоу: разработчик запускает `golangci-lint run ./internal/... ./cmd/... ./tools/...` перед пушем в `main` (PR-CI и branch protection не применимы к этому рабочему процессу — см. заметку в headline).

- Evidence: `.golangci.yml` (committed, depguard rule green); `golangci-lint config verify` → ok; baseline 81 findings; `which golangci-lint` → v2.12.2.
- Debunked-form check: dead-code detection не установлен как pass/fail (и не нужен); legacy dead-tree — известный problem-to-solve (T021 cutover), не box to tick; `unused`-линтер присутствует, но его 28 находок — это baseline, не блокер.
- Next action: держать depguard зелёным (он уже green — это детерминированный guard против cross-layer импортов, которые агент легко введёт вслепую); инкрементально чистить baseline (unused → удалить мёртвый код; errcheck → проверить возвращаемые ошибки tx.Rollback/json.Unmarshal/fmt.Sscanf; staticcheck → рефакторить); добавить `govulncheck` шаг локально перед релиз-тегом.

### Axis B — Architecture & boundaries: READY

Clean architecture выражена явно и типизирована: `internal/{domain,usecase,adapter}` + composition root `internal/app/wire.go` (620 строк). Domain определяет порты (`Executor`, `Provider`, `Repo`, `Store`, `OIDCPort`). Проверка нарушений слоёв: `grep internal/domain → internal/{adapter,usecase,app}` = 0 (теперь enforced depguard-правилом в `.golangci.yml`). Это самый сильный axis — агент может уважать явные границы, а depguard его «шлёпнет по рукам» за cross-layer импорт. Caveat 1: `usecase` напрямую импортирует `adapter` (`managedashboard/*`, `rtk_bridge.go`, `imageproxy`, `ttsproxy`, `proxyembeddings`) — не через pure ports, а через конкретные adapter-типы; это не радикальное нарушение слоистой модели, но снижает тестируемость usecase в изоляции. Caveat 2: `adapter → usecase ≠ 0` — ~10 файлов `internal/adapter/transport/http/api/*.go` импортируют `internal/usecase/managedashboard` (+ `v1video.go` → `videoproxy`). В строгой clean arch adapter может зависеть от usecase (это допустимое направление), но это создаёт цикл зависимостей при попытке вынести usecase-порты в domain — текущая структура не нарушает depguard, но и не изолирует usecase.

- Evidence: `internal/app/wire.go`; `internal/domain/{provider,usage,auth}/*.go` (interfaces); `grep` domain→lower = 0; depguard rule green; `grep adapter → usecase` = ~10 hits in `api/*.go` + `v1video.go`.
- Next action: для критичных usecase (`proxychat`) доступ к adapter уже через `V1Deps` инъекции — распространить этот паттерн на `managedashboard` (вынести repo-порты в domain) для тестируемости и разрыва adapter→usecase края.

### Axis C — Feedback loop: PARTIAL

Полный `go test -race -count=1 ./...` — зелёный, race-clean, ~30s (отличная скорость цикла). 93 test-файла / 181 source. Контрактные golden tests (`tests/golden/`) портированы из Vitest-снапшотов — 4 fixture-файла (`golden-request`, `golden-response-stream`, `golden-translator-concerns`, `golden-url-header`), embed через `//go:embed`, покрывают `translator/{claude,codex,cursor,gemini,kiro,commandcode,ollama,openai}`. v1 endpoint tests обширны (`v1_test.go`, `v1apichat_test.go`, `v1responses_test.go`, ...). `tools/shadowdiff` — harness для shadow-diff против legacy.

**Гэп покрытия (после прохода #68–#70, total ~50%, неравномерно):**

| Зона | Coverage | Значение |
|------|---------|----------|
| accountfallback / videoproxy / format / proxyfetch / embedding | 87–92% | сильная |
| auth (usecase) / resolver / tokenrefresh / transport/http | 70–82% | норма |
| **provider/base / provider/default / openai translator** | **88.5 / 98 / 75.5%** | поднято в #68–#70 — центральная трасса теперь под тестом |
| db/repo / proxy (stack) / proxychat | 59–67% | средне, proxychat критичен |
| translator (pkg) / api (handlers) | 47 / 38% | слабо — следующий фокус |
| rtk / webfetch / config / sqlite | 22–57% | слабо-средне |

`translator/{claude,codex,cursor,gemini,kiro,commandcode}` формально `[no test files]`, но покрыты golden contracts — это корректный сценарный подход. Реально без тестов: `managedashboard`, `app`, `domain/{auth,settings}`, большинство `provider/*` адаптеров (github, qwen, kiro, vertex, ...). В ходе #69 найден реальный баг транслятора (роль `tool` отфильтровывается в `openaiToOpenaiResponsesRequest` до проверки роли tool) — закреплён в тесте как текущее поведение; фикс логики — отдельная задача.

- Evidence: `go test -cover` per-pkg (provider/base 88.5%, provider/default 98%, openai translator 75.5%); `go test -race ./...` ~30s green; `tests/golden/`; `internal/usecase/proxychat/nonstream_translate_test.go`.
- Next action: сценарные тесты на `openai↔claude↔gemini↔ollama` non-stream + stream translation (расширить golden fixture-набор), unit на `api` handlers (38% → цель 70%+), `managedashboard` (0% — критичный usecase без тестов). Это снимет оставшийся режим «malformed response»-класса багов.

### Axis D — Observability: PARTIAL

In-code: `log/slog` используется в 26 файлах, 90 вызовов (`slog.Info/Warn/Error/Debug`). Health endpoints: `/health` (`internal/adapter/transport/http/server.go:56`) и `/api/health` (`internal/adapter/transport/http/api/health.go:9`). External-call fallback: `internal/adapter/transport/proxy/fallback.go` (cached parallel probe), fast-fail, health cache — полноценный proxy-stack. Error tracking wiring — отсутствует (нет sentry/errtrack). Distributed tracing — отсутствует. **Correlation/request ID — отсутствует**: нет `x-request-id`, нет request-scoped slog handler. Это конкретный гэп — при дебаге мультикомпонентного бага (proxychat → translator → provider → proxy stack) агент не свяжет логи одного запроса.

- Evidence: `grep slog.` → 90 hits; `grep requestID/correlation/trace_id` → нет в Go-коде (только в JS-asset chunks и одном тесте); `/health` endpoints присутствуют.
- Next action: middleware, генерирующее `x-request-id` (или принятие из header) + `slog.New` с request-scoped handler, прокидывающим `request_id` в каждое log-поле. Дешёвый, высокий-leverage фикс.

### Axis E — Explication: PARTIAL

Сильная сторона — `CLAUDE.md` (project instructions) делает explicit: architecture map, key-files-to-edit table, patch tables (#2703 fix series, JS patches ported), env vars, build/run, container, upstream sync. `docs/cutover-runbook.md` — pre-flight gates, go-live, rollback, deletion criteria. `docs/specs/`, `docs/plans/`, `docs/goals/` — design + plan + goal trail для rewrite.

Гэпы: `docs/ARCHITECTURE.md` датирован 2026-02-06 и описывает **Next.js как primary runtime** (= legacy) — устарел относительно Go-rewrite, введёт агента в заблуждение. Нет ADR (Architecture Decision Records) — ключевые решения (format-pivot через OpenAI, account-fallback loop, `event:` prefix, `_openaiIntermediate` drop, reasoning stall timeout) зафиксированы только в commit-сообщениях и CLAUDE.md patch-table. Нет glossary (domain-термины: combo, connection, node, proxy-pool, alias, mitm — рассеяны). Doc↔code cross-link lint отсутствует.

- Evidence: `CLAUDE.md` (отличный); `docs/ARCHITECTURE.md` (stale 2026-02-06, Next.js-primary); `docs/cutover-runbook.md`; нет ADR-директории.
- Next action: переписать `docs/ARCHITECTURE.md` под Go-runtime (или удалить и сослаться на CLAUDE.md — single source of truth); добавить `docs/adr/` для 5–6 ключевых решений; краткий glossary в CLAUDE.md или `docs/glossary.md`.

### Axis F — Environment reproducibility: READY

Локальная воспроизводимость: `CGO_ENABLED=0 go build -o 9gouter ./cmd/9gouter` — работает (проверено). `scripts/build-dashboard.sh` (bun → static export → `dashboard_assets/`); `dashboard_assets/.gitkeep` для clean-clone compile. DB schema в репо (`internal/adapter/db/migrations/`, `schema.go`). Env template: `.env.example`. Container: `Containerfile` (multi-stage: Bun static export → Go CGO=0 static → distroless), `.containerignore`, `container-compose.yml` (port 20127, volume). Multi-arch CI (amd64+arm64) в `container-publish.yml`.

- Evidence: `Containerfile`; `container-compose.yml`; `.env.example`; `scripts/build-dashboard.sh`; `internal/adapter/db/migrations/`.
- Next action: — (strategic: cloud-deployable dev environment для масштабирования на много агентов — см. ниже).

### Axis G — Security as architecture: PARTIAL

Отдельные контуры: DB security (SQLite, modernc pure-Go, credentials в values в БД), API security (HMAC session cookie `auth_token` + bcrypt password hash + OIDC через `coreos/go-oidc`), web surface (dashboard /api, session-auth protected), proxy surface (SOCKS5, strictProxy, MITM bypass). External-call fallback присутствует (proxy stack). Log sanitization — **не проверен выборочно**: `slog` вызовы могут логировать request bodies / headers с credentials (потенциальная PII/secrets утечка). Secret management strategy: secrets в SQLite (encrypted-at-rest через OS file perms) + env vars (`SESSION_SECRET`, `DASHBOARD_SESSION_SECRET`) — стратегия валидна (SOPS-style не требуется). Vuln tracking: `govulncheck` не в CI; auto-update не настроен (правильно).

- Evidence: `internal/adapter/auth/` (session, oidc, loginlimiter); `internal/adapter/transport/proxy/` (fallback, fastfail, mitmbypass); `internal/adapter/config/config.go` (env: SESSION_SECRET, DASHBOARD_SESSION_SECRET).
- Next action: sampled audit логов на PII/secrets (grep `slog.*Body|slog.*Header|slog.*token` в internal); `govulncheck` шаг в CI.

### Axis H — SDLC alignment & autonomy: PARTIAL

**Solo-флоу без PR:** один разработчик пушит напрямую в `main`, PR не используются, branch protection в GitHub не настроена (и не нужна — это convention одного человека, а не командное правило). CI триггерится только на `v*` tag push или `workflow_dispatch` (container build) — нет автоматического gate на каждый пуш. Поэтому детерминированная рельса — **локальная pre-push**: `golangci-lint run` (`.golangci.yml` закоммичен, depguard green) + `go test -race ./...` разработчик запускает перед отправкой. Это рабочий контракт: нет CI-гейта, но есть детерминированный инструмент в репо. Risk: nothing forces the run — broken change может дойти до `main` если разработчик забыл запустить (как недавний «malformed response» дошёл до ветки без проверки). Multi-agent review с distinct lenses — отсутствует как постоянная практика (доступна через workflow-оркестрацию по запросу). Second engine for review — отсутствует. Feature flags: `PROXY_AUTO_SELECT_ENABLED`, `PROXY_DISPATCHER_CONNECTIONS` — env-флаги с разумным количеством, не code smell.

Autonomy per stage: planning/spec → human (specs/plans/goals workflow присутствует); code+tests → highly automatable (локальная рельса есть, CI-гейта нет — зависит от дисциплины); deploy/ops → tag-triggered CI, team-dependent (правильно — агентам не доверяем prod).

- Evidence: `.github/workflows/container-publish.yml` (tag-only trigger); нет PR-CI workflow (намеренно — solo-флоу); `.golangci.yml` закоммичен как локальная рельса; branch protection не в репо (не применимо).
- Next action: расширить локальную pre-push рельсу (`govulncheck` перед релиз-тегом); для критичных изменений (translator, proxy stack) — multi-agent review через workflow-оркестрацию по запросу (parallel reviewers: correctness, security, contract). Не вводить PR-CI/branch protection — они не соответствуют solo-флоу.

## Roadmap (ranked by leverage, weighted by lifecycle stage)

> Not "count of failing binary criteria". Ranked by how much each fix unblocks agentic development for THIS project at THIS stage (stabilizing).

### Top 3 highest-leverage fixes

1. **Axis C — продолжить покрытие критичной трассы**: `openai translator` 12→75.5%, `provider/base` 6→88.5%, `provider/default` 12→98% уже подняты в #68–#70. Следующий фокус — `api` handlers (38%), `translator` pkg (47%), `managedashboard` (0%): сценарные + unit тесты, расширить golden-fixture для `openai↔claude↔gemini↔ollama` non-stream+stream. **Impact:** снимает оставшийся класс «malformed response»-багов; агент получит детерминированный сигнал на всём центральном пути, а не только на translator/base/default.
2. **Axis A — держать и чистить локальную lint-рельсу**: `.golangci.yml` закоммичен (depguard green — детерминированный guard против cross-layer импортов, которые агент легко введёт). Инкрементально чистить baseline 81 (unused → удалить мёртвый код; errcheck → проверить tx.Rollback/json.Unmarshal/fmt.Sscanf; staticcheck → рефакторить); добавить `govulncheck` локально перед релиз-тегом. **Impact:** depguard — единственный детерминированный архитектурный гейт в solo-флоу без PR; baseline-чистка снижает шум и делает новые регрессии видимыми. (PR-CI/branch protection НЕ применяются к solo-флоу — не предлагать.)
3. **Axis D — correlation ID**: middleware `x-request-id` + request-scoped `slog` handler. **Impact:** агент дебажит мультикомпонентные баги (proxychat → translator → provider → proxy stack), связывая логи одного запроса. Дешёвый фикс, мультипликатор для расследований.

### Quick wins (low effort, real signal)

1. `govulncheck` локально перед релиз-тегом (solo-флоу: нет PR-CI, поэтому уязвимости проверяются вручную перед `v*` tag) — Effort: Low (один шаг в pre-push рельсу).
2. Переписать/удалить устаревший `docs/ARCHITECTURE.md` (описывает Next.js как primary) — Effort: Low (или удалить, сослаться на CLAUDE.md как single source of truth).
3. Sampled audit `slog` вызовов на PII/secrets (`grep -rn "slog.*Body\|slog.*Header\|slog.*token" internal`) — Effort: Low.
4. Почистить baseline `.golangci.yml` (28 unused — удалить мёртвый код: `intPtr`/`floatPtr`, `versionResponse`, `proxychatDomainProvider`, stub-трансляторы в codex/cursor) — Effort: Low-Medium, снимает шум и делает новые unused-находки видимыми.

### Strategic / forward-looking

- **Cloud-deployable dev environment** (Axis F strategic): env-as-code sandbox для масштабирования на десятки агентов — orchestration layer ещё зреет, но `Containerfile` + `container-compose.yml` уже близки.
- **Multi-agent review с distinct lenses** (Axis H): для verification критичных изменений (translator, proxy stack) — parallel reviewers (correctness, security, contract), mandatory result recording. Second engine на review ловит blind spots writer-engine.
- **Cutover T021** (explication): удаление legacy JS dead-tree (~1167 файлов) сократит repo-noise для агента и сделает dead-code problem решённой. Разрушительно — требует явного одобрения.
- **ADRs** для ключевых решений (format-pivot, account-fallback, event-prefix, `_openaiIntermediate` drop, reasoning stall timeout) — превратить commit-сообщения в retrievable decision context.

## Verdicts Likely To Shift Soon

> Agentic tooling moves on a ~6-month cadence. Re-run the assessment then.

- **Axis H — multi-agent orchestration**: deterministic workflow (orchestration code + agent stages) для review/verification — pattern зреет; за 6 месяцев может стать must-have для hard-verification stages (translator contracts).
- **Axis F — cloud dev environments**: sandbox services / env-as-code для scaling на много агентов — orchestration layer матуреет; strategic capability может перейти в current must-have.
- **Axis A — arch-boundary enforcement**: depguard-правило (domain↮lower layers) теперь green и закоммичено; через 6 месяцев ожидаем расширение до adapter↮usecase-границы (сейчас ~10 нарушений в `api/*.go` — известная проблема, не блокер).
- **Axis E — agent traceability (retro-look loop / LLM-gateway)**: pattern для turn recurring agent struggles into permanent rails; не текущий must-have, но сильный positive когда появится — переоценить.