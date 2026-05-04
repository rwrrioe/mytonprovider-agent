# Changelog

Хронология изменений в этой переделке. Полезно тем, кто пропустил итерации
обсуждения и хочет понять, как пришли к текущему состоянию.

## Этап 1: рефакторинг репозиториев агента (до пивота)

**Что было**: `usecases` использовали старые DB-типы (`db.X`), не было
единого error-pattern'а.

**Что сделано**:
- Все методы `internal/adapters/outbound/postgres/*.go` приведены к виду
  `const op = "postgres.<Repo>.<Method>"` + `fmt.Errorf("%s:%w", op, err)`.
- Заменили старые `db.X` на доменные сущности (`domain.Provider`,
  `domain.StorageContract`, `domain.ContractProviderRelation` и т.д.).
- Закомментированные методы из старого backend'а **оставлены без изменений**
  (по запросу пользователя — как библиотека SQL'я на будущее).

## Этап 2: написание адаптеров (TON, DHT, directprovider, ipconfig)

Изначально `internal/usecase/discovery|poll|proof|update/ports.go`
объявляли интерфейсы, но реализаций не было. Пришлось:

- **`outbound/ton/liteclient/`** — пул liteservers (`ConnectionPool`),
  методы `GetTransactions`, `GetStorageContractsInfo`, `GetProvidersInfo`.
- **`outbound/ton/master_scanner.go`** — `Discovery.MasterScanner`. Парсит
  TX-комментарии master-кошелька на `tsp-<pubkey>`.
- **`outbound/ton/scanner.go`** — `Discovery.WalletScanner`. Парсит TX
  provider-кошелька на storage-reward-withdrawal (op `0xa91baf56`).
- **`outbound/ton/inspector.go`** — `Poll.ContractInspector` /
  `Proof.ContractInspector`. On-chain `GetProvidersInfo`.
- **`outbound/dht/dht.go`** — `EndpointResolver.Resolve` +
  `ResolveStorageWithOverlay`. DHT-резолв через `tonutils-go/adnl/dht` +
  `tonutils-storage-provider/pkg/transport`.
- **`outbound/directprovider/client.go`** — `RatesProbe.Probe` (ADNL
  `GetStorageRates`) + `BagProver.Verify` (RLDP-сессия + GetTorrentInfo +
  GetPiece + Merkle proof check).
- **`outbound/ipconfig/client.go`** — `IPInfoClient.GetIPInfo` (HTTP
  ifconfig.io).

## Этап 3: первая версия юзкейсов (orchestration + DB writes)

Каждый usecase читал из БД, делал loop'и через сетевые адаптеры,
писал результаты в БД. Сигнатуры `(ctx) (interval time.Duration, err error)`.

Сделано:
- `discovery.CollectNewProviders/CollectStorageContracts/ResolveEndpoints`.
- `poll.UpdateProviderRates/UpdateRejectedContracts`.
- `proof.CheckStorageProofs`.
- `update.UpdateRating/UpdateUptime/UpdateIPInfo`.

Это работало как «standalone agent», но не отвечало финальным требованиям.

## Этап 4: попытка спланировать main.go + cleanup

В этот момент был сделан план для главной точки входа, конфига, scheduler'а и
docker'а. Старая модель: агент сам себя расписывает на интервалы, пишет
напрямую в свою (или общую) БД. Включено в утверждённый план; начали писать
конфиг.

## Этап 5: пивот на Redis Streams (по запросу пользователя)

**Решение**: агент перестаёт быть self-scheduled, становится воркером
по триггерам из Redis. Бэкенд — единственный owner расписания.

**Pivot 1** (рассматривался): pure Redis push — все данные в trigger payload,
агент без доступа к БД. Отвергнут: размер сообщений раздувается (probe_rates
по 1000 пабов = ~50KB/триггер).

**Pivot 2** (рассматривался): чисто HTTP API на бэкенде, агент через REST.
Отвергнут: ещё один канал, лишняя инфраструктура.

**Pivot 3 (выбран)**: hybrid — агент имеет read-only доступ к БД для
оркестрации, пишет только LT-чекпоинты. Все остальные результаты идут
через result-stream к бэкенду, который применяет их транзакционно с дедупом.

**Идемпотентность**: `system.processed_jobs(job_id PK)` + транзакция
[`MarkProcessedTx` + handler] на бэкенде, XACK только после COMMIT.

## Этап 6: реализация на стороне агента

- **`internal/jobs/types.go`** — JSON envelope schemas (TriggerEnvelope,
  ResultEnvelope, 7 per-cycle result структур).
- **`internal/lib/config/config.go`** — cleanenv-loader. Группировка
  `Workers` per-usecase (4 группы: discovery/poll/proof/update).
- **`internal/adapters/inbound/redisstream/`** — Consumer (XREADGROUP пул),
  EnsureGroup.
- **`internal/adapters/outbound/redisstream/publisher.go`** — XADD result-stream'ов.
- **Postgres-репозитории урезаны** до read-only (+ LT-write):
  - `provider.go`: оставлены `GetAllPubkeys`, `GetAllWallets`, `UpdateLT`;
    удалены `Create`, `UpdateRates`.
  - `contract.go`: оставлен `GetActiveRelations`; удалены
    `CreateContracts`, `CreateRelations`, `MarkRejected`, `SaveProofChecks`.
  - `endpoint.go`: оставлены `LoadFresh`, `LoadAll`; удалён `Upsert`.
  - `ipinfo.go`: оставлен `GetProvidersIPs`; удалён `SaveIPInfo`.
  - `system.go`: оставлен полный (LT read+write).
  - `status.go`: удалён целиком.
- **Юзкейсы переписаны**:
  - Сигнатуры → `func(ctx) error` (interval больше не нужен).
  - Loop'и и оркестрация остались.
  - Удалены DB-writes для не-LT данных, добавлены `publisher.PublishResult`
    в конце каждого метода.
  - LT-writes (`SetMasterWalletLT`, `UpdateLT`) сохранены.
- **`pkg/metrics/metrics.go`** — prometheus + `/metrics` endpoint.
- **`cmd/app/main.go`** — bootstrap: ADNL + Postgres + Redis + repos +
  адаптеры + юзкейсы + 7 consumer'ов.
- **`cmd/loadtest/main.go`** — утилита тестовой нагрузки.

Удалено:
- `internal/adapters/inbound/worker/` (старый generic-scheduler).

## Этап 7: реализация на стороне бэкенда

- **`pkg/jobs/types.go`** — зеркало агентских envelope'ов + `ToDB()` хелперы
  (`ProviderInfo.ToDB()` → `db.ProviderCreate`, и т.д.).
- **`pkg/redisstream/`**:
  - `publisher.go` — `Publisher.Trigger(cycleType, hint)` с UUID job_id.
  - `consumer.go` — `Consumer.Run`, транзакционный apply через
    `applyTx(env)`: BEGIN → `MarkProcessedTx` (dedup) → handler → COMMIT
    → XACK.
  - `groups.go` — `EnsureGroup`.
- **`pkg/dispatcher/dispatcher.go`** — cron-планировщик. Каждому циклу
  отдельный `time.Ticker`. `single_inflight: true` — проверяет `XPENDING`
  + `XInfoGroups.Lag` перед XADD, чтобы не плодить параллельные триггеры.
  Гасит NOGROUP/no-such-key как «нет in-flight» (на bootstrap'е до
  подключения первого агента).
- **`pkg/handlers/handlers.go`** — 7 типизированных обработчиков. Каждый
  декодирует payload, вызывает существующие репо-методы через
  `repo.WithTx(tx)`.
- **`pkg/repositories/dbtx.go`** — общий `DBTX` интерфейс, реализованный
  и `*pgxpool.Pool`, и `pgx.Tx`. Позволяет одному набору репо-методов
  работать как в pool-режиме, так и внутри tx.
- **`pkg/repositories/system/repository.go`**: добавлены `WithTx` +
  `MarkProcessedTx`. metrics middleware прозрачен относительно tx.
- **`pkg/repositories/providers/repository.go`**: поле `db` сменилось на
  `DBTX`, добавлен `WithTx`. metrics middleware пропускает `WithTx`.
- **`db/000002_processed_jobs.up.sql/down.sql`** — миграция.
- **`pkg/config/config.go`** — переписан:
  - убраны `System.ADNLPort`, `System.Key`, `TON.ConfigURL`, `TON.BatchSize`.
  - добавлены `Redis` и `Cycles` секции.
- **`cmd/main.go`** — переписан: убран `providersMaster`, добавлен
  dispatcher + result-consumers. `runAggregates` (uptime/rating SQL)
  оставлен как отдельная goroutine раз в 5 минут.
- **`cmd/init.go`** — удалён `newProviderClient` (ADNL/DHT инициализация
  больше не нужна).

Удалено:
- `pkg/workers/providersMaster/` — целиком.
- `pkg/clients/ton/` — целиком.
- `pkg/clients/ifconfig/` — целиком.
- Зависимости `tonutils-go`, `tonutils-storage`, `tonutils-storage-provider`
  через `go mod tidy`.

## Этап 8: интеграционный тест в Docker

Поднял оба сервиса в разных compose-проектах, через external-сеть `mtpa_bridge`:
- `docker network create mtpa_bridge`.
- Backend's redis и db подключены к `bridge_net` (доступны агенту).
- Backend's app подключён только к private network `backend` (агент его НЕ
  видит).
