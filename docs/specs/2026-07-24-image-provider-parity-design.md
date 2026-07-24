# Паритет image-provider adapters — дизайн

**Дата:** 2026-07-24  
**Статус:** Одобрен  
**Область:** Go image-generation pipeline (`/v1/images/generations`)

## Цель

Убрать честные `501` из Go-пайплайна генерации изображений для всех адаптеров, которые уже поддержаны legacy JS: `sdwebui`, `comfyui`, `huggingface`, `fal-ai`, `black-forest-labs`, `stability-ai`, `runwayml`, `cloudflare-ai`, `nanobanana` и `antigravity`.

Каждый адаптер должен сохранить legacy HTTP-контракт, проходить через connection-aware proxy routing и нормализовать успешный результат в существующий OpenAI-совместимый ответ `/v1/images/generations`.

## Не входит в scope

- Новый публичный endpoint или изменение существующей OpenAI-совместимой формы ответа.
- Video generation: у RunwayML в image pipeline поддерживаются только модели `gen4_image*`; video-модели остаются на `/v1/videos/*`.
- Полный ComfyUI graph-workflow: legacy-контракт сам является минимальным `{prompt}` passthrough.
- Combo fallback, account fallback, реактивный OAuth refresh и usage persistence для images: это отдельные уже известные gaps, не условие снятия transport `501`.

## Контракт входа и выхода

Вход сохраняет существующие поля `/v1/images/generations`: `model`, `prompt`, `n`, `size`, `quality`, `style`, `response_format`, `output_format`, `background`.

Для legacy image-edit adapters транспортный слой дополнительно сохраняет и пробрасывает необязательные поля без изменения контракта базового endpoint:

- `image`, `images`, `mask_image`, `maskImage`, `mask`;
- `width`, `height`;
- Cloudflare optional fields `negative_prompt`, `guidance`, `seed`, `num_steps`, `steps`, `strength`.

Успех для `response_format=url` или `b64_json` возвращается в форме:

```json
{"created": 0, "data": [{"url": "..."}]}
```

или:

```json
{"created": 0, "data": [{"b64_json": "..."}]}
```

`response_format=binary` отдаёт raw bytes первой картинки. При URL-result image download выполняется через тот же proxy-aware запрос, что submit и polling.

## Архитектура

### Transport boundary

`imageproxy` получает минимальный инъецируемый HTTP executor. Он принимает `*http.Request` и возвращает `*http.Response`; usecase не импортирует repositories, `proxy` package или HTTP transport.

Composition root создаёт executor, который:

1. извлекает `_connectionId` из `Credentials.ProviderSpecificData`;
2. загружает соответствующий `ProviderConnection` из `ConnectionRepo`;
3. выполняет запрос через `proxy.ProxyAwareFetch`, `ProxyPoolRepo`, `ProxyOpts` и connection-level proxy settings;
4. при недоступной connection возвращает ясную ошибку, не обходя назначенный маршрутизатор прямым запросом.

Все внешние image calls — submit, poll, result fetch и URL-to-binary download — используют этот executor. Если production executor не передан, constructor сохраняет стандартный `http.Client` только как безопасный тестовый/default fallback, не как путь production wiring.

### Adapter dispatch

Registry `image.Config` перестаёт маркировать реализованные providers `Unsupported`. `Handler.synthesize` выбирает provider-local build/parse/normalize logic. Общим остаётся только механизм polling; URL, auth headers, request shape и terminal statuses остаются у конкретного provider.

### Polling lifecycle

Общий helper принимает submit-derived poll URL и provider-local callback распознавания состояния. Он реализует legacy defaults:

- interval: `1500 ms`;
- overall timeout: `120 s`;
- завершение немедленно на `context` cancellation/deadline;
- no retry после terminal failure;
- provider-specific error при upstream non-2xx, malformed response, failed status или timeout.

Тестовая конфигурация позволяет сократить interval/deadline без sleep в 120 секунд. Production defaults остаются parity-значениями.

## Provider matrix

