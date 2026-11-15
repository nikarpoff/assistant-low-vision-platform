# Источники мини-набора

Встроенный мини-набор в формате EXP-1 для `cmd/loadgen`, сквозной проверки `deployments/e2e-smoke.sh` (DEP-8) и тестов: 5 снимков (3 в помещении, 2 на улице), 2 вопроса, разметка COCO с атрибутами `direction` и `distance` (`annotations.json`), манифест вопросов (`questions.jsonl`) и эталонные описания (`descriptions.jsonl`). Разметка, манифест и описания написаны вручную для этого набора и распространяются под Apache-2.0, как код репозитория.

## Снимки

Все снимки взяты с Wikimedia Commons (проверено 2026-11-08), уменьшены до 640 пикселей по длинной стороне и пережаты в JPEG (Pillow, качество 78-80, без EXIF). Качество подобрано так, чтобы детектор на бэкенде `fake` выбирал для каждого файла нужную сцену: первые 8 байт SHA-256 файла (big-endian) по модулю 3, таблица сцен - в README `objects-detector`.

| Файл | Обстановка | Сцена `fake` | Источник | Автор | Лицензия |
|---|---|---|---|---|---|
| `images/img001.jpg` | `indoor`, лестница впереди | 0 - ступени впереди близко, предупреждение пассивного режима | [File:Indoor staircase.jpg](https://commons.wikimedia.org/wiki/File:Indoor_staircase.jpg) | MarkBuckawicki | [CC0 1.0](https://creativecommons.org/publicdomain/zero/1.0/) |
| `images/img002.jpg` | `indoor`, стол и стулья, людей нет | 1 - комната без опасностей, `passive.clear` | [File:Chairs in a meeting room (Unsplash).jpg](https://commons.wikimedia.org/wiki/File:Chairs_in_a_meeting_room_(Unsplash).jpg) | Breather (Unsplash) | [CC0 1.0](https://creativecommons.org/publicdomain/zero/1.0/) |
| `images/img003.jpg` | `indoor`, гостиная | 1 | [File:Living room in apartment of Condomínio do Edifício Zaher, Le Blond, Rio de Janeiro, Brazil.jpg](https://commons.wikimedia.org/wiki/File:Living_room_in_apartment_of_Condom%C3%ADnio_do_Edif%C3%ADcio_Zaher,_Le_Blond,_Rio_de_Janeiro,_Brazil.jpg) | Wilfredor | [CC0 1.0](https://creativecommons.org/publicdomain/zero/1.0/) |
| `images/img004.jpg` | `outdoor`, пешеходный переход со светофорами | 2 - улица | [File:The Brook pedestrian crossing - Chatham.jpg](https://commons.wikimedia.org/wiki/File:The_Brook_pedestrian_crossing_-_Chatham.jpg) | OathOn | [CC0 1.0](https://creativecommons.org/publicdomain/zero/1.0/) |
| `images/img005.jpg` | `outdoor`, улица с машинами | 2 | [File:Pedestrian crossing on Saint Anthony Street, intersection with Grada Street in Tomaszów Mazowiecki, Poland.jpg](https://commons.wikimedia.org/wiki/File:Pedestrian_crossing_on_Saint_Anthony_Street,_intersection_with_Grada_Street_in_Tomasz%C3%B3w_Mazowiecki,_Poland.jpg) | WrS.tm.pl | общественное достояние (Public domain) |

На `img005.jpg` вдали видны прохожие; лица неразличимы.

`testdata/llmbench/scene.jpg` - побайтная копия `images/img002.jpg` (CC0, людей в кадре нет); его использует живая проверка `deployments/llm-live-check.sh` для режима `vlm` (DEP-11).

## Вопросы

Записи - произношения слов из The Shtooka Project, опубликованные на Wikimedia Commons. Каждый вопрос склеен из двух слов с паузой 150 мс и переведён в WAV PCM 16 кГц моно 16 бит (`ffmpeg -ac 1 -ar 16000 -sample_fmt s16`).

| Файл | Текст | Условие | Источники | Автор | Лицензия |
|---|---|---|---|---|---|
| `questions/q001.wav` | где дверь | `quiet` | [File:Ru-где.ogg](https://commons.wikimedia.org/wiki/File:Ru-%D0%B3%D0%B4%D0%B5.ogg), [File:Ru-дверь.ogg](https://commons.wikimedia.org/wiki/File:Ru-%D0%B4%D0%B2%D0%B5%D1%80%D1%8C.ogg) | The Shtooka Project; «дверь» - Сергей Сахно, Vion Nicolas | [CC BY 2.0 FR](https://creativecommons.org/licenses/by/2.0/fr/), [CC BY 3.0 US](https://creativecommons.org/licenses/by/3.0/us/) |
| `questions/q002.wav` | где стул | `street_noise` | [File:Ru-где.ogg](https://commons.wikimedia.org/wiki/File:Ru-%D0%B3%D0%B4%D0%B5.ogg), [File:Ru-стул.ogg](https://commons.wikimedia.org/wiki/File:Ru-%D1%81%D1%82%D1%83%D0%BB.ogg) | The Shtooka Project | [CC BY 2.0 FR](https://creativecommons.org/licenses/by/2.0/fr/) |

Изменения: слова склеены; в `q002.wav` к речи добавлен белый шум с SNR 10 дБ (генератор `numpy.random.default_rng(2026)`), он изображает условие `street_noise`. Шум синтетический.
