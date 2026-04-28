# Architecture

## Зачем мы переехали

**Старая архитектура** (single backend monolith):
- Бэкенд сам ходил во все источники: TON liteserver, DHT, ADNL к провайдерам, ifconfig.
- Все циклы (`scan_master`, `probe`, `proof`, …) запускались через внутренний
  scheduler `pkg/workers/providersMaster`.
- Бэкенд писал результаты в Postgres напрямую.

**Проблемы**:
- Невозможно масштабировать сетевые операции горизонтально.
- Сложно добавить второй экземпляр бэкенда: гонки на запись и двойные scan'ы.
- ADNL/DHT-зависимости утяжеляют бэкенд (большой бинарь, длинная цепочка
  подключений на старте).
- Сетевые проблемы у одного цикла блокируют scheduler других.

**Новая архитектура** (Redis Streams + stateless agent):
- Бэкенд публикует «триггеры» в Redis, ничего не делает в сети сам.
- Агенты подписываются на стримы через consumer-group, выполняют циклы,
  публикуют агрегированные результаты.
- Бэкенд consume'ит результаты, применяет в Postgres транзакционно с дедупом.

## Распределение ответственности

| Компонент    | Делает                                                          | Не делает                                  |
|--------------|-----------------------------------------------------------------|--------------------------------------------|
| **Backend**  | пишет в Postgres, держит расписание, считает SQL-агрегаты       | ADNL/DHT/TON network — ничего              |
| **Agent**    | TON liteserver, ADNL, DHT, ifconfig HTTP, оркестрация цикла     | пишет в Postgres только LT-чекпоинты      |
| **Postgres** | source of truth для providers/contracts/relations/endpoints/... | —                                          |
| **Redis**    | message bus (cycle-streams + result-streams)                    | persistent state — нет, MAXLEN'ится       |

## Data flow

```
┌──────────────────────────────────────────────────────────────────┐
│ Backend                                                          │
│                                                                  │
│  cron Ticker ──┐                                                 │
│                ▼                                                 │
│         dispatcher.maybeTrigger(cycle)                           │
│                │                                                 │
│                ▼ XADD (mtpa:cycle:<type>)                        │
└────────────────│─────────────────────────────────────────────────┘
                 │
                 ▼
            ┌─────────┐
            │  Redis  │
            └────┬────┘
                 │ XREADGROUP (consumer-group "mtpa")
                 ▼
┌──────────────────────────────────────────────────────────────────┐
│ Agent                                                            │
│                                                                  │
│  redis-consumer ──▶ usecase.<cycle>(ctx)                         │
│                          │                                       │
│                          ├─ read Postgres (read-only)            │
│                          ├─ ADNL/DHT/TON/HTTP I/O (orchestrate)  │
│                          ├─ aggregate result                     │
│                          ├─ publisher.PublishResult(...)         │
│                          └─ write LT to Postgres (checkpoint)    │
│                                                                  │
│  redis-consumer ──▶ XACK                                         │
└──────────────────────────────────────────────────────────────────┘
                 │
                 ▼ XADD (mtpa:result:<type>)
            ┌─────────┐
            │  Redis  │
            └────┬────┘
                 │ XREADGROUP (consumer-group "mtpa-backend")
                 ▼
┌──────────────────────────────────────────────────────────────────┐
│ Backend                                                          │
│                                                                  │
│  redis-consumer ──▶ BEGIN tx                                     │
│                     │                                            │
│                     ├─ INSERT INTO system.processed_jobs         │
│                     │  ON CONFLICT DO NOTHING ──▶ если 0 rows:   │
│                     │                              COMMIT (skip) │
│                     │                                            │
│                     ├─ handler(tx, env): apply writes through    │
│                     │  providers.WithTx(tx).<repo method>        │
│                     │                                            │
│                     ├─ COMMIT                                    │
│                     └─ XACK                                      │
└──────────────────────────────────────────────────────────────────┘
```

## Ключи Redis

| Ключ                          | Кто пишет | Кто читает  | Тип        |
|-------------------------------|-----------|-------------|------------|
| `mtpa:cycle:<type>`           | backend   | agent       | XADD/XREADGROUP |
| `mtpa:result:<type>`          | agent     | backend     | XADD/XREADGROUP |

`<type>` ∈ `{scan_master, scan_wallets, resolve_endpoints, probe_rates, inspect_contracts, check_proofs, lookup_ipinfo}`.

Стрим-префикс настраивается (`mtpa` по умолчанию) и должен совпадать у обоих
сервисов.

Consumer-groups:
- `mtpa` — на cycle-стримах (читает агент). Все агенты в фуит-кластере живут
  в одной группе → каждое сообщение получает ровно один агент.
- `mtpa-backend` — на result-стримах (читает бэкенд).

## Циклы

| Cycle ID            | Что делает                                                    | Триггер интервал по умолч. | LT-write |
|---------------------|---------------------------------------------------------------|----------------------------|----------|
| `scan_master`       | сканирует master-кошелёк, находит новых провайдеров           | 5m                         | ✓ master |
| `scan_wallets`      | для каждого provider-кошелька — найти новые storage-контракты | 30m                        | ✓ per-wallet |
| `resolve_endpoints` | DHT-резолв IP/port провайдеров                                | 30m                        | —        |
| `probe_rates`       | ADNL `GetStorageRates` + статус online                        | 1m                         | —        |
| `inspect_contracts` | on-chain `GetProvidersInfo`, найти rejected relations         | 30m                        | —        |
| `check_proofs`      | RLDP storage-proof: GetTorrentInfo + GetPiece + CheckProof    | 60m                        | —        |
| `lookup_ipinfo`     | HTTP ifconfig для гео-данных                                  | 4h                         | —        |

