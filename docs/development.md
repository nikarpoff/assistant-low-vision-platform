# Подготовка машины разработки

Скрипт `scripts/setup-dev-env.sh` ставит всё, что нужно для работы со всеми репозиториями проекта: Go и moq для Шлюза, JDK, Android SDK и Gradle для приложения, Python 3.12 для сервисов ИИ. Скрипт идемпотентный: то, что уже стоит в нужной версии, не переустанавливается, поэтому повторный запуск ничего не меняет.

## Что ставит скрипт

| Компонент | Версия | Куда | Зачем |
|---|---|---|---|
| Go | 1.24.13 | `/usr/local/go`, ссылки `/usr/local/bin/go` и `/usr/local/bin/gofmt` | сборка и тесты Шлюза |
| moq | v0.5.3 | `/usr/local/bin/moq` | моки для `go generate` |
| JDK | OpenJDK 17, пакет `openjdk-17-jdk-headless`; 21, если 17 нет в репозитории дистрибутива | apt, `/usr/lib/jvm/java-17-openjdk-amd64` | Gradle и `sdkmanager` |
| Android cmdline-tools | 22.0, сборка 15859902 | `/opt/android-sdk/cmdline-tools/latest` | `sdkmanager` |
| Android SDK | `platform-tools`, `platforms;android-37.0`, `build-tools;37.0.0` | `/opt/android-sdk` | сборка приложения |
| Gradle | 9.8.2 | `/opt/gradle/gradle-9.8.2` | только генерация Gradle Wrapper приложения |
| Python | 3.12 | `uv python install 3.12`, `~/.local/share/uv/python` | сервисы ИИ |

Ссылки на Go и moq лежат в `/usr/local/bin`, потому что команды критериев приёмки (`sdd.py verify`) выполняются в неинтерактивной оболочке, где PATH из профиля не гарантирован.

Кроме установки скрипт записывает две настройки:
- `sdk.dir=/opt/android-sdk` в `assistant-low-vision-mobile/local.properties`. Файл в `.gitignore` приложения;
- `org.gradle.daemon.idletimeout=600000` в `~/.gradle/gradle.properties`. Демон Gradle завершается после 10 минут простоя и не держит память.

Docker Engine с плагином compose и uv скрипт не ставит, а только проверяет.

## Требования

