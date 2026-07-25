# Паритет image-provider adapters — дизайн

**Дата:** 2026-07-24  
**Статус:** Одобрен  
**Область:** Go image-generation pipeline (`/v1/images/generations`)

## Цель

Убрать честные `501` из Go-пайплайна генерации изображений для всех адаптеров, уже поддержанных legacy JS: `sdwebui`, `comfyui`, `huggingface`, `fal-ai`, `black-forest-labs`, `stability-ai`, `runwayml`, `cloudflare-ai`, `nanobanana` и `antigravity`.

Каждый адаптер сохраняет зафиксированный ниже HTTP-контракт, использует допустимую transport boundary и нормализует успешный результат в существующий OpenAI-совместимый ответ `/v1/images/generations`.

## Не входит в scope

- Новый публичный endpoint или изменение существующей OpenAI-совместимой формы ответа.
- Video generation: у RunwayML в image pipeline поддерживаются только модели `gen4_image*`; video-модели остаются на `/v1/videos/*`.
- Полный ComfyUI graph-workflow: контракт — минимальный `{prompt}` passthrough.
- Combo fallback, account fallback, реактивный OAuth refresh и usage persistence для images: это отдельные известные gaps, не условие снятия transport `501`.
- Универсальная поддержка image edits для всех providers: расширенные image-edit поля разрешены только там, где это явно указано в provider matrix.

## Контракт входа и выхода

### Базовые поля

Вход сохраняет существующие поля `/v1/images/generations`: `model`, `prompt`, `n`, `size`, `quality`, `style`, `response_format`, `output_format`, `background`.

Дополнительные поля принимаются транспортным слоем, но не являются универсальным расширением OpenAI API:

- `image`, `images`, `mask_image`, `maskImage`, `mask` разрешены только для provider/model из явной capability table ниже;
- `width`, `height` разрешены там, где provider contract использует отдельные размеры;
- `negative_prompt`, `guidance`, `seed`, `num_steps`, `steps`, `strength` разрешены только для Cloudflare AI image models; multipart-модели принимают только named/dimension fields, но не edit inputs.

Transport сохраняет присутствие каждого optional JSON key с помощью `json.RawMessage`: отсутствующий key, `null`, `""` и `0` не сливаются. Любой key, включая `null` и `0`, считается supplied для capability check. Поддерживаемый `null`/пустая строка означает «не включать в upstream payload» по legacy contract; поддерживаемый numeric `0` передаётся как `0`. Неподдерживаемый supplied key всегда возвращает `400` до executor.

`mask_image`, `maskImage` и `mask` — три mutually-exclusive aliases одного canonical `mask`: допускается ровно один supplied alias; два и более alias дают `400`, даже если один из них `null` или пуст. После проверки canonical `mask` и исходный `image` типизируются provider-local resolver-ом.

Если поле не поддержано выбранным provider/model, handler возвращает `400` с сообщением вида `<provider> does not support <field> for this image model`. Поля не игнорируются молча.

### Capability table

Table default-deny: поля вне перечисленных строк всегда отклоняются до executor.

| Provider/model | Разрешённые extended fields | Canonical upstream mapping |
|---|---|---|
| `fal-ai/*` | `image` | `image_url`; один input |
| `black-forest-labs/flux-kontext-pro`, `flux-kontext-max` | `image` | `image_prompt`; один input |
| прочие `black-forest-labs/*` | — | — |
| `cloudflare-ai/@cf/runwayml/stable-diffusion-v1-5-img2img` | `image`, `width`, `height`, six named fields | JSON image keys ниже |
| `cloudflare-ai/@cf/runwayml/stable-diffusion-v1-5-inpainting` | `image`, один mask alias, `width`, `height`, six named fields | JSON image/mask keys ниже |
| остальные Cloudflare JSON image models | `width`, `height`, six named fields | JSON/multipart according to model set; no edit input |
| три Cloudflare FLUX.2 multipart models | `width`, `height`, six named fields | multipart; no image/mask |
| `runwayml/*`, `nanobanana/*` | — | legacy edit pass-through intentionally excluded from this Go parity tranche |
| все остальные matrix providers | — | — |

