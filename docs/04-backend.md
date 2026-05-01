# Backend: code walkthrough

Бэкенд — единственный writer Postgres. Делает три вещи:
1. **Dispatcher** — раз в N минут публикует триггеры в `mtpa:cycle:<type>`.
2. **Result consumer** — читает `mtpa:result:<type>`, применяет агрегаты в БД
   транзакционно с дедупом по `job_id`.
3. **Local workers** — telemetry/benchmarks (приходят от провайдеров через HTTP),
   cleaner (чистка истории), aggregates (uptime/rating через SQL).

## Структура каталогов

```
mytonprovider-backend/
├── cmd/
│   ├── main.go              # bootstrap
│   └── init.go              # connectPostgres + newPostgresConfig
├── config/
│   └── dev.yaml             # пример конфига
├── db/
│   ├── 000001_init.up.sql           # исходная схема
│   ├── 000002_processed_jobs.up.sql # NEW: dedup-таблица
│   └── 000002_processed_jobs.down.sql
├── pkg/
│   ├── cache/                # in-mem cache для telemetry/benchmarks
│   ├── config/config.go      # Config struct + cleanenv
│   ├── constants/            # ReasonCode + sort/order constants
│   ├── dispatcher/           # NEW: cron-планировщик триггеров
│   ├── handlers/             # NEW: per-cycle result handlers
│   ├── httpServer/           # API для UI/клиентов
│   ├── jobs/                 # NEW: JSON envelope schemas
│   ├── metrics/              # prometheus business metrics
│   ├── models/
│   │   ├── api/v1/types.go
│   │   ├── apperror.go
│   │   └── db/types.go       # ProviderCreate, ProviderWalletLT, StorageContract...
│   ├── redisstream/          # NEW: publisher + consumer + groups
│   ├── repositories/
│   │   ├── dbtx.go           # NEW: общий DBTX интерфейс (pool | tx)
│   │   ├── providers/        # большой repo с UPSERT'ами
│   │   └── system/           # system.params + processed_jobs
│   ├── services/providers/   # бизнес-логика API (фильтры, агрегации)
│   ├── utils/
│   └── workers/
│       ├── workers.go        # generic scheduler для local workers
│       ├── cleaner/          # archive history > N days
│       └── telemetry/        # collect telemetry from providers via HTTP
└── docker-compose.yml        # postgres + redis + db_migrate + app
```

## Backend consumer-scheduler 

Общая структура представляет собой cron-scheduler который раз в N секунд вызывает XADD и добавляет новые jobs в редис стрим статусов. Отдельная сущность consumer читает результаты из стрима результатов и пишет в БД.

![[Scheduler-Consumer.png]]


Scheduler представлен объектом dispatcher, который после тика раз в N секунд вызывает publisher (обертка над XADD):

![[Pasted image 20260501095124.png]]

Consumer читает стрим и запускает пул воркеров. Каждый воркер  запускает транзакци из двух частей: дедупликация по jobs и сохранение результатов агента в БД. Process делает дедупликацию по jobs и вызывает хендлер. Хендлер - обертка над конкретным методом репозитория, который просто сохраняет данные уже после дедупликации.

![[Consumer-backend.png]]

Более подробно о структурах Consumer-Dispatcher ниже.
## Поток управления

### 1. Старт ([`cmd/main.go`](../../mytonprovider-backend/cmd/main.go))

