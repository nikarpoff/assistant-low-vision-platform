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
- Добавлен клиент языковой модели `internal/llm` для любого OpenAI-совместимого поставщика: запрос с `stream: true`, параметрами генерации и `APP_LLM_EXTRA_BODY`, только заголовки `Authorization`, `Content-Type` и `Accept`, чтение SSE с комментариями и фрагментом `usage`, проверка поля `error` в каждом фрагменте, `model` и `system_fingerprint` из ответа, таймауты первого токена и всей генерации, виды отказа PLT-15 в метрике `alv_upstream_errors_total{upstream="llm"}`, без повторов и с проверкой TLS.
- Добавлена заглушка поставщика `cmd/fakellm` (`internal/fakellm`): поток SSE в формате Polza.ai, `/v1/models`, ключ, задержки и модель из флагов и переменных `FAKELLM_*`, все режимы отказа `FAKELLM_FAIL`, запись полученных запросов для тестов.
- README дополнен описанием клиента языковой модели, видов отказа поставщика и заглушки `cmd/fakellm`.
