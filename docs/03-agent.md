# Agent: code walkthrough

Stateless воркер. На вход — триггеры из Redis, выполнение цикла, выход —
результат в Redis + LT-чекпоинты в Postgres.

## Структура каталогов

```
mytonprovider-agent/
├── cmd/
│   ├── app/main.go           # bootstrap: ADNL stack + Redis + repos + usecases + consumers
│   └── loadtest/main.go      # утилита тестовой нагрузки
├── config/
│   ├── config.example.yaml   # шаблон
│   └── config.yaml           # реальный конфиг (не коммитим, .gitignore)
├── internal/
│   ├── domain/               # доменные типы (Provider, StorageContract, ReasonCode, ...)
│   ├── jobs/types.go         # JSON-контракт стримов (зеркало бэкендового pkg/jobs)
│   ├── lib/
│   │   ├── config/config.go  # cleanenv-loader
│   │   ├── sl/sl.go          # slog-helper для error attributes
│   │   └── utils/utils.go
│   ├── adapters/
│   │   ├── inbound/
│   │   │   └── redisstream/  # consumer trigger-стримов
│   │   └── outbound/
│   │       ├── postgres/     # репозитории (read-only + LT-write)
│   │       ├── redisstream/  # publisher result-стримов
│   │       ├── dht/          # DHT-резолвер endpoint'ов
│   │       ├── directprovider/  # ADNL probe + RLDP bag prover
│   │       ├── ipconfig/     # HTTP ifconfig клиент
│   │       └── ton/          # TON liteclient + master/wallet scanners + on-chain inspector
│   └── usecase/
│       ├── discovery/        # scan_master + scan_wallets + resolve_endpoints
│       ├── poll/             # probe_rates + inspect_contracts
│       ├── proof/            # check_proofs
│       └── update/           # lookup_ipinfo
└── pkg/
    └── metrics/metrics.go    # prometheus + /metrics
```

## Поток управления

1. **Старт** ([`cmd/app/main.go`](../cmd/app/main.go)):
   - `config.MustLoad()` — yaml + env через cleanenv.
   - `connectPostgres` — pgxpool.
   - `tonclient.NewClient` — пул liteservers из config URL.
   - `initADNL` — UDP listener + 2 ADNL gateways (DHT + provider) + dht.Client + transport.Client.
   - `redis.NewClient` + ping.
   - `redisout.NewPublisher` — для XADD результатов.
   - Создание репозиториев + сетевых адаптеров.
   - Создание юзкейсов с инжекцией `Publisher`.
   - На каждый включённый цикл — `EnsureGroup` + запуск `redisin.Consumer.Run` в goroutine.

2. **Каждый цикл** (один из 7):
   - Consumer делает `XREADGROUP COUNT 1 BLOCK <block_ms>` на `mtpa:cycle:<type>`.
   - На сообщение:
     - Декодирует `TriggerEnvelope`, прокидывает `JobID` через `ctx`
       (`jobs.WithJobID(ctx, jobID)`).
     - Вызывает usecase-метод (например, `discoveryUC.CollectNewProviders(ctx)`).
     - Метод выполняет полный цикл: read DB → orchestrate → publish result + LT-write.
     - После возврата (успех/ошибка) — `XACK` без условий.
   - Pool — несколько goroutines в одном consumer'е могут параллельно обрабатывать
     несколько сообщений одного типа (`pool > 1`).

## Юзкейсы

Все живут в `internal/usecase/`. Каждый — структура с зависимостями (порты),
конструктор `New(...)` и набор методов-handlers (по одному на цикл этого
юзкейса). Сигнатура handler'а: `func(ctx context.Context) error`.

Внутри handler'а:
1. Прочитать необходимое из Postgres (read-only, через `*Repo` интерфейсы).
2. Сделать сетевой I/O (через `MasterScanner`, `WalletScanner`, `EndpointResolver`,
   `RatesProbe`, `BagProver`, `ContractInspector`, `IPInfoClient`).
3. Собрать `jobs.<Cycle>Result` payload.
4. Вызвать `publisher.PublishResult(ctx, streamKey, jobs.JobIDFrom(ctx), cycleType, payload)`.
5. Если цикл предполагает LT-checkpoint — записать его (`SetMasterWalletLT`,
   `UpdateLT`).