| Provider | Submit | Poll / result | Auth | Normalized output |
|---|---|---|---|---|
| `sdwebui` | `POST /sdapi/v1/txt2img`, `{prompt,width,height,steps:20,batch_size}` | sync | none | `images[]` → `b64_json` |
| `comfyui` | `POST` configured base, `{prompt}` | sync | none | legacy passthrough OpenAI-shaped body |
| `huggingface` | `POST /models/{model}`, `{inputs:prompt}` | sync raw binary | Bearer | bytes → `b64_json` |
| `fal-ai` | `POST /{model}`, `{prompt,num_images,image_size,image_url}` | `status_url`, then `response_url` | `Authorization: Key` | `images[]` / `image` → URL list |
| `black-forest-labs` | `POST /v1/{model}`, `{prompt,width,height,image_prompt}` | `polling_url` | `x-key` | `result.sample` → URL |
| `stability-ai` | `POST /{core|ultra|sd3}` | sync | Bearer | `image` → `b64_json` |
| `runwayml` | `POST /text_to_image` for `gen4_image*` | `/tasks/{id}` | Bearer + `X-Runway-Version: 2024-11-06` | `output[]` → URL list |
| `cloudflare-ai` | `/accounts/{accountId}/ai/run/{model}` | sync JSON or raw image | Bearer | JSON/raw image normalized to OpenAI |
| `nanobanana` | `/generate` with dummy callback URL | `record-info?taskId=` | Bearer | `resultImageUrl` / `originImageUrl` → URL |
| `antigravity` | existing provider executor/Gemini envelope | sync | existing Antigravity auth | inline image data → `b64_json` |

## Ошибки и observability

- Submit `4xx/5xx` сохраняет status code и распарсенное upstream message.
- Poll `4xx/5xx`, terminal failed/cancelled state и malformed response возвращают `502` с provider-prefixed diagnostic.
- Deadline expiration возвращает `504`; request context cancellation не превращается в ложный success.
- Логируются provider id, model, lifecycle phase (`submit`, `poll`, `result-download`) и status без prompt, API key, access token, image base64 или signed URL query parameters.

## Тестирование

Без mock HTTP clients. Каждый adapter тестируется с реальным `httptest.Server`, который проверяет method, path, headers и body, а затем моделирует response sequence.

Обязательные сценарии:

1. submit request конкретного provider имеет legacy auth и payload;
2. async providers проходят pending → completed → result response;
3. terminal failure, polling non-2xx и timeout/cancel возвращают ошибку;
4. sync JSON и raw-binary adapters нормализуются в OpenAI response;
5. binary response корректно декодирует `b64_json` и proxy-aware скачивает URL;
6. HTTP-level `/v1/images/generations` разрешает provider/credentials, выдаёт `x-9gouter-connection-id` и проходит реальный wiring executor;
7. integration test executor доказывает, что submit, poll и URL download используют одну proxy-aware boundary.

Финальная проверка: `gofmt` затронутых Go файлов и `go test -race ./...`.

## Риски и решения

| Риск | Решение |
|---|---|
| Poll URL может быть на ином host | Не строить URL заново; выполнять upstream URL через тот же injected executor. |
| Cloudflare смешивает JSON и multipart | Multipart только для трёх legacy FLUX.2 models; остальные модели используют JSON. |
| ComfyUI не имеет полноценного graph contract | Не расширять за legacy `{prompt}` passthrough и не заявлять graph support. |
| Proxy routing потечёт в provider adapters | Оставить proxy/database знания только в composition-root executor. |
| Абстракция polling скроет provider semantics | Helper управляет только deadline/sleep/context; status/parser остаются локальными. |

## Acceptance criteria

- Ни один provider из matrix не возвращает исходный `501 image transport not implemented in Go build`.
- Каждый provider сохраняет legacy request/auth/parse semantics.
- Все external image lifecycle requests используют одну connection-aware proxy boundary.
- Тесты основаны на реальных `httptest.Server`; transport responses не мокаются.
- `go test -race ./...` проходит.
