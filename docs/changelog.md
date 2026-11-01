# Changelog

### v0.1.0
- Добавлены эталонные контракты сервисов ИИ-моделей в `testdata/contracts/`: примеры запросов и ответов детектора, ASR и TTS, ошибок и результатов замеров EXP-2.
- Добавлен эталон ответа словаря детектора `testdata/contracts/objects-detector/vocabulary.response.json` с хешем словаря.
- Добавлена таблица случаев нормализации текста `testdata/contracts/asr-service/normalize.cases.json` - общее правило PLT-4 и ASR-4.
- Добавлен список фраз прогрева TTS `testdata/contracts/tts-service/prewarm_phrases.txt` - фразы правил опасности по умолчанию.
- Добавлены вектора протокола `alv.v1` в `testdata/protocol/vectors.json`: кадры, битые кадры, сообщения приложения и Шлюза, неверные сообщения.
- Добавлен скрипт `scripts/sync-contracts.sh`: копирует контракты в репозитории сервисов и приложения, с `--check` сверяет копии.
- Добавлена выдержка из общей спецификации в `docs/spec.md`.
- Добавлен скрипт `scripts/setup-dev-env.sh` подготовки машины разработки: Go 1.24.13, moq v0.5.3, JDK 17, Android SDK (API 37, build-tools 37.0.0), Gradle 9.8.2 и Python 3.12; лицензии Android SDK принимаются только с явным флагом.
- Добавлено описание подготовки машины разработки в `docs/development.md`.
- Добавлен каркас Шлюза на Go 1.24: модуль `github.com/nikarpoff/assistant-low-vision-platform`, точка входа `cmd/gateway` с остановкой по сигналу.
- Добавлен собственный printf-логгер `internal/logger` со строками `<LEVEL>: <message>` и уровнем из `APP_LOG_LEVEL`.
- Добавлены настройки Шлюза `internal/env`: все переменные окружения со значениями по умолчанию, проверки режима описания, модели, адреса поставщика и токенов при старте.
- Добавлены метрики Prometheus `internal/metrics` в собственном реестре: этапы, E2E, запросы, отказы сервисов и поставщика, активные сессии, отброшенные записи журнала.
- Добавлен HTTP-сервер с `GET /health` и валидатор запросов.
- Добавлена сборка distroless-образа `build/Dockerfile` и `.dockerignore`.
- Добавлены CI на GitHub Actions (генерация моков, gofmt, vet, тесты) и публикация образа в GHCR с тегами `latest`, `sha-<7>` и `v<semver>`.
- Добавлен скрипт `scripts/regenerate-mocks.sh`, моки исключены из git.
- README переписан на русском: назначение, стек, быстрый старт, таблица переменных, протокол, HTTP API, разработка, структура.
- Добавлен пакет `internal/protocol` протокола `alv.v1`: кодирование и разбор бинарных кадров PROTO-3 с лимитами снимка (`APP_MAX_IMAGE_BYTES`) и фрагмента аудио (64 КиБ), сообщения приложения и Шлюза PROTO-4 и PROTO-5 с проверкой обязательных полей и перечислений, проверка версии в `hello` (PROTO-10), коды ошибок PROTO-11 с `retryable`, длительности `timings` с одним знаком после запятой (PROTO-12). Кодек проходит все вектора `testdata/protocol/vectors.json`.
- Добавлена зависимость `github.com/google/uuid` v1.6.0 для `request_id` и `session_id`.
- Добавлены клиенты детектора, ASR и TTS `internal/upstream`: пул keep-alive соединений на каждый сервис, бинарные тела, заголовки `Authorization` и `X-Request-ID`, таймауты PLT-8, типизированные ошибки (`ErrBusy`, `ErrUnavailable`, `ErrTimeout`, `ErrUnauthorized`, `ErrRejected`, `ErrInvalidResponse`) и счётчик `alv_upstream_errors_total{upstream, reason}`.
- Добавлен потоковый клиент TTS: ответ отдаётся фрагментами из целых отсчётов, таймаут `APP_TTS_FIRST_CHUNK_TIMEOUT_MS` ограничивает ожидание первого фрагмента.
- Добавлен словарь детектора `internal/vocabulary`: загрузка при старте с нарастающей паузой между попытками, проверка классов, формы названий для количества и сопоставитель упоминаний `mentions`.
- Добавлен снимок конфигурации стенда `internal/snapshot` и `config_id`: `/v1/info` сервисов, поставщик и модель языковой модели (кроме режима `template`), собственные настройки Шлюза и метка варианта размещения `placement` на верхнем уровне, без секретов; обновление раз в `APP_CONFIG_REFRESH_S`.
- В README добавлены разделы о клиентах сервисов ИИ-моделей, словаре детектора и снимке конфигурации стенда.