### `discovery`

Файл: [`internal/usecase/discovery/discovery.go`](../internal/usecase/discovery/discovery.go).

```go
type Discovery struct {
    masterScanner MasterScanner
    walletScanner WalletScanner
    resolver      EndpointResolver
    providerRepo  ProviderRepo  // GetAllPubkeys, GetAllWallets, UpdateLT
    contractRepo  ContractRepo  // GetActiveRelations
    endpointRepo  EndpointRepo  // LoadAll
    systemRepo    SystemRepo    // GetMasterWalletLT, SetMasterWalletLT
    publisher     Publisher
}

func (d *Discovery) CollectNewProviders(ctx context.Context) error
func (d *Discovery) CollectStorageContracts(ctx context.Context) error
func (d *Discovery) ResolveEndpoints(ctx context.Context) error
```

- `CollectNewProviders`: читает `from_lt` из system.params, сканирует master,
  фильтрует по уже известным pubkey'ям, публикует `ScanMasterResult`,
  пишет новый `last_lt` в БД.
- `CollectStorageContracts`: для каждого provider-кошелька сканирует
  транзакции, агрегирует контракты и relations, публикует `ScanWalletsResult`,
  обновляет per-wallet LT.
- `ResolveEndpoints`: читает relations + текущие endpoints, для каждой устаревшей
  записи делает DHT-резолв, публикует список свежих endpoints.

### `poll`

Файл: [`internal/usecase/poll/poll.go`](../internal/usecase/poll/poll.go).

```go
type Poll struct {
    probe        RatesProbe
    inspector    ContractInspector
    providerRepo ProviderRepo  // GetAllPubkeys
    contractRepo ContractRepo  // GetActiveRelations
    publisher    Publisher
}

func (p *Poll) UpdateProviderRates(ctx context.Context) error
func (p *Poll) UpdateRejectedContracts(ctx context.Context) error
```

- `UpdateProviderRates`: параллельный ADNL `GetStorageRates` для всех
  pubkey'ев (semaphore = `MaxConcurrentProbe`). Каждому pubkey соответствует
  запись в `statuses` (offline/online). Online'ам — запись в `rates`.
- `UpdateRejectedContracts`: для всех уникальных contract address'ов вызывает
  on-chain `GetProvidersInfo`, применяет `isRemovedByLowBalance`-логику
  (баланс/maxSpan/lastProofTime), агрегирует rejected relations.

### `proof`

Файл: [`internal/usecase/proof/proof.go`](../internal/usecase/proof/proof.go).

```go
type Proof struct {
    inspector    ContractInspector
    resolver     EndpointResolver
    prover       BagProver
    contractRepo ContractRepo  // GetActiveRelations
    endpointRepo EndpointRepo  // LoadFresh (TTL-based cache)
    publisher    Publisher
}

func (p *Proof) CheckStorageProofs(ctx context.Context) error
```

Группирует relations по pubkey, для каждого pubkey:
- Загружает свежий endpoint из кэша (`endpointRepo.LoadFresh`).
- Если endpoint stale/missing → все bag'и помечаются `IPNotFound`.
- Иначе для каждого bag'а: `prover.Verify(ctx, ep, bagID)` → `ReasonCode`.
- При >20% подряд фейлов — оставшиеся помечаются `UnavailableProvider`
  (защита от мёртвого провайдера).

### `update`

Файл: [`internal/usecase/update/update.go`](../internal/usecase/update/update.go).

```go
type Update struct {
    ipinfo       IPInfoClient
    ipinfoRepo   IPInfoRepo  // GetProvidersIPs
    publisher    Publisher
}

func (u *Update) UpdateIPInfo(ctx context.Context) error
```

Читает провайдеров с непустым IP, но без `ip_info`. Для каждого делает
HTTP `GET ifconfig.io/json?ip=...` (rate-limited через `BetweenIPsCooldown`),
агрегирует результаты, публикует.

## Порты (interfaces)