- Agent — отдельный compose, только в `bridge_net`.

**Найденные баги и фиксы**:

1. **Bootstrap NOGROUP**: первый XADD триггера падал с "NOGROUP" в
   `dispatcher.maybeTrigger.hasInflight`, потому что cycle-стрим/группу
   создаёт агент при первом XREADGROUP. Фикс: `isNoGroup`/`isNoStream`
   в `dispatcher.go` возвращают `false` (нет in-flight).

2. **`AddStatuses` падал с "ON CONFLICT cannot affect row a second time"**:
   корневая причина — `domain.ProviderStatus` в агенте сериализовался без
   JSON-тегов (PascalCase: `"PublicKey"`), а бэкенд ожидал snake_case
   (`"public_key"`). На бэкенде `jobs.ProviderStatus.ToDB()` получал
   пустой `Pubkey` для всех 30 элементов → batch с одинаковым lower("")
   → ON CONFLICT collision.
   Фикс: добавил JSON-теги во все типы `internal/domain/{provider,contract}.go`.

3. **`job_id=""` в результатах**: агент не пробрасывал JobID из триггера в
   результат — все результаты имели пустой `job_id`, что сводило дедуп
   к одному ключу. Фикс: добавил `jobs.WithJobID(ctx, jobID)` в
   `redisstream/consumer.go`. Все usecase методы теперь делают
   `jobs.JobIDFrom(ctx)` при публикации.