```go
func run() (err error) {
    cfg := config.MustLoadConfig()
    logger := slog.New(slog.NewJSONHandler(os.Stdout, ...))

    // Postgres
    connPool, _ := connectPostgres(ctx, cfg, logger)

    // Repositories
    providersRepo := providers.NewRepository(connPool)
    providersRepo  = providers.NewMetrics(...)
    systemRepo    := system.NewRepository(connPool)
    systemRepo     = system.NewMetrics(...)

    // Redis
    rdb := redis.NewClient(...)
    publisher := redisstream.NewPublisher(rdb, prefix, triggerMaxLen)

    // Per-cycle bindings: schedule + handler
    cycleSet := handlers.NewSet(logger, providersRepo, systemRepo)

    // EnsureGroup на каждом result-stream + spawn consumer
    for _, b := range bindings {
        if !b.schedule.Enabled { continue }
        redisstream.EnsureGroup(ctx, rdb, "mtpa:result:"+b.type, "mtpa-backend")
        consumer := redisstream.NewConsumer(rdb, connPool, systemRepo,
            ConsumerConfig{Stream, Group, ConsumerID="backend", CycleType, Parallel, BlockMs},
            cycleSet.Handler(b.type), logger)
        go consumer.Run(ctx)
    }

    // Dispatcher (cron triggers)
    disp := dispatcher.New(rdb, publisher, "mtpa-backend", schedules, logger)
    go disp.Run(ctx)

    // Local workers (telemetry, cleaner)
    localWorkers := workers.NewWorkers(telemetryWorker, cleanerWorker, logger)
    go localWorkers.Start(ctx)

    // Aggregates (uptime/rating SQL caches)
    go runAggregates(ctx, providersRepo, logger)

    // HTTP API
    server := httpServer.New(...)
    go app.Listen(":" + cfg.System.Port)

    <-signalChan; cancel(); ...
}
```

### 2. Dispatcher

Файл: [`pkg/dispatcher/dispatcher.go`](../../mytonprovider-backend/pkg/dispatcher/dispatcher.go).

```go
type CycleSchedule struct {
    CycleType      string
    Enabled        bool
    Interval       time.Duration
    SingleInflight bool   // если true — пропускать триггер если предыдущий
                          // ещё не отработан агентом (XPENDING/Lag check)
}

func (d *Dispatcher) Run(ctx) {
    for _, s := range schedules {
        if !s.Enabled { continue }
        go d.runCycle(ctx, s)  // отдельный Ticker на каждый тип
    }
}

func (d *Dispatcher) runCycle(ctx, s) {
    d.maybeTrigger(ctx, log, s)  // первый раз сразу
    t := time.NewTicker(s.Interval)
    for {
        <-t.C
        d.maybeTrigger(ctx, log, s)
    }
}

func (d *Dispatcher) maybeTrigger(ctx, log, s) {
    if s.SingleInflight {
        busy := d.hasInflight(ctx, "mtpa:cycle:"+s.CycleType)
        if busy { return }
    }
    d.publisher.Trigger(ctx, s.CycleType, nil)  // XADD с UUID job_id
}

func (d *Dispatcher) hasInflight(ctx, stream) (bool, error) {
    // XPENDING (PEL count) + XInfoGroups (Lag)
    // NOGROUP / no such key → false (стрим/группы создаст агент)
}
```

### 3. Result Consumer

Файл: [`pkg/redisstream/consumer.go`](../../mytonprovider-backend/pkg/redisstream/consumer.go).

Каждый цикл имеет свой Consumer на `mtpa:result:<type>`. Несколько worker
goroutines внутри (Parallel в конфиге).

```go
func (c *Consumer) process(ctx, log, msg) {
    env, _ := decodeEnvelope(msg)  // ResultEnvelope

    committed, err := c.applyTx(ctx, env)
    if err != nil {
        // НЕ XACK — сообщение переедет, попробуем снова
        return
    }
    c.ack(ctx, msg.ID)
}

func (c *Consumer) applyTx(ctx, env) (committed bool, err error) {
    tx, _ := c.pool.BeginTx(ctx, ...)
    defer rollback-on-error

    // 1. dedup: INSERT INTO system.processed_jobs ... ON CONFLICT DO NOTHING
    inserted, _ := c.systemRepo.MarkProcessedTx(ctx, tx, env.JobID, env.Type, env.AgentID)

    if !inserted {
        // дубль → пустой COMMIT, false
        tx.Commit(ctx)
        return false, nil
    }

    // 2. handler.apply (только если status=ok)
    if env.Status == "ok" {
        if err := c.handler(ctx, tx, env); err != nil {
            return false, err  // rollback по defer
        }
    }

    // 3. COMMIT → caller сделает XACK
    tx.Commit(ctx)
    return true, nil
}
```