Каждый юзкейс-пакет имеет `ports.go` с зависимостями. Это:
- Сетевые порты (реализуются `outbound/dht`, `outbound/directprovider` и т.д.).
- Repository-порты (реализуются `outbound/postgres`).
- `Publisher` (реализуется `outbound/redisstream`).

Конкретные интерфейсы:
- `discovery.MasterScanner`, `WalletScanner`, `EndpointResolver`, `ProviderRepo`,
  `ContractRepo`, `EndpointRepo`, `SystemRepo`, `Publisher`
- `poll.RatesProbe`, `ContractInspector`, `ProviderRepo`, `ContractRepo`, `Publisher`
- `proof.ContractInspector`, `EndpointResolver`, `BagProver`, `ContractRepo`,
  `EndpointRepo`, `Publisher`
- `update.IPInfoClient`, `IPInfoRepo`, `Publisher`

## Адаптеры (outbound)

### `postgres/`

Read-only (плюс LT-write). Файлы:
- `provider.go` — GetAllPubkeys, GetAllWallets, UpdateLT.
- `contract.go` — GetActiveRelations.
- `endpoint.go` — LoadFresh, LoadAll.
- `ipinfo.go` — GetProvidersIPs.
- `system.go` — GetMasterWalletLT, SetMasterWalletLT.
- `storage.go` — `NewPool` (pgxpool helper с Ping).

Закомментированные методы в `provider.go` оставлены преднамеренно — это
«библиотека» возможных запросов из старого бэкенда, на случай если что-то
понадобится. Раскомментировать и запушить — никого не сломает.

### `ton/`

- `liteclient/client.go` — пул `liteclient.ConnectionPool`, методы
  `GetTransactions`, `GetStorageContractsInfo`, `GetProvidersInfo`.
- `master_scanner.go` — `Discovery.MasterScanner`. Парсит транзакции master
  на регистрационные комментарии `tsp-<pubkey>`.
- `scanner.go` — `Discovery.WalletScanner`. Парсит транзакции provider-кошелька
  на storage-reward-withdrawal (op `0xa91baf56`).
- `inspector.go` — `Poll.ContractInspector` + `Proof.ContractInspector`.
  Маппит `liteclient.StorageContractProviders` → `domain.ContractOnChainState`.

### `dht/`

`dht.go` — `Discovery.EndpointResolver` + `Proof.EndpointResolver`. Использует
`tonutils-go/adnl/dht` + `tonutils-storage-provider/pkg/transport` для:
- `Resolve(pubkey, contractAddr)`: DHT lookup `storage-provider` запись →
  IP/port → `VerifyStorageADNLProof` → DHT lookup адреса storage-узла.
- `ResolveStorageWithOverlay(providerIP, bags)`: best-effort через
  overlay-DHT, если основной resolve не нашёл storage.

### `directprovider/`

`client.go` — `Poll.RatesProbe` + `Proof.BagProver`.
- `Probe(pubkey)`: `transport.Client.GetStorageRates(ctx, pubkey, fakeSize=1)`.
  Возвращает `domain.Rates` + флаг online.
- `Verify(ep, bagID)`: создаёт ADNL peer для `ep.Storage`, ping, RLDP-сессия,
  `GetTorrentInfo` → парсинг BoC → `GetPiece(random)` → `cell.CheckProof`.
  Возвращает `domain.ReasonCode`.

### `ipconfig/`

`client.go` — `Update.IPInfoClient`. HTTP `GET ifconfig.io/json?ip=...`,
маппинг JSON → `domain.IpInfo`.

### `redisstream/` (outbound)

`publisher.go` — `Publisher`. Методы:
- `PublishResult(ctx, streamKey, jobID, cycleType, payload)` — XADD
  с `MAXLEN ~ ResultMaxLen`, статус `ok`.
- `PublishError(ctx, streamKey, jobID, cycleType, errMsg)` — XADD
  со статусом `error` (сейчас не используется юзкейсами, но доступно).

## Адаптер (inbound)

### `redisstream/`