После фиксов end-to-end полностью работает: 10 циклов `probe_rates` через
`loadtest` дали `ok=10 error=0 missing=0`.

## Этап 9: документация (этот каталог)

- README.md — entry + ToC.
- 01-architecture.md — high-level + data flow + decisions.
- 02-data-contracts.md — JSON-схемы.
- 03-agent.md — code walkthrough агента.
- 04-backend.md — code walkthrough бэкенда.
- 05-deployment.md — docker-compose + production.
- 06-operations.md — verify, troubleshoot, scale.
- 07-changelog.md — этот документ.

## Этап 11: запуск metrics HTTP-сервера

`metrics.Serve()` не вызывалась — порт `:2112` не открывался, метрики
накапливались в памяти, но не были доступны снаружи.

Исправлено в `internal/app/app.go` (`MustRun`): после запуска consumer'ов,
если `metrics.enabled: true`, стартует горутина `metrics.Serve(ctx, port, logger)`.
При `metrics.enabled: false` — в лог пишется `"metrics disabled, skipping"`.

Теперь `curl localhost:2112/metrics` работает.

## Этап 10: XAUTOCLAIM reaper

Добавлен `internal/adapters/inbound/redisstream/reaper.go`.

- `Consumer.Run` теперь запускает фоновую горутину `runReaper` наряду
  с пулом воркеров.
