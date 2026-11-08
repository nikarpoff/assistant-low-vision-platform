# Развёртывание системы

Каталог `deployments/` разворачивает всю систему: Шлюз, PostgreSQL, сервисы ИИ-моделей ([objects-detector](https://github.com/nikarpoff/objects-detector), [asr-service](https://github.com/nikarpoff/asr-service), [tts-service](https://github.com/nikarpoff/tts-service)), обратный прокси Caddy с TLS и прокси gpt2giga для GigaChat. Требования - раздел DEP в [../docs/spec.md](../docs/spec.md).

## Содержание

- [Файлы](#файлы)
- [Варианты размещения](#варианты-размещения)
- [Туннель WireGuard](#туннель-wireguard)
- [TLS и открытые порты](#tls-и-открытые-порты)
- [Настройки и секреты](#настройки-и-секреты)
- [Поставщики языковой модели](#поставщики-языковой-модели)
- [Веса моделей](#веса-моделей)
- [Эталонная конфигурация](#эталонная-конфигурация)
- [Замер](#замер)
- [Живая проверка поставщиков](#живая-проверка-поставщиков)
- [Стенд на заглушках](#стенд-на-заглушках)
- [Типичные неполадки](#типичные-неполадки)

## Файлы

| Файл | Где запускается | Что внутри |
|---|---|---|
| `compose.vm.yml` | VM, вариант B | Caddy (TLS), Шлюз, PostgreSQL, gpt2giga (профиль `gigachat`) |
| `compose.models.yml` | машина моделей, вариант B | детектор и ASR (образы `-cuda`), TTS (образ `-cpu`); порты - только на адресе туннеля |
| `compose.single.yml` | машина моделей, вариант A | Шлюз, PostgreSQL, gpt2giga (профиль `gigachat`), детектор, ASR, TTS; порт Шлюза - только на адресе туннеля |
| `compose.vm-proxy.yml` | VM, вариант A | только Caddy: TLS и проксирование на Шлюз через туннель |
| `compose.fake.yml` | любая машина без видеокарты, весов и ключей | Шлюз, PostgreSQL, сервисы на бэкенде `fake`, заглушка поставщика `cmd/fakellm` |
| `compose.local-build.yml` | вместе с любым файлом выше | сборка образов из соседних каталогов общего проекта вместо загрузки из реестра |
| `caddy/Caddyfile` | VM | домен из `ALV_DOMAIN`, адрес Шлюза из `ALV_GATEWAY_UPSTREAM` |
| `wireguard/vm.conf.example`, `wireguard/models.conf.example` | VM, машина моделей | примеры конфигурации туннеля |
| `certs/russian_trusted_root_ca.pem` | VM или машина моделей | корневой сертификат НУЦ Минцифры для gpt2giga |
| `.env.example` | обе машины | все переменные с комментариями; копируется в `.env` |
| `reference.env` | обе машины эталонного стенда | несекретные значения эталонной конфигурации |
| `fetch-weights.sh` | машина моделей | загрузка весов моделей в тома |
| `llm-live-check.sh` | VM или машина разработки | живая проверка Polza.ai и GigaChat настоящими ключами |

Имена сервисов во всех файлах: `caddy`, `gateway`, `postgres`, `gpt2giga`, `detector`, `asr`, `tts`, `fakellm`. Сеть Docker - `alv-net`, compose создаёт её сам. У каждого сервиса `restart: unless-stopped`; Шлюз стартует независимо от сервисов ИИ-моделей, а его готовность показывает `/ready`.

Образы:
- свои - `ghcr.io/nikarpoff/<репозиторий>:<ALV_IMAGE_TAG>`, у сервисов ИИ-моделей с суффиксом варианта: `latest-cuda`, `v0.1.0-cpu`. Образ Шлюза один на все инструменты: `/gateway` - точка входа, `/fakellm`, `/loadgen`, `/score`, `/llmbench` запускаются заменой `entrypoint`, поэтому Go и Python на VM не нужны;
- чужие закреплены версией: `caddy:2.11.7-alpine`, `postgres:17.11-alpine`, `ghcr.io/ai-forever/gpt2giga:0.3.0`.

Файлы compose не используют `env_file:`: все значения подставляются из `--env-file` через `${VAR:-значение}`, поэтому `docker compose config` работает и на чистом клоне без `.env`. `compose.single.yml`, `compose.vm-proxy.yml` и `compose.fake.yml` расширяют (`extends`) сервисы `compose.vm.yml` и `compose.models.yml`: у вариантов A и B одни и те же настройки Шлюза.

## Варианты размещения

| Вариант | VM | Машина моделей | Зачем |
|---|---|---|---|
| A. Всё своё на машине моделей | `compose.vm-proxy.yml`: только Caddy | `compose.single.yml`: Шлюз, PostgreSQL, gpt2giga, детектор, ASR, TTS | нет сетевого перехода между Шлюзом и моделями, но снимок и аудио идут через туннель |
| B. Распределённый (эталонный) | `compose.vm.yml`: Caddy, Шлюз, PostgreSQL, gpt2giga | `compose.models.yml`: детектор, ASR, TTS | Шлюз на VM с публичным адресом, модели - на видеокарте за туннелем |

Оборудование: VM - не меньше 2 vCPU и 2 ГБ ОЗУ, публичный IP и домен; машина моделей - видеокарта 8 ГБ (её делят детектор и ASR, TTS по умолчанию на CPU), 8 ядер CPU, 16 ГБ ОЗУ, выход в интернет через NAT. На машине моделей нужен NVIDIA Container Toolkit. В обоих вариантах языковая модель - у облачного поставщика, к которому Шлюз обращается по HTTPS со своей машины.

Команды ниже выполняются из каталога `deployments/` на соответствующей машине. `reference.env` передаётся вторым `--env-file` только на эталонном стенде, когда задача 002 заполнила его значения; без него берутся значения из `.env`.

### Вариант B

1. На обеих машинах - клон репозитория, `cp .env.example .env`, секреты и общие токены сервисов в `.env` (см. [Настройки и секреты](#настройки-и-секреты)).
2. Туннель: на VM `wg-quick up wg0`, затем на машине моделей `wg-quick up wg0`; проверка - `ping 10.77.0.1` с машины моделей.
3. Машина моделей: веса и сервисы.

   ```bash
   ./fetch-weights.sh --env-file .env --env-file reference.env
   docker compose --env-file .env --env-file reference.env -f compose.models.yml up -d
   ```

4. VM: Caddy, Шлюз, PostgreSQL (и gpt2giga, если поставщик - GigaChat).

   ```bash
   docker compose --env-file .env --env-file reference.env -f compose.vm.yml up -d
   docker compose --env-file .env --env-file reference.env -f compose.vm.yml --profile gigachat up -d   # GigaChat
   ```

5. Проверка: `curl https://<ALV_DOMAIN>/health` - `OK`; `curl -H "Authorization: Bearer <APP_ADMIN_TOKEN>" https://<ALV_DOMAIN>/ready` - все сервисы и `llm` в состоянии `ok`.

Порядок важен: туннель поднимается до compose, потому что порты сервисов публикуются на адресе `10.77.0.2`. Сервисы ИИ-моделей лучше поднять до Шлюза, иначе до их готовности `/ready` отвечает 503 (Шлюз при этом работает и повторяет загрузку словаря).

### Вариант A

1. Шаги 1-2 варианта B.
2. Машина моделей: всё своё.

   ```bash
   ./fetch-weights.sh --env-file .env --env-file reference.env
   docker compose --env-file .env --env-file reference.env -f compose.single.yml up -d
   ```

3. VM: только Caddy, он проксирует на `10.77.0.2:8080` через туннель.

   ```bash
   docker compose --env-file .env -f compose.vm-proxy.yml up -d
   ```

В варианте A метка размещения всегда `A`: `compose.single.yml` задаёт её сам, поэтому `APP_PLACEMENT=B` из `reference.env` её не меняет. Снимки `/v1/config` вариантов A и B при одинаковых моделях и поставщике различаются только полем `placement`. Сервисы ИИ-моделей в варианте A не публикуются вовсе, Шлюз слушает только `10.77.0.2:8080`.

## Туннель WireGuard

VM и машину моделей связывает WireGuard на хостах (`wg-quick`), не в compose. Инициатор - машина моделей, потому что она за NAT.

| Узел | Адрес в туннеле | Переменная | Пример конфигурации |
|---|---|---|---|
| VM | `10.77.0.1` | `ALV_TUNNEL_VM_IP` | `wireguard/vm.conf.example` |
| Машина моделей | `10.77.0.2` | `ALV_TUNNEL_MODELS_IP` | `wireguard/models.conf.example` |

1. Ключи - на каждой машине свои, приватный ключ не покидает хост и не попадает в git:

   ```bash
   umask 077
   wg genkey | tee vm.key | wg pubkey > vm.pub          # на VM
   wg genkey | tee models.key | wg pubkey > models.pub  # на машине моделей
   ```

2. Конфигурации: пример копируется в `/etc/wireguard/wg0.conf`, на места `<...>` вписываются ключи и публичный IP VM. У машины моделей `Endpoint` - публичный IP VM и `PersistentKeepalive = 25`; у VM для этого пира `Endpoint` не задан. На VM открыт UDP-порт 51820.
3. Запуск до compose: `wg-quick up wg0` на VM, затем на машине моделей; автозапуск - `systemctl enable wg-quick@wg0`.
4. Проверка связи: `wg show` - свежий `latest handshake`; `ping 10.77.0.1` с машины моделей и `ping 10.77.0.2` с VM; после запуска compose - `curl http://10.77.0.2:8101/health` с VM (вариант B).

Если адреса туннеля другие, они меняются в `wg0.conf` обеих машин и в `ALV_TUNNEL_VM_IP` и `ALV_TUNNEL_MODELS_IP` в `.env`.

## TLS и открытые порты

- A-запись домена `ALV_DOMAIN` указывает на публичный IP VM. Caddy сам выпускает и продлевает сертификат Let's Encrypt; собственный удостоверяющий центр и работа без домена не поддерживаются. Сертификаты лежат в томе `caddy-data`, поэтому перезапуск не выпускает их заново.
- TLS 1.2 и 1.3, HTTP/1.1 и HTTP/2; WebSocket проксируется без буферизации, таймаут чтения от Шлюза - 120 с (Шлюз шлёт ping каждые 15 с). Порт 80 нужен для ACME и перенаправления на HTTPS.
- Приложение подключается по `wss://<ALV_DOMAIN>/v1/session`.

Открытые порты VM в интернет: 80/tcp и 443/tcp (Caddy), UDP 51820 (WireGuard), SSH (вход только по ключу). У машины моделей входящих портов из интернета нет. Остальные порты на хосте публикуются только на адресе туннеля или на `127.0.0.1`:

| Порт | Сервис | Где слушает |
|---|---|---|
| 80, 443 | Caddy | все интерфейсы VM |
| 8080 | Шлюз | `10.77.0.2` (вариант A); `127.0.0.1` (стенд на заглушках); в варианте B не публикуется - Caddy ходит к нему внутри `alv-net` |
| 8101, 8102, 8103 | детектор, ASR, TTS | `10.77.0.2` (вариант B); в варианте A не публикуются |
| 8104 | gpt2giga | `127.0.0.1` (отладка и живая проверка) |
| 5432 | PostgreSQL | не публикуется |

Межсетевой экран VM, например `ufw`: `allow 22/tcp`, `allow 80/tcp`, `allow 443/tcp`, `allow 51820/udp`, остальное входящее - `deny`. Проверка снаружи: `nmap -Pn -p 22,80,443,5432,8080,8101-8104 <IP VM>` и `nmap -sU -p 51820 <IP VM>`; на машине моделей `ss -tlnp` - порты 8101-8103 (или 8080 в варианте A) только на `10.77.0.2`. TLS: `openssl s_client -connect <ALV_DOMAIN>:443 -servername <ALV_DOMAIN>` - сертификат Let's Encrypt, протокол TLSv1.2 или TLSv1.3.

## Настройки и секреты

`.env.example` содержит все переменные обеих машин с комментариями: для какого файла compose и какого варианта нужна переменная. Значения в нём - значения по умолчанию, секреты пустые. Настоящий `deployments/.env` на каждом хосте свой и в git не попадает (`.gitignore`).

| Переменная | Где нужна | Смысл |
|---|---|---|
| `ALV_DOMAIN` | VM | домен приложения |
| `ALV_GATEWAY_UPSTREAM` | VM | адрес Шлюза для Caddy; пусто - `gateway:80` (B) или `${ALV_TUNNEL_MODELS_IP}:8080` (A) |
| `ALV_TUNNEL_VM_IP`, `ALV_TUNNEL_MODELS_IP` | обе | адреса туннеля, `10.77.0.1` и `10.77.0.2` |
| `ALV_IMAGE_TAG` | обе | тег своих образов: `latest`, `v0.1.0` |
| `APP_*` | машина Шлюза | настройки Шлюза, таблица - в [README репозитория](../README.md#env) |
| `POSTGRES_PASSWORD` | машина Шлюза | пароль журнала |
| `GIGACHAT_CREDENTIALS`, `GIGACHAT_SCOPE`, `GIGACHAT_MODEL` | машина Шлюза | gpt2giga |
| `DETECTOR_*`, `ASR_*`, `TTS_*` | машина моделей; токены - обе | `MODEL_BACKEND`, `MODEL_NAME`, `TARGET_PLATFORM`, `AUTH_TOKEN`, `WORKERS`, `QUEUE_MAX` сервиса |

Секреты - только в `.env` или окружении, никогда в git, логах и тикетах:
- `APP_DEVICE_TOKENS`, `APP_ADMIN_TOKEN` - без них Шлюз не стартует (если не `APP_AUTH_DISABLED=true`);
- `DETECTOR_AUTH_TOKEN`, `ASR_AUTH_TOKEN`, `TTS_AUTH_TOKEN` - один и тот же токен задаётся в `.env` обеих машин: сервис принимает его как `AUTH_TOKEN`, Шлюз передаёт как `APP_*_TOKEN`. Пустой токен выключает проверку в сервисе, поэтому на стенде их задают всегда;
- `POSTGRES_PASSWORD` - только буквы и цифры, он входит в строку подключения;
- `APP_LLM_API_KEY`, `GIGACHAT_CREDENTIALS` - ключи поставщиков;
- приватные ключи WireGuard - в `/etc/wireguard`, не в `.env`.

Токены и пароли удобно генерировать так: `openssl rand -hex 32`. После правки `.env` - `docker compose ... up -d`: compose пересоздаст контейнеры с новыми значениями.

Переменные Шлюза `APP_HAZARD_RULES_PATH` и `APP_INTENTS_PATH` в файлах compose не передаются: Шлюз берёт встроенные правила и фразы. Чтобы подменить их, файл монтируется в контейнер Шлюза, а путь задаётся в `environment`.

## Поставщики языковой модели

Языковая модель - только облачные поставщики по OpenAI-совместимому API. Шлюз работает с ними одним клиентом.

| Поставщик | `APP_LLM_URL` | `APP_LLM_API_KEY` | Дополнительно |
|---|---|---|---|
| Polza.ai (модели Qwen) | `https://polza.ai/api/v1` | ключ Polza.ai | - |
| GigaChat (Сбер) через gpt2giga | `http://gpt2giga:8090/v1` | ключ доступа к прокси: любая длинная случайная строка, она же становится `GPT2GIGA_API_KEY` | профиль `gigachat`, `GIGACHAT_CREDENTIALS` |

- **Смена модели** - одна переменная `APP_LLM_MODEL` (для режима `vlm` - `APP_VLM_MODEL`) и `docker compose ... up -d`. `config_id` меняется, приложение не пересобирается.
- **Смена поставщика** - три переменные `APP_LLM_URL`, `APP_LLM_API_KEY`, `APP_LLM_MODEL` и профиль `gigachat` (`--profile gigachat` для GigaChat, без него - для Polza.ai). Код Шлюза и приложение не меняются.
- **Секреты** `APP_LLM_API_KEY` и `GIGACHAT_CREDENTIALS` хранятся только в `deployments/.env` (не в git) или в окружении. `reference.env` секретов не содержит.

### gpt2giga

[gpt2giga](https://github.com/ai-forever/gpt2giga) - официальный прокси GigaChat с OpenAI-совместимым API, лицензия [MIT](https://github.com/ai-forever/gpt2giga/blob/main/LICENSE). Прокси сам получает и обновляет OAuth-токен GigaChat (живёт 30 минут) по `GIGACHAT_CREDENTIALS`; Шлюз о нём не знает и видит обычный OpenAI-совместимый сервер.

Сервис `gpt2giga` (образ `ghcr.io/ai-forever/gpt2giga:0.3.0`) есть в `compose.vm.yml` и `compose.single.yml`, профиль `gigachat`:
- из `.env`: `GIGACHAT_CREDENTIALS`, `GIGACHAT_SCOPE` (по умолчанию `GIGACHAT_API_PERS`), `GIGACHAT_MODEL` (модель прокси по умолчанию; Шлюз всё равно передаёт `model` в каждом запросе), `GPT2GIGA_API_KEY` = `${APP_LLM_API_KEY}`;
- задано в compose и из `.env` не меняется: `GIGACHAT_VERIFY_SSL_CERTS=True`, `GIGACHAT_CA_BUNDLE_FILE=/certs/russian_trusted_root_ca.pem`, `GPT2GIGA_MODE=PROD`, `GPT2GIGA_ENABLE_API_KEY_AUTH=True`, `GPT2GIGA_TRAFFIC_LOG_ENABLED=False` (прокси не сохраняет запросы и снимки), `GPT2GIGA_LOG_LEVEL=INFO`;
- порт 8090 контейнера опубликован только на `127.0.0.1:8104`; Шлюз ходит на `http://gpt2giga:8090/v1` внутри `alv-net`;
- healthcheck - `GET /health`.

Отключать проверку TLS (`GIGACHAT_VERIFY_SSL_CERTS=False`) запрещено.

### Сертификат НУЦ Минцифры

TLS-сертификаты GigaChat выданы Национальным удостоверяющим центром Минцифры, поэтому gpt2giga доверяет корню из `certs/russian_trusted_root_ca.pem`. Файл смонтирован в контейнер только для чтения по пути `/certs/russian_trusted_root_ca.pem`. Это публичный корневой сертификат; Шлюзу и приложению он не нужен.

| Поле | Значение |
|---|---|
| Субъект | `C=RU, O=The Ministry of Digital Development and Communications, CN=Russian Trusted Root CA` (самоподписанный) |
| Серийный номер | `1000` |
| Действует | с 2022-03-01 по 2032-02-27 |
| SHA-256 отпечаток | `D2:6D:2D:02:31:B7:C3:9F:92:CC:73:85:12:BA:54:10:35:19:E4:40:5D:68:B5:BD:70:3E:97:88:CA:8E:CF:31` |
| SHA-256 файла | `936a43fea6e8e525bcc0f81acd9c3d21b4fc4b9b68acea7906d698005afc6504` |

Откуда взят (2026-11-01):
- скачан по HTTPS с проверкой сертификата со статического сервера портала Госуслуг `gu-st.ru`: [https://gu-st.ru/content/lending/russian_trusted_root_ca_pem.crt](https://gu-st.ru/content/lending/russian_trusted_root_ca_pem.crt). Ссылки на сертификаты НУЦ публикует страница [https://www.gosuslugi.ru/crt](https://www.gosuslugi.ru/crt); этот же адрес приводит официальная документация GigaChat ([developers.sber.ru](https://developers.sber.ru/docs/ru/gigachat/certificates)). Файл `https://gu-st.ru/content/Other/doc/russian_trusted_root_ca.cer` побайтно тот же;
- отпечаток SHA-256 совпадает с опубликованным для Russian Trusted Root CA (например, в [andreiborisov/ca-strated](https://github.com/andreiborisov/ca-strated), там же совпадает SHA-256 PEM-файла). Сама страница Госуслуг собирается скриптом и автоматически не читается;
- цепочки `gigachat.devices.sberbank.ru:443`, `ngw.devices.sberbank.ru:9443` (выдача токенов) и `api.giga.chat:443` проверяются этим корнем: `Verify return code: 0 (ok)`.

Проверка файла:

```bash
openssl x509 -in certs/russian_trusted_root_ca.pem -noout -subject -fingerprint -sha256
```

Если GigaChat когда-нибудь перестанет отдавать промежуточный сертификат Russian Trusted Sub CA, он дописывается в тот же файл после корневого (с той же страницы Госуслуг).

### Условия поставщиков

- Polza.ai: [пользовательское соглашение](https://polza.ai/documents/user-agreement), [политика конфиденциальности](https://polza.ai/documents/privacy-policy), [документация API](https://polza.ai/docs/api-reference/introduction.md).
- GigaChat: [условия использования для физических лиц](https://developers.sber.ru/docs/ru/policies/gigachat-agreement/individuals), [документация](https://developers.sber.ru/docs/ru/gigachat/overview).

В режиме `llm` поставщику уходит только текст: объекты сцены, сводка истории, расшифровка вопроса. В режиме `vlm` уходит и снимок, поэтому `vlm` - только в экспериментах с согласием на передачу снимков облачному поставщику.

## Веса моделей

У детектора, ASR и TTS в `compose.models.yml` и `compose.single.yml` именованные тома `alv-detector-weights`, `alv-asr-weights`, `alv-tts-weights`, смонтированные в `/opt/model` (это `ARTIFACTS_PATH` сервиса). Имена томов фиксированы, поэтому оба файла и скрипт загрузки используют одни и те же тома, а перезапуск ничего не скачивает заново. В том же томе лежит кэш библиотек (`XDG_CACHE_HOME=/opt/model/.cache`).

Бэкенд и модель сервиса задаются в `.env` или `reference.env`: `DETECTOR_MODEL_BACKEND`, `DETECTOR_MODEL_NAME`, `DETECTOR_TARGET_PLATFORM` и так же для `ASR_*` и `TTS_*`. По умолчанию - `fake`: он отвечает без весов и видеокарты, а `/v1/info` и `config_id` показывают `backend: fake`. Бэкенды-кандидаты:

| Сервис | Бэкенды |
|---|---|
| детектор | `ultralytics`, `rfdetr`, `onnx`, `grounding_dino` |
| ASR | `gigaam`, `tone`, `whisper`, `vosk` |
| TTS | `silero`, `piper`, `f5tts` |

Лицензии моделей и весов - в README сервисов. Веса в образы не входят.

`fetch-weights.sh` заранее скачивает веса моделей из файла настроек для офлайн-старта:

```bash
./fetch-weights.sh --dry-run --env-file .env.example   # только план, без Docker и сети
./fetch-weights.sh --env-file .env --env-file reference.env
```

```
fetch: detector onnx/yolo11n -> alv-detector-weights:/opt/model
fetch: asr fake/default -> alv-asr-weights:/opt/model
fetch: tts fake/default -> alv-tts-weights:/opt/model
```

- `--env-file` по умолчанию - `deployments/.env`; можно повторить, тогда последний файл главнее, а переменные окружения главнее файлов - как у `docker compose`. Файлы читаются как данные, не исполняются.
- Строка на каждый сервис: `fetch: <сервис> <бэкенд>/<модель> -> <том>:<каталог>`; `default` - модель бэкенда по умолчанию.
- Без `--dry-run` скрипт запускает `python -m scripts.fetch_weights` в образе сервиса через `docker compose -f compose.models.yml run`, с теми же настройками и томом, что у сервиса. Python и модели на хосте не нужны. Ненулевой код выхода, если загрузка хоть одного сервиса не удалась.

## Эталонная конфигурация

Эталонная конфигурация для замеров NFR-1 - вариант B. Её несекретные значения лежат в `reference.env`: `APP_PLACEMENT=B`, `APP_DESCRIBER_MODE=llm`, `APP_LLM_URL`, `APP_LLM_MODEL`, `APP_VLM_MODEL`, бэкенды и модели сервисов, тег образов. На эталонном стенде `reference.env` передаётся вторым `--env-file` после `.env`, поэтому его значения главнее.

Пока задача 002 не зафиксировала модели, `APP_LLM_MODEL` в `reference.env` пуст и перекрывает значение из `.env`: Шлюз в режиме `llm` не стартует с ошибкой `APP_LLM_MODEL environment variable is required in llm mode`. Так стенд нельзя по ошибке принять за эталонный. Пустые бэкенды сервисов дают `fake`, пустой тег - `latest`.

| Параметр | Значение |
|---|---|
| Вариант размещения | B (`compose.vm.yml` на VM, `compose.models.yml` на машине моделей) |
| VM: тип и регион | вписывается при развёртывании стенда |
| VM: vCPU и ОЗУ | вписывается при развёртывании стенда |
| Машина моделей: CPU | вписывается при развёртывании стенда |
| Машина моделей: видеокарта | вписывается при развёртывании стенда |
| Машина моделей: ОЗУ | вписывается при развёртывании стенда |
| Телефон: модель, версия Android, ОЗУ | вписывается перед испытаниями на телефоне |
| Сеть телефона | Wi-Fi с RTT до VM не больше 50 мс; отдельно - мобильная сеть (LTE) |
| Поставщик языковой модели | задача 002 |
| Идентификаторы моделей (`APP_LLM_MODEL`, `APP_VLM_MODEL`, бэкенды и модели сервисов) | задача 002 |
| Даты замеров | задача 002 |

## Замер

Инструменты эксперимента запускаются из образа Шлюза, Go на машине не нужен. Пример - `llmbench`:

```bash
IMAGE=ghcr.io/nikarpoff/assistant-low-vision-platform:latest
docker run --rm -e APP_LLM_API_KEY -v "$PWD/../testdata/llmbench:/testdata/llmbench:ro" -w / \
  --entrypoint /llmbench "$IMAGE" --url https://polza.ai/api/v1 --model <модель> --warmup 5 --n 50 > llmbench.json
```

- `llmbench` - TTFT и время до конца первого предложения у поставщика тем же клиентом, что у Шлюза; флаги `--url`, `--model`, `--n`, `--warmup`, `--image <jpeg>` (снимок, как в режиме `vlm`). Замер для эталонной конфигурации делается с VM, для варианта A - с машины моделей. Ключ передаётся только по имени переменной (`-e APP_LLM_API_KEY`).
- `loadgen` - имитатор приложений по настоящему протоколу: манифест набора данных, режимы `describe`, `ask`, `passive`, число прогонов `--runs` и номер первого прогона `--first-run` (прогоны в разное время нумеруются без совпадений). Пишет JSONL по запросам и сводку. Набор данных монтируется в контейнер, адрес - `wss://<ALV_DOMAIN>/v1/session` и токен устройства.
- `/v1/stats` - p50 и p95 по этапам, доли деградации и откатов по журналу: `curl -H "Authorization: Bearer <APP_ADMIN_TOKEN>" https://<ALV_DOMAIN>/v1/stats`. `/v1/config` - снимок конфигурации и `config_id`.
- `score` - оценка ответов по JSONL `loadgen`; с `--stats <файл>` (сохранённый ответ `/v1/stats`) добавляет вердикт по гипотезе.

Все флаги - `--help` инструмента. Методика, прогоны и отчёты - задача `002-hypothesis-experiment`.

## Живая проверка поставщиков

`llm-live-check.sh` проверяет настоящих поставщиков настоящими ключами. В сквозную проверку на заглушках она не входит. Ключи и идентификаторы моделей берутся только из окружения: если переменной нет, скрипт печатает `live: <ПЕРЕМЕННАЯ> is empty` и завершается с кодом 1 - пропуск не считается успехом.

```bash
export APP_LLM_API_KEY=... APP_LLM_MODEL=... APP_VLM_MODEL=...
./llm-live-check.sh polza
# live: polza llm ok model=<model из ответа> ttft_p95_ms=<...>
# live: polza vlm ok model=<model из ответа>

export GIGACHAT_CREDENTIALS=... GIGACHAT_MODEL=...
./llm-live-check.sh gigachat
# live: gigachat ok model=<model из ответа> ttft_p95_ms=<...>
```

- `polza`: адрес всегда `https://polza.ai/api/v1`, независимо от `APP_LLM_URL`. `llmbench` делает 1 прогревочный и 5 потоковых запросов с эталонным промптом моделью `APP_LLM_MODEL`, затем 3 запроса со снимком `testdata/llmbench/scene.jpg` моделью `APP_VLM_MODEL`.
- `gigachat`: поднимает `gpt2giga` из `compose.vm.yml` (профиль `gigachat`, проект `alv-live`) с одноразовым случайным ключом прокси, ждёт `/health` на `127.0.0.1:8104` и запускает `llmbench` на `http://127.0.0.1:8104/v1` моделью `GIGACHAT_MODEL`: 1 прогревочный и 5 потоковых запросов. Прокси останавливается при любом исходе. Порт 8104 должен быть свободен: работающий gpt2giga стенда на время проверки останавливают. Проверка TLS включена, поэтому успех подтверждает и сертификат НУЦ.
- Запрос засчитан, если пришёл непустой текст, поток завершился нормально и в ответе есть непустой `model`. Любой отказ - код выхода не 0.
- Ключи передаются в контейнеры только по имени переменной, не печатаются и не пишутся в файлы.
- `llmbench` берётся из образа `ghcr.io/nikarpoff/assistant-low-vision-platform:${ALV_IMAGE_TAG:-latest}`; с `ALV_LOCAL_BUILD=1` образ собирается из этого репозитория. Нужны Docker и `curl`.

## Стенд на заглушках

`compose.fake.yml` поднимает всю цепочку без видеокарты, весов и ключей: сервисы на бэкенде `fake`, заглушка поставщика `cmd/fakellm`, модель `fake/fake-llm`. Наружу публикуется только `127.0.0.1:8080`.

```bash
docker compose --env-file .env.example -f compose.fake.yml -p alv-fake up -d
# образы из соседних каталогов общего проекта (лёгкие сервисы с EXTRAS=""):
docker compose --env-file .env.example -f compose.fake.yml -f compose.local-build.yml -p alv-fake up -d --build
```

Шлюз требует токены устройства и админа: задайте их в окружении (`APP_DEVICE_TOKENS=... APP_ADMIN_TOKEN=... docker compose ...`) или `APP_AUTH_DISABLED=true`. Остановка - `docker compose -f compose.fake.yml -p alv-fake down -v`.

## Типичные неполадки

| Признак | Причина и решение |
|---|---|
| `bind: cannot assign requested address` для `10.77.0.2` | туннель не поднят: сначала `wg-quick up wg0`, потом compose |
| `/ready` - 503, детектор, ASR или TTS `unavailable` | туннель или сервисы не работают: `wg show`, `curl http://10.77.0.2:8101/health` с VM, `docker compose ... ps` на машине моделей; совпадают ли `*_AUTH_TOKEN` в `.env` обеих машин (иначе `unauthorized`) |
| Шлюз перезапускается с `APP_LLM_MODEL environment variable is required in llm mode` | не задана модель; на эталонном стенде `reference.env` ещё не заполнен задачей 002 |
| Шлюз перезапускается с `APP_DEVICE_TOKENS environment variable is required` | не заданы токены устройства или админа |
| Caddy не получает сертификат | A-запись не указывает на IP VM, закрыт порт 80 или 443, исчерпан лимит Let's Encrypt; `docker compose ... logs caddy` |
| Приложение не подключается по WSS | неверный домен или токен устройства; `curl https://<ALV_DOMAIN>/health` |
| PostgreSQL не стартует: `superuser password is not specified` | пуст `POSTGRES_PASSWORD` |
| Журнал не пишется после смены `POSTGRES_PASSWORD` | пароль задаётся только при первом создании тома `postgres-data`: верните прежний или пересоздайте том (журнал будет потерян) |
| gpt2giga: `CERTIFICATE_VERIFY_FAILED` | файл `certs/russian_trusted_root_ca.pem` повреждён или GigaChat сменил цепочку; сверьте отпечаток, при необходимости допишите Russian Trusted Sub CA. Проверку TLS не отключать |
| gpt2giga: 401 от прокси | `APP_LLM_API_KEY` Шлюза не совпадает с ключом прокси: оба берутся из одной переменной, пересоздайте оба контейнера |
| `llm` в `/ready` - `unavailable` с `insufficient_balance` в журнале | у поставщика кончились средства или лимит расходов |
| Сервис долго стартует или падает с ошибкой загрузки весов | веса не скачаны: `./fetch-weights.sh`; на машине без сети - скачать заранее |
| `could not select device driver "nvidia"` | на машине моделей не установлен NVIDIA Container Toolkit |
| `port is already allocated` для 8080 или 8104 | порт занят другим стендом или gpt2giga: остановите его (`docker compose ... down`) |