**Гарантии**:
- `MarkProcessedTx` + handler в одной транзакции → всё-или-ничего.
- Если handler упал → ROLLBACK → нет XACK → сообщение придёт снова.
- Если backend упал между COMMIT и XACK → сообщение придёт снова, но дедуп
  поймает и сделает XACK без повторной обработки.
- `status=error` от агента → пишем `processed_jobs` (чтобы не пытаться снова),
  но handler НЕ вызываем — данных нет.

### 4. Handlers

Файл: [`pkg/handlers/handlers.go`](../../mytonprovider-backend/pkg/handlers/handlers.go).

```go
type Set struct {
    logger    *slog.Logger
    providers providersRepo.Repository
    system    systemRepo.Repository
}

func (s *Set) Handler(cycleType string) redisstream.ResultHandler { ... }
```

Семь обработчиков, все следуют одному шаблону:
```go
func (s *Set) handle<Cycle>(ctx, tx, env) error {
    var result jobs.<Cycle>Result
    json.Unmarshal(env.Payload, &result)

    repo := s.providers.WithTx(tx)
    repo.<MethodA>(ctx, ...)
    repo.<MethodB>(ctx, ...)
    return nil
}
```

`WithTx(tx)` — ключевой паттерн. Каждый репо может «оборнуться» в транзакцию,
после чего Exec/Query пойдут через `pgx.Tx` вместо `pgxpool.Pool` (см. ниже).

### 5. Repositories: `DBTX` + `WithTx`

Файл: [`pkg/repositories/dbtx.go`](../../mytonprovider-backend/pkg/repositories/dbtx.go).

```go
type DBTX interface {
    Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
    Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
    QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
```

И `*pgxpool.Pool`, и `pgx.Tx` реализуют этот интерфейс.

В каждом репо:
```go
type repository struct {
    db DBTX
}

func (r *repository) WithTx(tx pgx.Tx) Repository {
    return &repository{db: tx}
}

func NewRepository(db *pgxpool.Pool) Repository {
    return &repository{db: db}
}
```

В обычном (non-tx) режиме репо использует pool и каждый вызов берёт свежий
коннект. В handler-режиме `repo.WithTx(tx)` возвращает копию с `db = tx` —
все Exec/Query идут через эту транзакцию.

Есть ещё metrics middleware (`metrics.go`), который оборачивает Repository и
инкрементирует prom-counters перед/после каждого вызова. `WithTx` в middleware
**прозрачен**: возвращает чистый repo (без метрик), чтобы tx-bound операции
шли мимо обёртки.

### 6. `system.processed_jobs`

Файл миграции:
[`db/000002_processed_jobs.up.sql`](../../mytonprovider-backend/db/000002_processed_jobs.up.sql).

```sql
CREATE TABLE IF NOT EXISTS system.processed_jobs (
    job_id       text        PRIMARY KEY,
    type         text        NOT NULL,
    agent_id     text,
    processed_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_processed_jobs_processed_at
    ON system.processed_jobs (processed_at);
```

Вставка: `INSERT INTO ... ON CONFLICT (job_id) DO NOTHING`. `RowsAffected()` = 1
→ первая обработка, 0 → дубль.

Индекс по `processed_at` нужен для очистки старых записей (cleaner может это
делать отдельной задачей).

## Local workers

`pkg/workers/workers.go` — generic scheduler (тот же, что был в старом коде,
но с `providersMaster` удалённым). Запускает:

- `telemetry.UpdateTelemetry` — собирает telemetry, приходящую от провайдеров
  через HTTP API (push-модель). Не связано с агентом, остаётся как было.