- Reaper тикает каждые `ReaperTimeout`, вызывает `XAUTOCLAIM` с пагинацией
  (курсор до `"0-0"`), передаёт reclaimed-сообщения в `process()`.
- Исправлен `NewConsumer`: поля `midIdle` и `reaperTimeout` теперь
  корректно инициализируются из `ConsumerConfig`.
- Параметры: `min_idle` и `reaper_timeout` в конфиге воркера.

## Что НЕ сделано (намеренно или отложено)

- **Multi-instance backend support** (требует distributed lock в dispatcher).
- **HTTP `/healthz`** на агенте (есть только `/metrics`).
- **OTel trace propagation** через envelope.
- **Тесты** — ни юнит, ни интеграционных. Smoke только через `loadtest`.
- **CI/CD pipeline**.
- **`verify_bags_batch`** — оптимизация proof-цикла (1 connection на провайдера
  вместо N).
- **Cleanup `system.processed_jobs`** старше 30 дней — нужен cron-job.

## Зависимости, которые добавились / убрали

**Agent (`go.mod`)**:
- `+` `github.com/redis/go-redis/v9`
- `+` `github.com/ilyakaznacheev/cleanenv`
- `+` `github.com/prometheus/client_golang`
- `+` `github.com/google/uuid`
- `+` `github.com/xssnick/tonutils-storage` (для `storage.TorrentInfoContainer`
  / `GetTorrentInfo` / `Piece` в directprovider).

**Backend (`go.mod`)**:
- `+` `github.com/redis/go-redis/v9`
- `−` `github.com/xssnick/tonutils-go`
- `−` `github.com/xssnick/tonutils-storage`
- `−` `github.com/xssnick/tonutils-storage-provider`

## Полный список новых файлов

**Agent**:
- `internal/jobs/types.go`
- `internal/lib/config/config.go`
- `internal/adapters/inbound/redisstream/{consumer,groups,reaper}.go`
- `internal/adapters/outbound/redisstream/publisher.go`
- `internal/adapters/outbound/dht/dht.go`
- `internal/adapters/outbound/directprovider/client.go`
- `internal/adapters/outbound/ipconfig/client.go`
- `internal/adapters/outbound/ton/{master_scanner,scanner,inspector}.go`
- `internal/adapters/outbound/ton/liteclient/client.go`
- `pkg/metrics/metrics.go`
- `cmd/app/main.go`
- `cmd/loadtest/main.go`
- `config/config.example.yaml`
- `docs/*.md` (этот каталог)

**Backend**:
- `pkg/jobs/types.go`
- `pkg/redisstream/{publisher,consumer,groups}.go`
- `pkg/dispatcher/dispatcher.go`
- `pkg/handlers/handlers.go`
- `pkg/repositories/dbtx.go`
- `db/000002_processed_jobs.{up,down}.sql`
- `SETUP.md`

## Полный список удалённых файлов

**Backend**:
- `pkg/workers/providersMaster/` (вся папка).
- `pkg/clients/ton/` (вся папка).
- `pkg/clients/ifconfig/` (вся папка).

**Agent**:
- `internal/adapters/inbound/worker/` (вся папка — заменена `redisstream/`).
