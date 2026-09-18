# yd-adapter

Серверный адаптер «Яндекс.Диск ⇄ локальная очередь» для файлового транспорта echo-бота.
Язык — Go, только стандартная библиотека, `CGO_ENABLED=0` (статический бинарник, минимум RAM).

Телефон кладёт конверт задачей в `/echo/in/` на Диске, адаптер перекладывает его в
`/opt/echo-bot/queue` на сервере `<сервер>`; воркеры (Hermes) работают как раньше.
Результаты идут обратно: `queue/outbox` → `/echo/out/`. Конверты `kind: chat` адаптер
отвечает сам через OpenAI-совместимый LLM, мимо очереди.

**Статус:** реализация не начата. План (решения, фазы, риски) — в репе
`LazDeltaChatBot`: `docs/plans/2026-09-18-yandex-disk-transport.md`.

## Раскладка на Диске

```
/echo/in/         конверты задач от телефона (+ in/att/<msgid>/ — вложения)
/echo/out/        конверты ответов (+ out/att/<msgid>/)
/echo/archive/    in/ — обработано, out/ — забрано, broken/ — мусор
```

Имена файлов `20260918T150233Z-a1b2.json`: сортировка по имени = хронология.

## Структура

```
main.go                точка входа: цикл pull_in → push_out → reclaim → retention
ydisk/                 клиент Yandex Disk REST API
internal/fakedisk/     фейковый Disk API для тестов (httptest)
internal/bridge/       мосты Диск ⇄ очередь, state.json
internal/reclaim/      возврат просроченных claim'ов в inbox/
internal/retention/    чистка archive/ по RETENTION_DAYS
internal/chatllm/      ответы на kind: chat через LLM
docs/PROTOCOL.md       спецификация файлового канала (канон)
docs/token-notes.md    процедура и учёт OAuth-токенов (без самих токенов)
tests/fixtures/        фикстуры протокола
tools/yd_smoke.py      smoke-проверка Disk API (одноразовая)
deploy/                systemd-юнит
```

## Разработка

```
go build ./... && go vet ./...
go test ./...
```

## Запуск (после деплоя)

```
yd-adapter --check    # токен, папки, очередь — диагностика
yd-adapter --once     # один проход цикла
yd-adapter            # демон: POLL_INTERVAL=10 c
```

Конфиг — `.env` рядом с бинарником (см. `.env.example`): `YD_TOKEN`, `YD_ROOT`,
`QUEUE_DIR`, `POLL_INTERVAL`, `RETENTION_DAYS`, `CLAIM_TIMEOUT`, `STATE_FILE`, `LLM_*`.

## Откат на почтовый канал

```
ssh ubuntu@<сервер> 'sudo systemctl stop yd-adapter && sudo systemctl start echo-bot'
ssh ubuntu@<сервер> 'sudo systemctl stop echo-bot && sudo systemctl start yd-adapter'
```

Одновременно `echo-bot` и `yd-adapter` работать не должны — оба пишут в `queue/`.