Это сознательное scope решение для RunwayML/Nanobanana: их legacy handlers принимали raw edit URLs, но Go transport не будет передавать неперепроверяемый URL третьей стороне и не умеет безопасно re-host data input. Поэтому оба вида fields возвращают pre-executor `400`; генерация text-to-image остаётся в scope.

### Image input security

Поддерживаемые provider-specific image inputs принимают:

- `data:image/<png|jpeg|webp>;base64,...` размером не более 16 MiB до декодирования;
- либо HTTPS URL.

Для URL-input действует SSRF guard до первого соединения и на каждом redirect: разрешён только `https`; запрещены localhost, loopback, link-local, RFC1918, carrier-grade NAT, unspecified и multicast IP, cloud metadata endpoints и домены `.internal`. Redirect не должен переводить запрос на запрещённый адрес. Fetch выполняется через image executor и имеет тот же egress policy, что provider lifecycle calls.

### Выход

Успех для `response_format=url` или `b64_json` возвращается в существующей форме:

```json
{"created": 0, "data": [{"url": "..."}]}
```

или:

```json
{"created": 0, "data": [{"b64_json": "..."}]}
```

`response_format=binary` отдаёт raw bytes первой картинки. При URL-result image download выполняется через тот же executor, что submit и polling; размер download ограничен 64 MiB. Raw upstream bytes принимаются только при magic-byte проверке PNG, JPEG или WebP. MIME и `Content-Type` binary response определяются по проверенному содержимому, а не по `output_format` или незаверенному upstream header.

## Архитектура

### Transport boundary

`imageproxy` получает минимальный `HTTPExecutor`:

```go
type HTTPExecutor interface {
    Do(*http.Request) (*http.Response, error)
}
```

Usecase не импортирует repositories, `proxy` package или HTTP transport. До вызова executor provider adapter прикрепляет к request context неизменяемый internal transport metadata: provider ID, credentials, origin connection ID и lifecycle phase. Метаданные не сериализуются и не передаются upstream.

Для untrusted image URL executor metadata также несёт immutable `ValidatedTarget`: original scheme/hostname/port, проверенный IP и redirect lineage. Только production proxy transport интерпретирует этот type; imageproxy создаёт его через injectable resolver после policy checks. При отсутствии pin-capable route image request отклоняется, а не переходит на непроверенный DNS dial.

Production composition root строит executor, который для connection-backed requests:

1. извлекает `_connectionId` из metadata credentials;
2. загружает соответствующий `ProviderConnection` из `ConnectionRepo`;
3. выполняет запрос через `proxy.ProxyAwareFetch`, `ProxyPoolRepo`, `ProxyOpts` и connection-level proxy settings;
4. при отсутствующей или недоступной connection возвращает ясную ошибку и **не** обходит назначенный маршрутизатор direct request;
5. наследует proxy options submit connection для poll, result-fetch и URL-to-binary download, даже когда URL находится на другом host.

`internal/adapter/transport/proxy` получает policy-aware request target contract и является единственной production boundary для pinning. Для direct HTTPS он dial-ит `ValidatedTarget.IP:port` при сохранении original hostname как TLS SNI и HTTP `Host`. Для HTTP/HTTPS proxy он строит CONNECT к pinned `IP:port`, сохраняя original hostname в TLS SNI/Host; для SOCKS5 он отправляет pinned IP:port. Relay и proxy-fallback не допускаются для untrusted URL, пока relay/fallback transport не принимает тот же pinned target contract; `strictProxy` error возвращается без direct fallback. `ProxyAwareFetch` получает injectable resolver/dial/CONNECT seam, чтобы recording integration test доказывал IP/port фактического egress, SNI и отсутствие DNS re-resolution. Обычные provider submit/poll URLs не являются untrusted image input и продолжают обычную proxy pipeline.

Обычный `http.Client` допускается только как явно документированный fallback для isolated tests и local constructor default. `internal/app/wire.go` обязан передавать production executor; отсутствие executor в production wiring — ошибка конфигурации.

