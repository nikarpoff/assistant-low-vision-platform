# assistant-low-vision-platform

Шлюз системы голосового описания сцены для людей с нарушениями зрения: принимает снимки и речь от мобильного приложения, вызывает сервисы ИИ-моделей и языковую модель и возвращает ответ речью.

## Назначение

Шлюз - единственная точка входа мобильного приложения (шаблон API Gateway):
- держит с приложением WebSocket-сессию по протоколу `alv.v1`: снимки и аудио идут бинарными кадрами, команды и ответы - JSON-сообщениями;
- вызывает сервисы [objects-detector](https://github.com/nikarpoff/objects-detector) (обнаружение объектов), [asr-service](https://github.com/nikarpoff/asr-service) (распознавание речи) и [tts-service](https://github.com/nikarpoff/tts-service) (синтез речи);
- строит описание сцены по шаблону (`template`), языковой моделью по тексту (`llm`) или мультимодальной моделью по снимку (`vlm`). Языковая модель - облачный поставщик по OpenAI-совместимому API: Polza.ai напрямую или GigaChat через прокси gpt2giga;
- в пассивном режиме предупреждает об опасностях по правилам, без языковой модели;
- пишет журнал запросов и замеры этапов в PostgreSQL. Снимки и аудио не сохраняются никогда;
- отдаёт метрики Prometheus, статистику по журналу и снимок конфигурации стенда.

В репозитории также лежат развёртывание всей системы (`deployments/`), эталонные контракты сервисов (`testdata/contracts/`) и инструменты эксперимента. Требования - в [docs/spec.md](docs/spec.md).

Состояние: версия 0.1.0 в разработке. Готовы каркас (настройки, логгер, метрики, HTTP-сервер с `/health`, образ и CI), журнал в PostgreSQL и статистика `/v1/stats` с проверкой админ-токена. Журнал и `/v1/stats` подключаются к конвейерам и маршрутам при сборке Шлюза; протокол, конвейеры и остальные эндпоинты появятся следующими изменениями.

## Стек технологий

- **Язык**: Go 1.24
- **HTTP**: gorilla/mux
- **Конфигурация**: переменные окружения (caarlos0/env)
- **Логирование**: собственный printf-логгер `internal/logger`
- **Метрики**: prometheus/client_golang
- **Журнал**: PostgreSQL, драйвер jackc/pgx v5 (`pgxpool`)
- **Валидация**: go-playground/validator
- **Тесты**: testify (require), moq
- **Образ**: distroless (`gcr.io/distroless/static-debian12:nonroot`), публикуется в `ghcr.io/nikarpoff/assistant-low-vision-platform`

## Быстрый старт

Без токенов и без языковой модели (режим `template`):

```bash
docker build -f build/Dockerfile -t alv-platform:dev .
docker run --rm -p 127.0.0.1:8080:80 \
  -e APP_AUTH_DISABLED=true -e APP_DESCRIBER_MODE=template \
  alv-platform:dev
curl http://127.0.0.1:8080/health
```

Без Docker:

```bash
APP_SERVER_PORT=8080 APP_AUTH_DISABLED=true APP_DESCRIBER_MODE=template go run ./cmd/gateway
```

## <a id="env"></a> Переменные окружения

Все настройки - переменные окружения с рабочими значениями по умолчанию. Длительности с суффиксом `_MS` - в миллисекундах, с `_S` и `_TIMEOUT` - в секундах.

Шлюз не стартует и пишет понятную ошибку, если:
- `APP_DESCRIBER_MODE` не `template`, `llm` или `vlm`;
- в режиме `llm` пуст `APP_LLM_MODEL`, в режиме `vlm` пусты и `APP_VLM_MODEL`, и `APP_LLM_MODEL`: идентификатор модели входит в `config_id`, поэтому значения по умолчанию у него нет;
- в режимах `llm` и `vlm` `APP_LLM_URL` - не абсолютный адрес со схемой `http` или `https`;
- `APP_AUTH_DISABLED` не `true`, а `APP_DEVICE_TOKENS` или `APP_ADMIN_TOKEN` пусты;
- `APP_LOG_LEVEL` - не уровень логирования;
- значение числовой или логической переменной не разбирается.

Пустой `APP_LLM_API_KEY` допустим (заглушка поставщика), но в режимах `llm` и `vlm` при старте в лог пишется предупреждение. В режиме `template` настройки языковой модели не проверяются и не используются.

### Сервер и логи

| Переменная | Описание | По умолчанию | Обязательная |
|---|---|---|---|
| `APP_SERVER_PORT` | Порт HTTP и WebSocket | `80` | Нет |
| `APP_SERVER_READ_TIMEOUT` | Таймаут чтения HTTP-запроса (сек) | `15` | Нет |
| `APP_SERVER_WRITE_TIMEOUT` | Таймаут записи HTTP-ответа (сек) | `15` | Нет |
| `APP_SERVER_IDLE_TIMEOUT` | Таймаут простоя keep-alive соединения (сек) | `60` | Нет |
| `APP_SERVER_SHUTDOWN_TIMEOUT` | Сколько ждать активные запросы при остановке (сек) | `5` | Нет |
| `APP_LOG_LEVEL` | Уровень логов: `DEBUG`, `INFO`, `WARNING`, `ERROR`. Тексты вопросов и ответов пишутся только на `DEBUG` | `INFO` | Нет |

### Аутентификация

| Переменная | Описание | По умолчанию | Обязательная |
|---|---|---|---|
| `APP_AUTH_DISABLED` | `true` - работа без токенов, только для разработки | `false` | Нет |
| `APP_DEVICE_TOKENS` | Токены устройств через запятую | - | Да, если `APP_AUTH_DISABLED` не `true` |
| `APP_ADMIN_TOKEN` | Токен `/v1/stats` и `/v1/config` | - | Да, если `APP_AUTH_DISABLED` не `true` |

### Описание сцены

| Переменная | Описание | По умолчанию | Обязательная |
|---|---|---|---|
| `APP_DESCRIBER_MODE` | Режим описания: `template`, `llm`, `vlm` | `llm` | Нет |
| `APP_PLACEMENT` | Метка варианта размещения (`A`, `B`), входит в `config_id` | `dev` | Нет |
| `APP_PROMPT` | Имя шаблона промпта | `default` | Нет |
| `APP_MAX_ANSWER_CHARS` | Предельная длина ответа (символов) | `220` | Нет |
| `APP_VLM_WITH_DETECTIONS` | В режиме `vlm` добавлять объекты детектора в промпт как подсказки | `true` | Нет |
| `APP_SCENE_FRESHNESS_MS` | Сколько последняя сцена устройства годится для вопроса без снимка | `10000` | Нет |

### Языковая модель

| Переменная | Описание | По умолчанию | Обязательная |
|---|---|---|---|
| `APP_LLM_URL` | Базовый адрес OpenAI-совместимого API поставщика; к нему добавляются `/chat/completions` и `/models` | `https://polza.ai/api/v1` | Нет |
| `APP_LLM_API_KEY` | Ключ поставщика (секрет): ключ Polza.ai или ключ доступа к gpt2giga | пусто | Нет |
| `APP_LLM_MODEL` | Идентификатор модели для режима `llm` | пусто | Да, в режимах `llm` и `vlm` (если пуст `APP_VLM_MODEL`) |
| `APP_VLM_MODEL` | Идентификатор мультимодальной модели для режима `vlm`; пусто - `APP_LLM_MODEL` | пусто | Нет |
| `APP_LLM_TEMPERATURE` | Температура генерации | `0` | Нет |
| `APP_LLM_SEED` | Seed генерации | `42` | Нет |
| `APP_LLM_MAX_TOKENS` | Предел токенов ответа | `96` | Нет |
| `APP_LLM_EXTRA_BODY` | JSON-объект, добавляемый в тело запроса (например, выключение рассуждений). Поля `model`, `messages`, `stream`, `temperature`, `seed`, `max_tokens` и `user` в нём запрещены: их задаёт клиент или они не передаются поставщику | `{}` | Нет |

Клиент языковой модели (`internal/llm`) один для любого поставщика:
- шлёт `POST <APP_LLM_URL>/chat/completions` с `stream: true`, моделью, сообщениями промпта, `temperature`, `seed`, `max_tokens` и полями `APP_LLM_EXTRA_BODY`;
- из своих заголовков ставит только `Authorization: Bearer <APP_LLM_API_KEY>` (при пустом ключе не ставит), `Content-Type: application/json` и `Accept: text/event-stream`. `X-Request-ID`, `device_id`, `label` и поле `user` поставщику не передаются;
- читает SSE построчно: комментарии и пустые строки пропускает, поле `error` проверяет в каждом фрагменте, `model` и `system_fingerprint` берёт из первого фрагмента, где они есть;
- держит пул keep-alive соединений, проверяет TLS-сертификат по системным корневым сертификатам, не сжимает ответ, не следует перенаправлениям и не повторяет запрос;
- первый токен ждёт не дольше `APP_LLM_TTFT_TIMEOUT_MS`, всю генерацию - не дольше `APP_LLM_TOTAL_TIMEOUT_MS`.

Вид отказа (`reason`) пишется в журнал (`llm_error`) и в метрику `alv_upstream_errors_total{upstream="llm", reason}`:

| `reason` | Что случилось |
|---|---|
| `insufficient_balance` | HTTP 402 до потока или событие ошибки в потоке с `code` `INSUFFICIENT_BALANCE`, `402` (число или строка) или `PAYMENT_REQUIRED` |
| `http_429` | HTTP 429 до потока |
| `http_5xx` | HTTP 500-599 до потока |
| `http_4xx` | другой не-2xx статус до потока (400, 401, 403, 404, 408, 413, перенаправление) |
| `stream_error` | HTTP 200 и фрагмент с полем `error` или `finish_reason: "error"`, даже если за ним пришёл `data: [DONE]` |
| `disconnect` | соединение не установлено, закрылось или поток не прочитался до `[DONE]` и до фрагмента с `finish_reason` |
| `empty` | нормальный конец без текста (или только с пробелами) |
| `timeout_ttft` / `timeout_total` | нет первого токена за `APP_LLM_TTFT_TIMEOUT_MS` / генерация дольше `APP_LLM_TOTAL_TIMEOUT_MS` |

В лог при отказе пишутся `request_id`, `reason`, HTTP-статус, `code` и `trace_id` поставщика, а при сетевом сбое - ошибка соединения. Текст ошибки поставщика не пишется никогда.

### Сервисы ИИ-моделей

| Переменная | Описание | По умолчанию | Обязательная |
|---|---|---|---|
| `APP_DETECTOR_URL` | Адрес детектора | `http://detector:8000` | Нет |
| `APP_DETECTOR_TOKEN` | Токен детектора (секрет) | пусто | Нет |
| `APP_ASR_URL` | Адрес ASR | `http://asr:8000` | Нет |
| `APP_ASR_TOKEN` | Токен ASR (секрет) | пусто | Нет |
| `APP_TTS_URL` | Адрес TTS | `http://tts:8000` | Нет |
| `APP_TTS_TOKEN` | Токен TTS (секрет) | пусто | Нет |
| `APP_TTS_VOICE` | Голос TTS; пусто - голос сервиса по умолчанию | пусто | Нет |

### Таймауты

| Переменная | Описание | По умолчанию | Обязательная |
|---|---|---|---|
| `APP_DETECT_TIMEOUT_MS` | Вызов детектора | `400` | Нет |
| `APP_ASR_TIMEOUT_MS` | Вызов ASR | `2000` | Нет |
| `APP_LLM_TTFT_TIMEOUT_MS` | Первый токен языковой модели | `900` | Нет |
| `APP_LLM_TOTAL_TIMEOUT_MS` | Вся генерация | `5000` | Нет |
| `APP_TTS_FIRST_CHUNK_TIMEOUT_MS` | Первый аудиофрагмент предложения | `1000` | Нет |
| `APP_REQUEST_DEADLINE_MS` | Весь пользовательский запрос | `8000` | Нет |
| `APP_PASSIVE_DEADLINE_MS` | Пассивный запрос; дальше результат устарел | `1500` | Нет |

### Пассивный режим, опасности, намерения, история

| Переменная | Описание | По умолчанию | Обязательная |
|---|---|---|---|
| `APP_PASSIVE_NOTICES` | Озвучивать в пассивном режиме и уровень `notice` | `false` | Нет |
| `APP_HAZARD_RULES_PATH` | YAML с правилами опасности; пусто - встроенные правила | пусто | Нет |
| `APP_HAZARD_COOLDOWN_MS` | Как часто может повторяться одно предупреждение по паре «класс, направление» | `10000` | Нет |
| `APP_INTENTS_PATH` | YAML с фразами намерений; пусто - встроенные фразы | пусто | Нет |
| `APP_HISTORY_SCENES` | Сколько последних сцен устройства хранить | `5` | Нет |
| `APP_HISTORY_TURNS` | Сколько последних реплик устройства хранить | `6` | Нет |
| `APP_HISTORY_TTL_S` | Через сколько секунд без активности устройства история очищается | `600` | Нет |

### Параметры приложения

Шлюз передаёт их приложению в `welcome`.

| Переменная | Описание | По умолчанию | Обязательная |
|---|---|---|---|
| `APP_IMAGE_MAX_SIDE` | Длинная сторона снимка (пикселей) | `640` | Нет |
| `APP_JPEG_QUALITY` | Качество JPEG | `80` | Нет |
| `APP_PASSIVE_INTERVAL_MS` | Интервал снимков пассивного режима | `3000` | Нет |
| `APP_LISTEN_WINDOW_MS` | Окно ожидания речи | `5000` | Нет |
| `APP_MAX_QUESTION_MS` | Наибольшая длительность вопроса | `15000` | Нет |
| `APP_AUDIO_OUT_SAMPLE_RATE` | Частота аудио ответа (Гц) | `24000` | Нет |
| `APP_MAX_IMAGE_BYTES` | Наибольший размер снимка (байт) | `2097152` | Нет |

### Журнал и конфигурация стенда

| Переменная | Описание | По умолчанию | Обязательная |
|---|---|---|---|
| `APP_POSTGRES_DSN` | Строка подключения к PostgreSQL, например `postgres://alv:<пароль>@postgres:5432/alv?sslmode=disable` (секрет: содержит пароль); пусто - журнал выключен, при старте в лог пишется предупреждение | пусто | Нет |
| `APP_JOURNAL_BUFFER` | Ёмкость буфера записей журнала; при переполнении записи отбрасываются | `1024` | Нет |
| `APP_CONFIG_REFRESH_S` | Период обновления снимка конфигурации (сек) | `60` | Нет |

## Протокол `alv.v1`

Приложение держит одно WebSocket-соединение `wss://<хост>/v1/session` с подпротоколом `alv.v1` и токеном устройства в `Authorization: Bearer <токен>`. Текстовые кадры - JSON-сообщения с полем `type` (от приложения - `hello`, `describe`, `ask.start`, `ask.end`, `cancel`, `client.metrics`; от Шлюза - `welcome`, `accepted`, `transcript`, `answer.start`, `answer.text`, `answer.end`, `error` и другие). Бинарные кадры - заголовок 22 байта (версия, вид, `request_id`, `seq`) и полезная нагрузка: JPEG снимка, PCM вопроса или PCM ответа.

Полное описание - раздел PROTO в [docs/spec.md](docs/spec.md). Эталонные вектора кадров и сообщений, общие с приложением, - `testdata/protocol/vectors.json`.

## HTTP API

| Метод | Путь | Описание | Готово |
|-------|------|----------|---|
| GET | `/health` | Проверка живости, ответ `200` и текст `OK` | да |
| GET | `/v1/session` | WebSocket-сессия приложения (протокол `alv.v1`) | нет |
| GET | `/ready` | Готовность: статусы детектора, ASR, TTS, языковой модели, журнала и словаря; `503`, если обязательный сервис недоступен | нет |
| GET | `/metrics` | Метрики Prometheus | нет |
| GET | `/v1/stats` | Статистика по журналу: p50 и p95 этапов, доли деградации и откатов; админ-токен | обработчик готов, маршрут подключается при сборке Шлюза |
| GET | `/v1/config` | Снимок конфигурации стенда и `config_id`; админ-токен | нет |

Подробности - PLT-13 в [docs/spec.md](docs/spec.md). Ошибка `500` всегда обезличена: `{"error": "internal server error"}`, подробности только в логе.

### GET /v1/stats

Статистика по журналу для отчёта по конфигурации (PLT-13, EXP-5, EXP-8). Нужен заголовок `Authorization: Bearer <APP_ADMIN_TOKEN>`; без него или с другим токеном - `401` и `{"error": "unauthorized"}`. При `APP_AUTH_DISABLED=true` токен не проверяется.

Параметры запроса, все необязательные; пустой параметр не фильтрует:

| Параметр | Описание |
|---|---|
| `since` | Начало интервала по времени создания запроса, RFC 3339 (`2026-11-01T10:00:00Z`), включительно |
| `until` | Конец интервала, RFC 3339, не включительно; должен быть позже `since` |
| `kind` | Вид запроса: `describe` (по касанию), `ask`, `passive` |
| `config_id` | Конфигурация стенда |

```bash
curl -H "Authorization: Bearer $APP_ADMIN_TOKEN" \
  "http://127.0.0.1:8080/v1/stats?config_id=c9f2a1d0e3b4&since=2026-11-01T10:00:00Z&until=2026-11-01T12:00:00Z"
```

```json
{"config_id":"c9f2a1d0e3b4","since":"2026-11-01T10:00:00Z","until":"2026-11-01T12:00:00Z","kind":null,"stages":{"detect_ms":{"n":300,"p50":38.1,"p95":61.0,"mean":40.2}},"e2e":{"describe":{"n":300,"p50":1310.0,"p95":1820.0,"mean":1350.4}},"degraded":{"llm_timeout":{"n":9,"share":0.03},"llm_unavailable":{"n":3,"share":0.01},"tts_unavailable":{"n":0,"share":0.0}},"llm_errors":{"timeout_ttft":9,"http_429":2,"insufficient_balance":1},"llm_fallback_share":0.04,"llm_models":{"fake/fake-llm":312}}
```

Форма ответа - контракт для инструментов эксперимента, её закрепляет копия примера `testdata/stats/stats.response.json`:
- `config_id`, `since`, `until`, `kind` - применённые фильтры, `null` - фильтра нет; время - в UTC;
- `stages` - для каждого этапа PLT-14 из журнала: число замеров `n`, `p50`, `p95` и среднее `mean` в мс;
- `e2e` - то же по `trigger_to_first_audio_ms` из `client.metrics`, по видам запроса;
- `degraded` - для каждого `degraded_reason` (`llm_timeout`, `llm_unavailable`, `tts_unavailable`, ключи есть всегда) число ответов и их доля от всех ответов выборки. Ответ - запрос, закончившийся `answer.end`, то есть со статусом `ok` или `degraded`;
- `llm_errors` - число ответов по каждому виду отказа поставщика `llm_error` (PLT-15);
- `llm_fallback_share` - доля откатов: ответы с `degraded_reason` `llm_timeout` или `llm_unavailable`, делённые на все ответы описания сцены в режимах `llm` и `vlm` (`describe` по касанию и `ask` с намерением `describe`, `describe_ahead` или `question`); `null`, если таких ответов нет;
- `llm_models` - число запросов по каждому значению `model` из ответов поставщика.

Перцентили считаются методом nearest-rank: `x[ceil(q·n)-1]` по отсортированной выборке; `p50`, `p95` и `mean` округлены до 0,1 мс. Долю откатов считает функция `stats.FallbackShare` - та же, что у `loadgen` и `score`.

Коды ответа: `200`; `401` - нет админ-токена; `422` - неверный параметр (`{"error": "validation failed for fields: since"}`) или `until` не позже `since`; `503` - журнал выключен (`APP_POSTGRES_DSN` пуст); `500` - обезличенная внутренняя ошибка.

### Метрики

Все метрики объявлены в `internal/metrics` в собственном реестре:

| Метрика | Тип | Метки |
|---|---|---|
| `alv_stage_duration_seconds` | гистограмма | `stage`, `kind` |
| `alv_e2e_duration_seconds` | гистограмма | `kind`, `network` |
| `alv_requests_total` | счётчик | `kind`, `status` |
| `alv_upstream_errors_total` | счётчик | `upstream`, `reason` |
| `alv_sessions_active` | показатель | - |
| `alv_journal_dropped_total` | счётчик | - |

## Сервисы ИИ-моделей

Клиенты детектора, ASR и TTS лежат в `internal/upstream/{detector,asr,tts}` (PLT-15 в [docs/spec.md](docs/spec.md)):
- у каждого сервиса свой пул keep-alive соединений, без прокси и без сжатия;
- тела бинарные, без base64:
  - снимок детектору - `image/jpeg` как есть;
  - вопрос в ASR - `audio/L16; rate=16000; channels=1`;
  - в TTS - JSON TTS-1 (`text`, `voice`, если задан `APP_TTS_VOICE`, `sample_rate` из `APP_AUDIO_OUT_SAMPLE_RATE`, `format: pcm_s16le`, `speed: 1.0`), ответ - поток `audio/L16`;
- заголовки: `Authorization: Bearer <APP_*_TOKEN>` (при пустом токене не ставится) и `X-Request-ID` - UUID запроса приложения;
- таймауты PLT-8:
  - детектор - `APP_DETECT_TIMEOUT_MS`, ASR - `APP_ASR_TIMEOUT_MS` на весь вызов;
  - TTS - `APP_TTS_FIRST_CHUNK_TIMEOUT_MS` до первого аудиофрагмента. Клиент возвращает поток, как только пришёл первый фрагмент, остальное ограничено только дедлайном запроса. Фрагменты содержат целые отсчёты (чётное число байт);
  - `GET /v1/info` и `GET /v1/vocabulary` - 2 с;
- повторов нет. Каждый отказ - типизированная ошибка и `+1` к `alv_upstream_errors_total{upstream, reason}`, где `upstream` - `detector`, `asr` или `tts`:

| Что случилось | Ошибка | `reason` |
|---|---|---|
| ответ 503 (очередь сервиса заполнена) | `ErrBusy` | `busy` |
| другой ответ 5xx, нет соединения, поток оборвался | `ErrUnavailable` | `unavailable` |
| вызов или первый фрагмент TTS не уложился в таймаут | `ErrTimeout` | `timeout` |
| ответ 401 (неверный токен сервиса) | `ErrUnauthorized` | `unauthorized` |
| другой не-2xx ответ (400, 413, 415, 422) | `ErrRejected` | `rejected` |
| ответ 2xx с другим `Content-Type`, битым JSON или пустым потоком | `ErrInvalidResponse` | `invalid_response` |

Вызов, прерванный самим Шлюзом (отмена запроса или его дедлайн), отказом сервиса не считается и в счётчик не попадает. Поле `detail` ответа сервиса попадает в текст ошибки.

### Словарь детектора

`internal/vocabulary` (PLT-16, DET-2) загружает словарь при старте запросом `GET /v1/vocabulary`. Пока словарь не загружен, попытки повторяются с паузой, которая удваивается от 0,5 до 10 с; каждая неудача пишется в лог предупреждением. Словарь проверяется: уникальные `id` в snake_case, формы `one`, `few`, `many`, род `m`, `f`, `n` или `pl`, компилируемые шаблоны `mentions`.

Словарь даёт:
- форму названия для количества по правилам русского языка: 1 «стул», 2 «стула», 5 и 11 «стульев», 21 «стул»;
- классы для проверки правил опасности;
- сопоставитель упоминаний. Шаблоны `mentions` применяются к тексту, нормализованному по PLT-4, и оборачиваются в `(?:^| )(?:<шаблон>)(?: |$)`: RE2 не знает `\b` для кириллицы, поэтому границы слов - пробелы и края текста.

## Конфигурация стенда и `config_id`

`internal/snapshot` (PLT-12) собирает снимок конфигурации при старте и раз в `APP_CONFIG_REFRESH_S`. `config_id` - первые 12 hex-символов SHA-256 от канонического JSON снимка: ключи отсортированы, без пробелов, числа в исходной записи. Снимок отдаёт `GET /v1/config` в виде `{"config_id", "collected_at", "config"}`.

```json
{
  "placement": "B",
  "describer_mode": "llm",
  "services": {"detector": <GET /v1/info>, "asr": <GET /v1/info>, "tts": <GET /v1/info>},
  "llm": {"url": "https://polza.ai/api/v1", "model": "<APP_LLM_MODEL>"},
  "prompt": {"name": "default", "hash": "<хеш шаблона промпта>"},
  "generation": {"temperature": 0, "seed": 42, "max_tokens": 96, "extra_body": {}},
  "timeouts_ms": {"detect": 400, "asr": 2000, "llm_ttft": 900, "llm_total": 5000, "tts_first_chunk": 1000, "request_deadline": 8000, "passive_deadline": 1500},
  "hazard_rules_hash": "<хеш правил опасности>",
  "intents_hash": "<хеш фраз намерений>",
  "tts_voice": "",
  "image": {"max_side": 640, "jpeg_quality": 80}
}
```

- `placement` - метка варианта размещения `APP_PLACEMENT`, всегда на верхнем уровне: по ней сравниваются снимки вариантов A и B.
- `llm` - только в режимах `llm` и `vlm`. `url` - схема, хост и путь `APP_LLM_URL` без логина, пароля, параметров запроса и завершающего `/`. `model` - `APP_LLM_MODEL`, а в режиме `vlm` - `APP_VLM_MODEL`, если он задан. Список `/models` поставщика и модель из его ответа в снимок не входят.
- `extra_body` - разобранный `APP_LLM_EXTRA_BODY`: порядок ключей и пробелы на `config_id` не влияют. Не JSON-объект - ошибка старта.
- Хеши промпта, правил опасности и фраз намерений передают пакеты, которые загружают эти файлы.
- Если сервис не ответил на `/v1/info`, в снимке остаётся его последний известный ответ, поэтому короткий сбой не меняет `config_id`. До первого ответа на месте сервиса стоит `null`.
- Секретов в снимке нет: ни токенов сервисов, устройств и админа, ни `APP_LLM_API_KEY`, ни `APP_POSTGRES_DSN`.

## Схема базы данных

Журнал запросов хранится в PostgreSQL (PLT-11 в [docs/spec.md](docs/spec.md)), колонки именуются в snake_case. Снимки и аудио не сохраняются никогда: запрос хранит только размер полученного снимка.

**Миграции.** SQL-файлы `migrations/NNNN_<имя>.sql` встраиваются в бинарник и применяются при старте журнала по порядку имён. Применённые версии записываются в таблицу `schema_migrations`; файл и запись о его версии выполняются одной неявной транзакцией, поэтому в файле нет `BEGIN` и `COMMIT`. Выпущенная миграция не меняется, изменение схемы - новый файл. Если база при старте недоступна, Шлюз работает дальше, а миграции повторяются перед следующей записью.

**Запись.** Асинхронная: запрос кладёт запись в буфер ёмкостью `APP_JOURNAL_BUFFER` и не ждёт базу. Один писатель забирает записи пачками до 100 штук и пишет пачку одной транзакцией. Записи отбрасываются со счётчиком `alv_journal_dropped_total`, если буфер полон или база не ответила за 5 с. В лог в этих случаях пишется предупреждение не чаще раза в 10 с с числом отброшенных записей. Если база отклонила пачку (например, нарушено ограничение), записи пишутся по одной, и теряются только отклонённые. Повторные записи с тем же ключом игнорируются.

Журнал пишется с потерями, поэтому внешние ключи связывают только строки одной записи: сессию с устройством, этапы - с запросом. Запросы потерянной сессии сохраняются.

| Таблица | Колонки | Описание |
|---|---|---|
| `device` | `device_id` (PK), `first_seen_at`, `last_seen_at` | Устройство по псевдониму - случайному UUID приложения; первое и последнее появление |
| `session` | `session_id` (PK, UUID), `device_id` (FK), `app_version`, `network`, `label`, `started_at`, `ended_at` | WebSocket-сессия; `label` - код участника испытаний (EXP-6), пусто - нет |
| `request` | `request_id` (PK, UUID), `session_id`, `kind`, `describer_mode`, `intent`, `status`, `error_code`, `degraded_reason`, `image_bytes`, `transcript`, `answer_text`, `hazard`, `objects` (JSONB), `config_id`, `llm_model`, `llm_system_fingerprint`, `llm_error`, `created_at` | Запрос: вид (`describe`, `ask`, `passive`), режим описания, итоговый статус (`ok`, `degraded`, `error`, `cancelled`, `skipped`, `stale`, `clear`), поля поставщика языковой модели; индексы по `created_at`, (`config_id`, `created_at`) и `session_id` |
| `stage_timing` | `request_id` (FK), `stage`, `duration_ms` | Длительность этапа PLT-14, мс |
| `client_metric` | `request_id`, `name`, `value` | Метрика из `client.metrics` |
| `config_snapshot` | `config_id` (PK), `config` (JSONB), `collected_at` | Снимок конфигурации PLT-12 без секретов и время, когда он собран впервые |
| `schema_migrations` | `version` (PK), `applied_at` | Применённые миграции |

## Заглушка поставщика `cmd/fakellm`

OpenAI-совместимая заглушка языковой модели для тестов и сквозной проверки без ключей и сети (PLT-19). Адрес для Шлюза - `APP_LLM_URL=http://<хост>:<порт>/v1`.

- `POST /v1/chat/completions` - только `stream: true` (иначе `400 BAD_REQUEST`). Детерминированный текст потоком SSE в формате Polza.ai: комментарий `: PROCESSING`, фрагмент с ролью, после задержки первого токена - фрагменты `chat.completion.chunk` по словам с `model` и `system_fingerprint: fp_fake`, фрагмент с `finish_reason: "stop"`, фрагмент `usage` с пустым `choices`, `data: [DONE]`.
- `GET /v1/models` - список из одной модели.
- Ошибки - в формате Polza.ai: до потока `{"error": {"code": "...", "message": "...", "trace_id": "fake-trace"}, "trace_id": "fake-trace"}`, в потоке - фрагмент с `id` `error_fake`, полем `error`, `trace_id` и `finish_reason: "error"`, за которым идёт `data: [DONE]`.

```bash
go run ./cmd/fakellm --port 8090 --fail stream_error_after_sentence
```

Флаги главнее переменных окружения:

| Переменная | Флаг | Описание | По умолчанию |
|---|---|---|---|
| `FAKELLM_PORT` | `--port` | Порт HTTP | `8000` |
| `FAKELLM_MODEL` | `--model` | Модель во фрагментах и в `/v1/models` | `fake/fake-llm` |
| `FAKELLM_API_KEY` | `--api-key` | Ключ: если задан, запрос без `Authorization: Bearer <ключ>` получает `401 UNAUTHORIZED` | пусто |
| `FAKELLM_FIRST_TOKEN_DELAY_MS` | `--first-token-delay-ms` | Задержка первого токена (мс) | `0` |
| `FAKELLM_TOKEN_DELAY_MS` | `--token-delay-ms` | Задержка между токенами (мс) | `0` |
| `FAKELLM_FAIL` | `--fail` | Режим отказа, см. ниже | `none` |

| `FAKELLM_FAIL` | Поведение |
|---|---|
| `none` | обычный поток |
| `http_402` | до потока: 402, `code` `INSUFFICIENT_BALANCE` |
| `http_429` | до потока: 429, `Retry-After: 1`, `code` `TOO_MANY_REQUESTS` |
| `http_500` | до потока: 500, `code` `INTERNAL_ERROR` |
| `stream_error_before_text` | статус 200, первым фрагментом - событие ошибки `SERVICE_UNAVAILABLE`, затем `[DONE]` |
| `stream_error_after_sentence` | статус 200, первое предложение, событие ошибки `SERVICE_UNAVAILABLE`, `[DONE]` |
| `stream_402_after_sentence` | статус 200, первое предложение, событие ошибки `INSUFFICIENT_BALANCE`, `[DONE]` |
| `disconnect_after_sentence` | статус 200, первое предложение, затем обрыв соединения без `finish_reason` и `[DONE]` |
| `empty` | статус 200, сразу фрагмент с `finish_reason: "stop"` и `[DONE]`, текста нет |

Тесты поднимают заглушку в процессе: `httptest.NewServer(fakellm.NewServer(fakellm.Config{...}))`. `Config.Text` подменяет текст ответа, `Requests()` отдаёт полученные запросы (последние 256) с заголовками и телами.

## Разработка

Подготовка машины - [docs/development.md](docs/development.md): Go 1.24.13 и moq ставит `scripts/setup-dev-env.sh`.

### Запуск тестов

Моки (`*_moq_test.go`) генерируются по директивам `//go:generate moq` и в git не попадают, поэтому на чистом клоне сначала нужны моки:

```bash
./scripts/regenerate-mocks.sh
go test -p 1 ./...
```

### Интеграционные тесты

Репозиторий журнала проверяется на настоящем PostgreSQL. Тесты помечены тегом сборки `integration` и в обычный `go test ./...` не входят. Скрипт генерирует моки, поднимает `postgres:17-alpine` в Docker на свободном порту `127.0.0.1`, прогоняет `TestRepository` и удаляет контейнер при любом исходе:

```bash
./scripts/test-integration.sh
```

Нужны Docker, Go и moq. Образ меняет переменная `ALV_TEST_POSTGRES_IMAGE`. С уже запущенной базой тесты запускаются так (схема `public` базы очищается):

```bash
APP_TEST_POSTGRES_DSN="postgres://postgres:<пароль>@127.0.0.1:5432/alv_test?sslmode=disable" \
  go test -tags integration -v -run '^TestRepository$' ./internal/journal/
```

### Проверки как в CI

```bash
./scripts/regenerate-mocks.sh
test -z "$(gofmt -l .)"
go vet ./...
go test -p 1 ./...
```

CI (`.github/workflows/ci.yml`) делает то же на каждый pull request и на push в `dev`. Workflow `.github/workflows/release.yml` на push в `main` и на теги `v*` прогоняет тесты и публикует образ в `ghcr.io/nikarpoff/assistant-low-vision-platform` с тегами `latest` (только `main`), `sha-<7 символов>` и `v<semver>`.

### Сборка

```bash
go build ./...
docker build -f build/Dockerfile -t alv-platform:test .
```

Образ собирает все бинарники `cmd/*` в корень: `/gateway` - точка входа, остальные инструменты запускаются заменой `entrypoint`.

### Контракты сервисов

Этот репозиторий - источник истины для эталонных примеров сервисов (`testdata/contracts/<сервис>/`) и векторов протокола (`testdata/protocol/vectors.json`). Копии в репозиториях сервисов и приложения вручную не правятся:

```bash
scripts/sync-contracts.sh          # скопировать в соседние репозитории общего проекта
scripts/sync-contracts.sh --check  # сверить копии, ненулевой код при расхождении
```

### Форматирование

```bash
gofmt -w .
goimports -w .
go vet ./...
```

## Структура проекта

```
cmd/gateway/main.go               # точка входа: логгер, конфигурация, HTTP-сервер, остановка по сигналу
cmd/fakellm/main.go               # заглушка поставщика языковой модели для тестов и сквозной проверки
endpoints/httpserver/             # HTTP-сервер и регистрация маршрутов
internal/env/                     # переменные окружения (Vars) и проверки при старте
internal/logger/                  # printf-логгер: строки "<LEVEL>: <message>"
internal/metrics/                 # метрики Prometheus в собственном реестре
internal/protocol/                # протокол alv.v1: бинарные кадры, JSON-сообщения, коды ошибок
internal/upstream/                # общий HTTP-клиент сервисов ИИ-моделей: пул, заголовки, таймауты, типизированные ошибки
internal/upstream/detector/       # клиент детектора: /v1/detect, /v1/vocabulary, /v1/info
internal/upstream/asr/            # клиент ASR: /v1/recognize, /v1/info
internal/upstream/tts/            # клиент TTS: поток /v1/synthesize, /v1/info
internal/vocabulary/              # словарь детектора: загрузка, формы количества, сопоставитель упоминаний
internal/snapshot/                # снимок конфигурации стенда и config_id
internal/llm/                     # клиент языковой модели: SSE, виды отказов, таймауты
internal/fakellm/                 # заглушка поставщика: поток в формате Polza.ai, режимы отказа, запись запросов
internal/utils/                   # валидатор запросов
internal/postgres/                # пул соединений pgx за интерфейсом для моков
internal/journal/                 # журнал: записи, асинхронный писатель, репозиторий, миграции
internal/stats/                   # перцентили, доля откатов и статистика /v1/stats
internal/handlers/adminauth/      # проверка админ-токена /v1/stats и /v1/config
internal/handlers/statshandler/   # обработчик GET /v1/stats
migrations/                       # SQL-миграции журнала (встраиваются в бинарник)
testdata/contracts/               # эталонные контракты сервисов ИИ-моделей
testdata/protocol/                # вектора протокола alv.v1
testdata/stats/                   # эталон ответа /v1/stats (пример PLT-13)
build/Dockerfile                  # сборка образа (distroless)
.github/workflows/                # CI и публикация образа
docs/spec.md                      # выдержка из спецификации системы
docs/development.md               # подготовка машины разработки
docs/changelog.md                 # изменения по версиям
scripts/regenerate-mocks.sh       # регенерация моков
scripts/sync-contracts.sh         # синхронизация контрактов с соседними репозиториями
scripts/setup-dev-env.sh          # подготовка машины разработки
scripts/test-integration.sh       # интеграционные тесты журнала с PostgreSQL в Docker
```

## Лицензия

[Apache License 2.0](LICENSE).
