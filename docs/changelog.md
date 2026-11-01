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
