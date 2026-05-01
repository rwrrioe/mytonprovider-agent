Независимый набор воркеров, которые ищут и проверяют storage-providers в сети TON.  Scheduling и оркестрация проводится внешним сервисом, в нашем случае бэкендом. 
## Разделение сервисов

- **`mytonprovider-backend`** — единственный writer Postgres + cron-планировщик
  «триггеров» циклов. Публикует команды в Redis, читает результаты, пишет их
  в БД транзакционно с дедупом.
- **`mytonprovider-agent`** — fleet stateless-воркеров, которые выполняют
  тяжёлые сетевые операции (TON liteserver, ADNL, DHT, ifconfig). Получают
  задачи из Redis-стримов, читают данные из Postgres (read-only),
  выполняют циклы, публикуют агрегированные результаты обратно в Redis.

## Как читать документацию

| # | Документ | Кому | Что внутри |
|---|----------|------|------------|
| 1 | [architecture.md](01-architecture.md) | всем | архитектурный обзор, data-flow, обоснования |
| 2 | [data-contracts.md](02-data-contracts.md) | разработчикам обоих сервисов | JSON-контракты trigger/result envelope, per-cycle payload'ы |
| 3 | [agent.md](03-agent.md) | разработчикам агента | walkthrough кода: ports, usecases, adapters, конфиг |
| 4 | [backend.md](04-backend.md) | разработчикам бэкенда | walkthrough кода: dispatcher, consumer, handlers, idempotency |
| 5 | [deployment.md](05-deployment.md) | DevOps | docker-compose, сетевые модели, миграции |
| 6 | [operations.md](06-operations.md) | дежурным/SRE | verification, troubleshooting, scaling, multi-agent |
| 7 | [changelog.md](07-changelog.md) | всем | что и почему изменилось (timeline) |

## TL;DR за 60 секунд

```
            triggers (cron)
   Backend ───────────────▶ Redis Streams (mtpa:cycle:<type>) ───▶ Agent(s)
       ▲                                                              │
       │                                                              │ orchestrate cycle
       │                                                              │ (read DB, do net I/O)
       │                                                              │
       │              results                                         ▼
   Backend ◀───────────────  Redis Streams (mtpa:result:<type>) ◀── Agent(s)
       │
       │ apply tx (dedup via system.processed_jobs)
       ▼
   Postgres
```

- 7 типов циклов: `scan_master`, `scan_wallets`, `resolve_endpoints`,
  `probe_rates`, `inspect_contracts`, `check_proofs`, `lookup_ipinfo`.
- Идемпотентность гарантирована таблицей `system.processed_jobs` (PK по `job_id`).
- Backend — единственный writer основных таблиц; агент пишет только LT-чекпоинты
  (`system.params.masterWalletLastLT` и `providers.providers.last_tx_lt`).
- Агент масштабируется горизонтально через consumer-group (`mtpa`).
- Каждый цикл может включаться/выключаться независимо в обоих сервисах.

## Быстрый запуск

См. [deployment.md § Локальный запуск](05-deployment.md#локальный-запуск-с-docker-compose).
