# yd-adapter

Серверный адаптер «Яндекс.Диск ⇄ локальная очередь» для файлового транспорта echo-бота.
Язык — Go, только стандартная библиотека, `CGO_ENABLED=0` (статический бинарник, минимум RAM).

Телефон кладёт конверт задачей в `/echo/in/` на Диске, адаптер перекладывает его в
`/opt/echo-bot/queue` на сервере `<сервер>`; воркеры (Hermes) работают как раньше.
Результаты идут обратно: `queue/outbox` → `/echo/out/`. Конверты `kind: chat` адаптер
отвечает сам через OpenAI-совместимый LLM, мимо очереди.

**Статус:** реализовано и задеплоено на `<сервер>` (2026-09-21). Сквозной путь
проверен с реального телефона: задача агенту дошла до воркера и вернулась с вложениями,
chat-конверт отвечен LLM. План (решения, фазы, риски) — в репе `LazDeltaChatBot`:
`docs/plans/2026-09-18-yandex-disk-transport.md`.

## Деплой на сервере

```
/opt/echo-bot/yd-adapter/yd-adapter      статический бинарник (amd64)
/opt/echo-bot/yd-adapter/.env            YD_TOKEN, QUEUE_DIR, LLM_* (chmod 600)
/etc/systemd/system/yd-adapter.service   юнит (User=ubuntu, Restart=always)
```

```
# состояние и логи
sudo systemctl status yd-adapter
sudo journalctl -u yd-adapter -f

# диагностика (токен, папки, очередь, взаимомыключение с echo-bot)
/opt/echo-bot/yd-adapter/yd-adapter --check --env /opt/echo-bot/yd-adapter/.env
/opt/echo-bot/yd-adapter/yd-adapter --dry-run    # показать состояние и выйти
```

Пересборка и заливка (SCP не используем — pipe):

```
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/yd-adapter .
cat /tmp/yd-adapter | ssh ubuntu@<сервер> \
  'sudo systemctl stop yd-adapter && cat > /opt/echo-bot/yd-adapter/yd-adapter \
   && chmod 755 /opt/echo-bot/yd-adapter/yd-adapter && sudo systemctl start yd-adapter'
```

Правило: **`yd-adapter` и `echo-bot` одновременно работать не должны** — оба пишут в
`QUEUE_DIR` (задачи выполнятся дважды). `--check` предупреждает, если `echo-bot` активен.

## Откат на почтовый канал

```
ssh ubuntu@<сервер> 'sudo systemctl stop yd-adapter && sudo systemctl start echo-bot'
ssh ubuntu@<сервер> 'sudo systemctl stop echo-bot && sudo systemctl start yd-adapter'
```

Бэкап перед первым деплоем: `/opt/echo-bot/backup-pre-yd-adapter-*.tar.gz` (`.env` + очередь).

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