### Исключения transport boundary

- **`sdwebui` и `comfyui`:** это `AuthTypeNone` local providers. Credential resolver возвращает no-auth virtual credentials без `_connectionId`; executor выполняет их direct only. Их targets намеренно fixed literal loopback origins из registry (`127.0.0.1:7860` и `127.0.0.1:8188`), а не configurable hostnames. Запросы к ним разрешены только для loopback viewer (`isLocalRequest`); внешний viewer получает `403`. Direct transport не следует redirects, не выполняет DNS и не использует connection proxy или proxy-pool rotation.
- **`antigravity`:** image adapter делегирует в image-capable Antigravity provider executor, а не в API-key Gemini path. Он строит non-streaming `POST /v1internal:generateContent`, `requestType: "image_gen"`, clean model без terminal `-<width>x<height>`, `generationConfig:{temperature:1,topP:0.95,topK:40,maxOutputTokens:8192,imageConfig:{aspectRatio}}`, text-only merged contents и session ID. Он извлекает inline image data из Gemini response. Этот executor сохраняет OAuth bearer auth, project-ID resolution, refresh, account pinning, MITM bypass и proxy routing. Для Antigravity запрещено помещать credential в query (`?key=`).

Исключения перечислены исчерпывающе. Все остальные submit, poll, result-fetch и binary download calls используют один connection-aware image executor.

### Adapter dispatch

Registry `image.Config` перестаёт маркировать реализованные providers `Unsupported`. `Handler.synthesize` выбирает provider-local build/parse/normalize logic. Общим остаётся только механизм polling; URL, auth headers, request shape, host validation и terminal statuses остаются у конкретного provider.

Provider, подставляющий model или account identifier в URL path, валидирует сегменты до формирования URL и экранирует их через `url.PathEscape`. Cloudflare `accountId` принимается только как 32-hex identifier или UUID; model сегменты подчиняются provider-specific allowlist и не могут содержать query, fragment или traversal.

### Polling lifecycle

Общий helper получает submit-derived poll URL, origin transport metadata и provider-local callback распознавания состояния. Он реализует legacy defaults:

- interval: `1500 ms`;
- overall timeout: `120 s`;
- завершение немедленно на `context` cancellation/deadline;
- no retry после terminal failure;
- provider-specific error при upstream non-2xx, malformed response, failed status или timeout.

Каждый lifecycle URL (submit redirect, poll, result URL и binary download) запрещает userinfo и требует HTTPS: `http` и HTTPS→HTTP redirect отклоняются до HTTP call. Credential headers переносятся только при том же canonical origin (`https`, hostname, effective port); provider allowlist сама по себе не разрешает перенос credential на иной host. Допускаются только provider documented API hosts: BFL — `api.bfl.ai` и `.bfl.ai`; fal-ai — `queue.fal.run` и `.fal.run`; RunwayML — `api.dev.runwayml.com`; nanobanana — host configured base URL. Неожиданный host — `502` с diagnostic `provider returned unexpected polling host`.

Cloudflare JSON contract: valid dimensions сначала derived from exact `size` regex `^\d+x\d+$`, затем independently supplied finite `width`/`height` overwrite their corresponding derived value. `image` serializes exactly as both `image_b64: string` and `image: []byte`; canonical mask serializes as `mask_b64: string`, `mask: []byte`, `mask_image: []byte`. Named fields include only non-null, non-empty supplied values and preserve numeric zero. Multipart models serialise `prompt`, dimensions and named fields as strings and never include image/mask keys.

Тестовая конфигурация позволяет сократить interval/deadline без sleep в 120 секунд. Production defaults остаются parity-значениями.

## Provider matrix