`consumer.go` — `Consumer` + `ConsumerConfig`.
- `Run(ctx)` запускает `Pool` goroutines.
- В каждой goroutine: цикл `XREADGROUP` → `process(ctx, msg)`.
- `process`:
  - Декодирует `TriggerEnvelope` (только для логов: jobID).
  - Создаёт `handlerCtx` с timeout + `jobs.WithJobID(ctx, jobID)`.
  - `runHandler` с `recover()` (panic → error result, но в текущей реализации
    публикуется только `ok`-результат).
  - Метрики: `CyclesInflight.Inc/Dec`, `CycleDuration.Observe`, `CyclesTotal.Inc`.
  - `XACK` всегда (success/error → один и тот же путь). Backend сам решает
    через `processed_jobs`, что делать с дублями.

`groups.go` — `EnsureGroup` (XGROUP CREATE MKSTREAM, игнор BUSYGROUP).

## Конфиг

Файл: [`internal/lib/config/config.go`](../internal/lib/config/config.go).

Группировка `Workers` per-usecase:
```go
type Workers struct {
    Discovery UsecaseCfg `yaml:"discovery"`
    Poll      UsecaseCfg `yaml:"poll"`
    Proof     UsecaseCfg `yaml:"proof"`
    Update    UsecaseCfg `yaml:"update"`
}

type UsecaseCfg struct {
    Enabled     bool          // если false — consumer'ы этих циклов не запускаются
    Pool        int           // сколько goroutine'ов на каждый цикл
    Timeout     time.Duration // per-cycle timeout
    BlockMs     int           // XREADGROUP BLOCK
    Concurrency int           // внутренний параллелизм цикла (probe/verify)
    EndpointTTL time.Duration // для discovery + proof
}
```

`pool=1` означает «1 goroutine на каждый цикл этой группы». То есть для
`discovery` (3 цикла) с `pool=1` → 3 consumer goroutines (по одному на
scan_master, scan_wallets, resolve_endpoints).

`Concurrency` — отдельная сущность: внутри одного цикла probe пула провайдеров
параллельно (semaphore=`Concurrency`).

`AGENT_ID=auto` → UUID на старте. `SYSTEM_KEY` (ed25519) — пустой → новая
генерация на старте. Эти оба параметра можно зафиксировать в env, чтобы
identity сохранялась между рестартами (актуально для multi-agent setup'а).

## Метрики

Файл: [`pkg/metrics/metrics.go`](../pkg/metrics/metrics.go).

`/metrics` на `${METRICS_PORT}` (по умолч. 2112). Метрики (namespace=`ton_storage`,
subsystem=`mtpa`):

- `mtpa_cycles_total{cycle, status}` — Counter, total processed.
- `mtpa_cycle_duration_seconds{cycle}` — Histogram (exp buckets от 0.05s).
- `mtpa_cycles_inflight{cycle}` — Gauge.
- `mtpa_redis_errors_total` — Counter.
- `mtpa_publish_errors_total` — Counter.

Инкрементируются в `redisstream/consumer.go:process`.

## Loadtest harness

Файл: [`cmd/loadtest/main.go`](../cmd/loadtest/main.go).

Утилита для проверки конкретного цикла без ожидания cron-расписания:

```bash
go run ./cmd/loadtest \
  -addr <redis>:6379 \
  -prefix mtpa \
  -cycle scan_master \
  -count 10 \
  -timeout 60s
```

Отправляет N триггеров через XADD, читает result-stream от метки `$`, ждёт
N результатов с матчингом по `job_id`. Печатает sum-up: ok/error/missing.

Полезно при изменениях схемы или в smoke-тесте после деплоя.

## Где править если...

| Задача | Файл |
|--------|------|
| Изменить таймаут одного цикла | `config/config.yaml` (workers.<group>.timeout) |
| Добавить новый цикл | `internal/jobs/types.go` (envelope + result schema) → новый usecase-метод → wiring в `cmd/app/main.go` (binding) |
| Новое поле в результате | `internal/jobs/types.go` (агент) + `pkg/jobs/types.go` (бэкенд) + handler на бэкенде |
| Изменить логику цикла | `internal/usecase/<group>/<group>.go` |
| Новый сетевой адаптер | `internal/adapters/outbound/<name>/` + порт в `internal/usecase/<group>/ports.go` |
| Лимиты Postgres connections | `internal/adapters/outbound/postgres/storage.go` |