- `telemetry.UpdateBenchmarks` — то же для benchmarks.
- `cleaner.CleanupOldData` — архивация history таблиц старше N дней.

Дополнительно `runAggregates` (отдельная goroutine из main):
- Раз в 5 минут вызывает `repo.UpdateUptime()` и `repo.UpdateRating()`.
- Эти SQL-запросы пересчитывают `providers.providers.uptime` и
  `providers.providers.rating` на основе history.

## Конфиг

Файл: [`pkg/config/config.go`](../../mytonprovider-backend/pkg/config/config.go).

```go
type Config struct {
    System  System
    Metrics Metrics
    TON     TON
    DB      Postgress
    Redis   Redis
    Cycles  Cycles
}

type Cycles struct {
    ScanMaster       CycleSchedule
    ScanWallets      CycleSchedule
    ResolveEndpoints CycleSchedule
    ProbeRates       CycleSchedule
    InspectContracts CycleSchedule
    CheckProofs      CycleSchedule
    LookupIPInfo     CycleSchedule
}
```

Пример `dev.yaml`:
```yaml
system:
  system_port: "9090"
  system_log_level: 1

redis:
  addr: "redis:6379"
  group: "mtpa-backend"      # group для backend-консьюмера result-стримов
  stream_prefix: "mtpa"      # должен совпадать с агентом!
  trigger_maxlen: 10000
  block_ms: 5000
  parallel: 1                # сколько goroutine на каждый result-consumer

cycles:
  scan_master:       { enabled: true, interval: 5m,  single_inflight: true }
  scan_wallets:      { enabled: true, interval: 30m, single_inflight: true }
  resolve_endpoints: { enabled: true, interval: 30m, single_inflight: true }
  probe_rates:       { enabled: true, interval: 1m,  single_inflight: true }
  inspect_contracts: { enabled: true, interval: 30m, single_inflight: true }
  check_proofs:      { enabled: true, interval: 60m, single_inflight: true }
  lookup_ipinfo:     { enabled: true, interval: 4h,  single_inflight: true }
```

## Что удалено в этой миграции

- `pkg/workers/providersMaster/` — оркестрация переехала в агент.
- `pkg/clients/ton/` — больше не нужен (агент сам ходит в TON).
- `pkg/clients/ifconfig/` — то же.
- В `cmd/init.go` удалена функция `newProviderClient` (ADNL/DHT-инициализация).
- В `pkg/config/config.go` удалены `System.ADNLPort`, `System.Key`,
  `TON.ConfigURL`, `TON.BatchSize`.
- Зависимости `tonutils-go`, `tonutils-storage`, `tonutils-storage-provider`
  выгнаны через `go mod tidy`.

## Что осталось как было

- HTTP API (`pkg/httpServer/`, `pkg/services/providers/`) — не трогали.
- Telemetry worker — не трогали (push-модель от провайдеров).
- Cleaner — не трогали.
- Postgres-схема `providers.*` — не менялась.
- Метрики (`pkg/metrics/business.go`) — не трогали; пром-counters
  репозиториев и воркеров продолжают считаться.

## Где править если...

| Задача | Файл |
|--------|------|
| Изменить расписание цикла | `config/dev.yaml` (cycles.<type>.interval) |
| Включить/выключить цикл | `config/dev.yaml` (cycles.<type>.enabled) |
| Изменить логику apply | `pkg/handlers/handlers.go` (handle<Cycle>) |
| Новый цикл | `pkg/jobs/types.go` (envelope) → новый handler в `pkg/handlers/` → binding в `cmd/main.go` |
| Новый repo-метод | `pkg/repositories/providers/repository.go` (interface + impl) + metrics.go |
| Лимиты pgxpool | `cmd/init.go` (newPostgresConfig) |
| Заменить single-instance backend на multi-instance | добавить distributed lock на старте `dispatcher.runCycle` (Redis SETNX с TTL) |