| Provider | Submit | Poll / result | Auth / boundary | Normalized output | Дополнительные ограничения |
|---|---|---|---|---|---|
| `sdwebui` | `POST http://127.0.0.1:7860/sdapi/v1/txt2img`, `{prompt,width,height,steps:20,batch_size}` | sync | none, local direct-only | `images[]` → `b64_json` | loopback viewer; direct literal target, no redirect/DNS |
| `comfyui` | `POST http://127.0.0.1:8188`, `{prompt}` | sync | none, local direct-only | legacy OpenAI-shaped body | loopback viewer; direct literal target, no redirect/DNS |
| `huggingface` | `POST /models/{model}`, `{inputs:prompt}` | sync raw binary | Bearer, connection-aware executor | bytes → `b64_json` | model URL-segment validation; raw bytes magic-check |
| `fal-ai` | `POST /{model}`, `{prompt,num_images,image_size,image_url}` | `status_url`, затем `response_url` | `Authorization: Key`, connection-aware executor | `images[]` / `image` → URL list | poll/result host allowlist |
| `black-forest-labs` | `POST /v1/{model}`, `{prompt,width,height,image_prompt}` | `polling_url` | `x-key`, connection-aware executor | `result.sample` → URL | poll host allowlist |
| `stability-ai` | `POST /{core|ultra|sd3}` | sync | Bearer, connection-aware executor | `image` → `b64_json` | endpoint выбирается по модели: `ultra`, `sd3`, иначе `core` |
| `runwayml` | `POST /text_to_image` для `gen4_image*` | `/tasks/{id}` | Bearer + `X-Runway-Version: 2024-11-06`, connection-aware executor | `output[]` → URL list | non-`gen4_image*` → `400`, использовать `/v1/videos/*`; image edit input intentionally returns `400` |
| `cloudflare-ai` | `/accounts/{accountId}/ai/run/{model}` | sync JSON или raw image | Bearer, connection-aware executor | JSON/raw image normalized to OpenAI | multipart только для трёх legacy FLUX.2 models; image/mask не поддержаны на multipart models и дают `400` |
| `nanobanana` | `/generate` с `callBackUrl: https://localhost/callback` | `record-info?taskId=` | Bearer, connection-aware executor | `resultImageUrl` / `originImageUrl` → URL | callback — обязательная dummy field provider contract; listener не создаётся; poll host allowlist; image edit input intentionally returns `400` |
| `antigravity` | image-capable executor, non-streaming Gemini envelope | sync | existing Antigravity executor | inline image data → `b64_json` | `image_gen`/`imageConfig`; OAuth, project ID и refresh остаются в provider executor |

`runwayml` принимает только regex `^gen4_image[A-Za-z0-9._-]{0,64}$`. Ни одна video model не маршрутизируется через images endpoint.

Cloudflare multipart models не принимают edit inputs, потому что legacy multipart contract не передаёт image/mask. JSON путь поддерживает только перечисленные Cloudflare optional fields и безопасно валидированные image/mask inputs.

## Ошибки и observability

- Submit `4xx/5xx` сохраняет upstream status code и распарсенное upstream message.
- Poll `4xx/5xx`, terminal failed/cancelled state, unexpected poll host и malformed response возвращают `502` с provider-prefixed diagnostic.
- Deadline expiration возвращает `504`.
- Request context cancellation не превращается в ложный success; handler не пишет `200`, если context завершён до финальной нормализации.
- URL binary-download failure возвращает `502`, не `501`.
- Логи lifecycle calls содержат только provider ID, model, phase (`submit`, `poll`, `result-download`) и status. Prompt, API key, access token, image base64 и полный signed URL запрещены.
- Для URL в log используется только `scheme://host/path`: query и fragment всегда удаляются. В частности это относится к status/poll/result URLs.

## Тестирование

Без mock HTTP clients. Каждый adapter тестируется с реальным `httptest.Server`, который проверяет method, path, headers и body, затем моделирует response sequence.

Обязательные сценарии:

1. Каждый provider из matrix имеет `httptest.Server` contract test для submit request: URL, method, legacy headers, payload и нормализация.
2. `fal-ai`, BFL, RunwayML и nanobanana проходят pending → completed → result response; poll URL может отличаться от submit URL, но сохраняет origin transport metadata.
3. Terminal failure, polling non-2xx, malformed body, unexpected poll host, timeout и cancellation возвращают заявленные ошибки без retry после terminal state.
4. Sync JSON и raw-binary adapters нормализуются в OpenAI response; HuggingFace и Cloudflare отклоняют non-image raw bytes. SDWebUI decision table фиксирует absent `n`, `n:0`, `badx512`, `512xbad` и `0x0` ровно как legacy: absent `n` → `1`, explicit `0` → `0`; invalid/zero dimensions → `512`.
5. Binary response корректно декодирует `b64_json` и скачивает URL через тот же executor; download failure — `502`.
6. HTTP-level `/v1/images/generations` с реальным `imageproxy.Handler` разрешает provider/credentials, выдаёт `x-9gouter-connection-id` для connection-backed provider и проходит production executor wiring. Endpoint принимает `x-9gouter-connection-id` для preferred connection pinning так же, как chat endpoint; no-auth providers намеренно не выдают этот header.
7. Integration test executor доказывает, что submit, poll и URL download connection-backed provider проходят через одну proxy-aware boundary и наследуют submit connection proxy options. Отдельный proxy integration test с recording resolver/dial/CONNECT seam доказывает DNS re-resolution regression: direct, HTTP CONNECT и SOCKS получают validated IP:port, сохраняют original TLS SNI/Host и не используют relay/fallback для untrusted URL.
8. No-auth tests доказывают: sdwebui/comfyui работают без connection только от loopback viewer, получают `403` для external viewer, используют literal loopback target без DNS и не следуют redirect.
9. RunwayML test доказывает `400` для non-`gen4_image*` и supplied image fields; Stability AI tests покрывают `core`, `ultra` и `sd3`; Cloudflare tests покрывают JSON и multipart routes, exact key/type/precedence mappings, `seed:0`, mask aliases/conflict и reject edit input для multipart.
10. Antigravity provider test фиксирует image-specific envelope, `requestType:image_gen`, `imageConfig`, non-stream action, Bearer auth и extraction inline image; app wiring test подтверждает, что imageproxy использует именно этот executor.
11. Credentialed poll/result tests отклоняют allowlisted HTTP URLs и HTTPS→HTTP redirect; смена origin не переносит auth header.

Финальная проверка: `gofmt` затронутых Go файлов и `go test -race ./...`.

## Риски и решения

| Риск | Решение |
|---|---|
| Poll/result URL на ином host | Не строить URL заново; использовать origin connection executor и provider-specific host allowlist. |
| Credentials/proxy routing попадут в provider adapters | Adapter передаёт request с internal metadata; repositories и `proxy` остаются только в composition-root executor. |
| No-auth local provider станет public SSRF pivot | Пропускать sdwebui/comfyui только для loopback viewer и loopback base URL; direct-only transport. |
| User-supplied image URL вызовет SSRF | HTTPS-only guard, private-address denial до и после redirect, size cap и executor egress policy. |
| Cloudflare смешивает JSON и multipart | Multipart только для трёх legacy FLUX.2 models; edit inputs там явно отклоняются. |
| ComfyUI не имеет полноценного graph contract | Не расширять legacy `{prompt}` passthrough и не заявлять graph support. |
| Abstraction polling скроет provider semantics | Helper управляет только deadline/sleep/context; headers, host trust, status/parser остаются локальными. |
| Legacy JS будет удалён, а criterion останется не проверяемым | Матрица и contract tests выше являются замороженным Go parity contract: URL, headers, payload, parser, status mapping и terminal states. |

## Acceptance criteria

- Ни один provider из matrix не возвращает `501 image transport not implemented in Go build`.
- Каждый provider проходит свой зафиксированный matrix HTTP contract и соответствующий `httptest.Server` test.
- Все connection-backed external image lifecycle requests используют один connection-aware proxy executor; Antigravity использует свой существующий connection-aware provider executor; sdwebui/comfyui — только явно определённый local direct-only transport.
- Poll/result/download requests наследуют proxy policy origin submit connection и проходят provider host validation.
- Unsupported provider/model fields отклоняются явно, без silent drop; user-supplied image input соблюдает SSRF policy.
- Тесты основаны на реальных `httptest.Server`; transport responses не мокаются.
- `go test -race ./...` проходит.