- Debian или Ubuntu на x86_64 с apt. Проверено на Ubuntu 26.04.1 LTS. `build-tools` и `platform-tools` Android для Linux выпускаются только под x86_64.
- Права sudo.
- Установлены `curl`, `unzip`, [uv](https://docs.astral.sh/uv/getting-started/installation/), Docker Engine с плагином compose.
- Доступ к `go.dev`, `proxy.golang.org`, `dl.google.com`, `services.gradle.org`, зеркалам apt и GitHub (откуда uv скачивает Python).
- Не меньше 15 ГБ свободного места. То, что ставит скрипт, занимает около 1,2 ГБ: Android SDK - 0,5 ГБ, Go и JDK - по 0,3 ГБ, Gradle - 0,2 ГБ. Остальное займут кэш Gradle, образы Docker с torch и веса моделей.

## Запуск

```bash
scripts/setup-dev-env.sh                            # первый запуск остановится перед лицензиями Android SDK
scripts/setup-dev-env.sh --accept-android-licenses  # после того как лицензии прочитаны
```

| Параметр | Переменная окружения | Описание | По умолчанию |
|---|---|---|---|
| `--accept-android-licenses` | `ALV_ACCEPT_ANDROID_LICENSES=1` | принять лицензии Android SDK | не принимать |
| `--mobile-dir DIR` | `ALV_MOBILE_DIR` | каталог репозитория приложения, куда пишется `local.properties` | `assistant-low-vision-mobile` рядом с этим репозиторием |

Скрипт запускается от своего пользователя: системные шаги он сам выполняет через sudo. При запуске через `sudo scripts/setup-dev-env.sh` настройки Gradle и Python 3.12 достанутся root, и скрипт об этом предупредит.

В git worktree задачи (`.worktrees/T-<n>/assistant-low-vision-platform`) рядом может не быть репозитория приложения. Тогда путь к нему передаётся явно:

```bash
scripts/setup-dev-env.sh --mobile-dir /path/to/assistant-low-vision/assistant-low-vision-mobile
```

Коды выхода:
- 0 - окружение готово;
- 1 - шаг или итоговая проверка не прошли, либо не приняты лицензии Android SDK;
- 2 - неверные аргументы.

## Лицензии Android SDK

Лицензии принимает только тот, кто их прочитал, или агент с его явного согласия. Поэтому без флага скрипт останавливается перед `sdkmanager --licenses` и печатает подсказку. Прочитать и принять лицензии вручную:

```bash
sudo env JAVA_HOME=/usr/lib/jvm/java-17-openjdk-amd64 /opt/android-sdk/cmdline-tools/latest/bin/sdkmanager --licenses
```

С флагом `--accept-android-licenses` скрипт выполняет `yes | sdkmanager --licenses`. Так принимаются все лицензии, которые предлагает `sdkmanager`, а их отметки ложатся в `/opt/android-sdk/licenses/`. Если `/opt/android-sdk/licenses/android-sdk-license` уже есть, шаг пропускается. Это лицензия всех пакетов, которые ставит скрипт.

### Кто принял лицензии

| Машина | Кто принял | Когда | Основание |
|---|---|---|---|
| машина разработки агентов `kbks` (Ubuntu 26.04.1 LTS, 2 CPU, 3,9 ГБ ОЗУ) | агент Claude в роли разработчика, командой `scripts/setup-dev-env.sh --accept-android-licenses` | 2026-11-01 | явное согласие автора от 2026-11-01 (решение 10 плана задачи 001) |

На своей машине автор принимает лицензии сам и дописывает сюда строку.

## Итоговая проверка

Скрипт заканчивается проверкой и печатает каждую команду с её выводом:
- `go version`;
- `moq -version` и версия модуля moq из `go version -m`. У бинарника, собранного `go install`, `moq -version` всегда печатает `moq version dev`;
- `java -version`;
- `sdkmanager --list_installed`;
- `gradle --version` из `/opt/gradle/gradle-9.8.2`;
- `uv python find 3.12`;
- `docker compose version`;
- свободное место на файловых системах `/opt`, домашнего каталога и каталога Docker.

Если не прошла любая из команд, скрипт перечисляет их и выходит с кодом 1. Нехватка места - только предупреждение с фактическим значением: часть места к концу установки уже занята. На машине `kbks` после установки осталось 13,2 ГБ.

`sdkmanager` печатает предупреждение, что он устарел и его заменяет Android CLI (`android sdk`). На работу это не влияет.

## Повторный запуск и смена версий

Каждый шаг сначала проверяет, что уже установлено:

| Шаг | Как проверяется |
|---|---|
| Go | первая строка `/usr/local/go/VERSION` |
| ссылки | цель `readlink` |
| moq | версия модуля в `go version -m /usr/local/bin/moq` |
| JDK | статус пакета в `dpkg-query` |
| cmdline-tools | есть `cmdline-tools/latest/bin/sdkmanager` |
| лицензии | есть `licenses/android-sdk-license` |
| пакеты SDK | есть `package.xml` в каталоге пакета |
| Gradle | есть `/opt/gradle/gradle-9.8.2/bin/gradle` |
| Python | `uv python find --managed-python 3.12` |
| `local.properties`, `gradle.properties` | строка `ключ=значение` уже записана |

Повторный запуск ничего не скачивает и не переписывает. Единственное исключение - `sdkmanager --list_installed` из итоговой проверки: он перезаписывает свой кэш `/opt/android-sdk/.knownPackages` тем же содержимым.

Версии и контрольные суммы SHA-256 заданы переменными в начале скрипта. Архивы проверяются по SHA-256 до распаковки и распаковываются во временный каталог рядом с целевым, поэтому прерванный запуск не оставляет полуготовой установки. При смене версии меняется и сумма:
- Go - `https://go.dev/dl/`;
- cmdline-tools - `https://developer.android.com/studio#command-tools`;
- Gradle - `https://services.gradle.org/distributions/gradle-<версия>-bin.zip.sha256`.

Если в `/usr/local/go` лежит другая версия Go, скрипт заменяет её на 1.24.13.

## Android: версии и сборка

- **Уровень API.** compileSdk и targetSdk - 37 (Android 17): последний стабильный уровень по `sdkmanager --list` на 2026-11-01. Ему соответствует пакет `platforms;android-37.0`. В репозитории есть и минорные платформы `android-37.1` и `android-37.2`, но targetSdk бывает только целым, поэтому ставится основная 37.0. Если приложению понадобится compileSdk с минорным уровнем, пакет добавляется в `android_packages` скрипта.
- **build-tools.** Версия 37.0.0 - последняя стабильная, под API 37. Если версия build-tools по умолчанию у AGP другая, AGP попробует скачать её в `/opt/android-sdk`. Каталог принадлежит root, и под обычным пользователем загрузка не пройдёт. Тогда версия задаётся явно, `buildToolsVersion = "37.0.0"` в `app/build.gradle.kts`, или недостающий пакет ставится через `sudo sdkmanager`.
- **JDK.** Если выбранные AGP и Kotlin потребуют JDK 21 (ошибка AGP о версии Java), в скрипте меняется JDK.
- **Gradle Wrapper.** Дистрибутив в `/opt/gradle` нужен только для генерации wrapper. Дальше приложение собирается через `./gradlew`:

  ```bash
  cd assistant-low-vision-mobile
  /opt/gradle/gradle-9.8.2/bin/gradle wrapper --gradle-version <версия>
  ```

Скрипт не меняет профили оболочки. Gradle находит SDK по `local.properties`. Для удобства в `~/.bashrc` можно добавить:

```bash
export ANDROID_HOME=/opt/android-sdk
export PATH="$PATH:$ANDROID_HOME/cmdline-tools/latest/bin:$ANDROID_HOME/platform-tools"
```