LT-write — единственное место, где агент пишет в Postgres (плюс эти строки):
- `system.params.masterWalletLastLT` — после `scan_master`;
- `providers.providers.last_tx_lt` — после `scan_wallets` (по каждому
  пройденному кошельку).

Это сделано для **консистентности**: следующий scan_master/scan_wallets должен
стартовать с того LT, до которого реально дошли, без зависимости от того,
успел ли бэкенд обработать result-stream.

## Идемпотентность

Каждое триггер-сообщение получает UUID `job_id`, который пробрасывается
через result-envelope обратно в бэкенд. Бэкенд при apply'е делает в одной
транзакции:

```sql
BEGIN;
  INSERT INTO system.processed_jobs (job_id, type, agent_id) VALUES (...)
    ON CONFLICT (job_id) DO NOTHING;
  -- если 0 rows affected → дубль, COMMIT пустой
  -- иначе: handler.apply(tx, result)
COMMIT;
```

`XACK` делается **только после успешного COMMIT**. Если backend крашнулся
до коммита → транзакция роллбэкнулась → сообщение остаётся в PEL → после
рестарта переедет, попадёт в дедуп либо применится впервые.

## Гарантии

| Сценарий | Что защищает |
|---|---|
| Агент крашнулся в середине работы | XPENDING держит сообщение → reaper или backend через `XAUTOCLAIM` отдаст другому consumer'у; идемпотентные UPSERT'ы |
| Агент XACK не успел отправить | Re-delivery; `processed_jobs` ловит дубль |
| Бэкенд крашнулся между XREAD и COMMIT | TX откатилась; result re-read после рестарта |
| Бэкенд XACK не успел отправить после COMMIT | Re-delivery; `processed_jobs` ловит дубль |
| Two backend triggers same cycle | Distributed lock + `single_inflight: true` flag (XPENDING-check) |
| Out-of-order results (cycle B finished before A) | LT-writes используют `WHERE current < incoming`; остальные writes — last-event-time-wins |

## Что НЕ покрыто текущей реализацией (известные TODOs)

- **`XAUTOCLAIM` reaper** — не реализован. Если агент крашнулся, его сообщения
  висят в PEL до ручного reclaim. Можно добавить отдельным cron-job'ом или
  запускать в самом агенте при старте на старых консумер'ах.
- **OTel trace propagation** — `trace_parent` не пробрасывается через envelope.
- **HTTP `/healthz`** — на агенте есть только `/metrics`, отдельный health-check
  endpoint не предусмотрен.
- **Multi-instance backend** — потребует distributed lock (Redis SETNX) в
  диспетчере, чтобы не плодить дубли триггеров. Сейчас рассчитано на 1
  бэкенд-инстанс.
- **Bag-prover session reuse** — `directprovider.Verify` пересоздаёт ADNL peer
  для каждого bag'а. Можно ввести batch-job `verify_bags_batch` для одного
  провайдера, чтобы переиспользовать RLDP-сессию.

## Trade-offs (записано чтобы не забыть)

- **Агент имеет read-only доступ к Postgres**. Альтернатива (push всех данных
  в trigger payload) была бы чище концептуально, но сильно раздула бы
  размер сообщений (probe_rates по 1000 pubkey'ев → ~50KB на трик'ер). Поэтому
  оставили чтение из БД.
- **LT-writes на стороне агента** — нарушает принцип «один writer». Но
  альтернатива (LT через result-stream → backend writes) приводит к проблеме
  когда backend не успел обработать → следующий scan_master стартует с того
  же `from_lt` и переcканирует уже найденное. UPSERT идемпотентен, но трафик
  лишний. LT-write на агенте срабатывает СРАЗУ после успешного scan'а.
- **`single_inflight` опционально**. По умолчанию `true` для всех циклов —
  защищает от гонок. Можно выключить для idle-циклов, где гонки безвредны.

См. [decisions](#decisions-в-обратном-порядке) внизу для исторического контекста
этих решений.

## Decisions (в обратном порядке)

1. **2026-04-26**: cycle-конфиг сгруппирован per-usecase в агенте (4 секции:
   `discovery/poll/proof/update`), а не per-type (7 секций). Проще
   конфигурировать; теряется per-cycle гранулярность параллелизма, но это
   приемлемо для текущего масштаба.
2. **2026-04-26**: agent и backend оба имеют доступ к Postgres (с разными
   правами). Альтернатива «pure Redis push» отвергнута из-за раздувания
   payload'ов.
3. **2026-04-25**: оркестрация осталась в юзкейсах агента (`for _, p := range
   pubkeys { probe }`). Альтернатива «1 message = 1 atomic op» отвергнута:
   слишком много мелких сообщений, трудно дедуплить, неудобно агрегировать.
4. **2026-04-25**: Redis Streams (а не RabbitMQ/Kafka). Простой
   деплой (1 контейнер), есть consumer-group + at-least-once + PEL +
   XAUTOCLAIM — достаточно для текущих требований.
5. **2026-04-24**: `cleanenv` на обоих сервисах — совместимо со старым
   бэкенд-кодом, минимальная зависимость.
