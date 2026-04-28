# Data contracts

Все сообщения между бэкендом и агентом — JSON в Redis Streams. Каждое
сообщение записано как `XADD <stream> * data <json>`.

## Envelope: trigger (backend → agent)

Файл: [`internal/jobs/types.go`](../internal/jobs/types.go) (агент),
[`pkg/jobs/types.go`](../../mytonprovider-backend/pkg/jobs/types.go) (бэкенд).

```json
{
  "job_id":      "uuid-v4",
  "type":        "scan_master",
  "hint":        null,
  "enqueued_at": "2026-04-26T11:35:00Z"
}
```

| Поле          | Тип       | Описание                                                  |
|---------------|-----------|-----------------------------------------------------------|
| `job_id`      | UUID v4   | Уникальный идентификатор. Используется для дедупа.        |
| `type`        | string    | Имя цикла. Должно совпадать со стрим-ключом.              |
| `hint`        | raw JSON  | Опциональный hint от бэкенда (например, `{"force":true}`). Сейчас не используется. |
| `enqueued_at` | RFC3339   | Когда бэкенд опубликовал триггер. Для метрик/лагов.       |

## Envelope: result (agent → backend)

```json
{
  "job_id":       "uuid-v4",
  "type":         "scan_master",
  "status":       "ok",
  "error":        "",
  "payload":      { ... per-cycle result ... },
  "processed_at": "2026-04-26T11:35:14Z",
  "agent_id":     "agent-9000bf17-f1ea-4157-bdc0-f487b0782479"
}
```

| Поле           | Тип             | Описание                                                |
|----------------|-----------------|---------------------------------------------------------|
| `job_id`       | UUID v4         | Тот же `job_id` из триггера. Ключ дедупа на бэкенде.    |
| `type`         | string          | Имя цикла.                                              |
| `status`       | `ok` \| `error` | Успех или ошибка цикла.                                 |
| `error`        | string          | Текст ошибки, если `status=error`.                      |
| `payload`      | JSON object     | Per-cycle результат, см. ниже.                          |
| `processed_at` | RFC3339         | Когда агент закончил выполнение. Для event-time guards. |
| `agent_id`     | string          | Идентификатор агента. Полезно для диагностики.          |

При `status=error` бэкенд всё равно делает запись в `processed_jobs` (чтобы
не переобрабатывать), но handler не вызывается — данных нет.

## Per-cycle payload schemas

### `scan_master`

**Output (`ScanMasterResult`)**:
```json
{
  "new_providers": [
    {
      "public_key":    "abc123...",
      "address":       "0:...",
      "lt":            123456789,
      "registered_at": "2026-04-20T12:00:00Z"
    }
  ],
  "last_lt":       123456789,
  "scanned_count": 42
}
```

Бэкенд:
- INSERT `providers.providers` ON CONFLICT DO NOTHING (по `public_key`).
- UPDATE `system.params SET value=last_lt WHERE key='masterWalletLastLT'`.

### `scan_wallets`

**Output (`ScanWalletsResult`)**:
```json
{
  "contracts": [
    {
      "address":       "0:...",
      "bag_id":        "deadbeef...",
      "owner_address": "0:...",
      "size":          1048576,
      "chunk_size":    65536,
      "last_tx_lt":    987654,
      "providers":     ["0:...", "0:..."]
    }
  ],
  "relations": [
    {
      "contract_address":    "0:...",
      "provider_public_key": "abc123...",
      "provider_address":    "0:...",
      "bag_id":              "deadbeef...",
      "size":                1048576
    }
  ],
  "updated_wallets": [
    {
      "public_key":    "abc123...",
      "address":       "0:...",
      "lt":            987654,
      "registered_at": "2026-04-20T12:00:00Z"
    }
  ]
}
```

Бэкенд:
- INSERT `providers.storage_contracts` (per-provider relations).
- UPDATE `providers.providers.last_tx_lt` для каждого `updated_wallets`.

### `resolve_endpoints`

**Output (`ResolveEndpointsResult`)**:
```json
{
  "endpoints": [
    {
      "public_key": "abc123...",
      "provider":   { "public_key": "raw32bytes", "ip": "1.2.3.4", "port": 12345 },
      "storage":    { "public_key": "raw32bytes", "ip": "1.2.3.4", "port": 12346 },
      "updated_at": "2026-04-26T11:30:00Z"
    }
  ],
  "skipped": 5,
  "failed":  3
}
```

Бэкенд:
- UPDATE `providers.providers SET ip=..., port=..., storage_ip=..., storage_port=...`.

### `probe_rates`

**Output (`ProbeRatesResult`)**:
```json
{
  "statuses": [
    {
      "public_key": "abc123...",
      "is_online":  true,
      "checked_at": "2026-04-26T11:35:00Z"
    }
  ],
  "rates": [
    {
      "public_key": "abc123...",
      "rates": {
        "rate_per_mb_day": 1628,
        "min_bounty":      50000000,
        "min_span":        604800,
        "max_span":        6635520
      }
    }
  ]
}
```

Бэкенд:
- UPSERT `providers.statuses` (one row per provider, latest is_online + check_time).
- UPDATE `providers.providers SET rate_per_mb_per_day, min_bounty, min_span, max_span`.

`statuses` содержит запись для **каждого** опрошенного провайдера, даже
если он offline. `rates` — только для online (offline нельзя получить тарифы).

### `inspect_contracts`

**Output (`InspectContractsResult`)**:
```json
{
  "rejected": [
    {
      "contract_address":    "0:...",
      "provider_public_key": "abc123...",
      "provider_address":    "0:...",
      "bag_id":              "deadbeef...",
      "size":                1048576
    }
  ],
  "skipped_addrs": ["0:..."]
}
```

Бэкенд:
- DELETE из `providers.storage_contracts` по списку rejected.

`skipped_addrs` — контракты, для которых lite-server вернул ошибку: бэкенд
**не должен** их удалять (`skip = true` в логике агента).

### `check_proofs`

**Output (`CheckProofsResult`)**:
```json
{
  "results": [
    {
      "contract_address":  "0:...",
      "provider_address":  "0:...",
      "reason":            0,
      "checked_at":        "2026-04-26T11:35:00Z"
    }
  ]
}
```

`reason` — `domain.ReasonCode`:
- `0` = `ValidStorageProof` (успех)
- `101` = `IPNotFound`
- `102` = `NotFound`
- `103` = `UnavailableProvider`
- `104` = `CantCreatePeer`
- `105` = `UnknownPeer`
- `201` = `PingFailed`
- `202` = `InvalidBagID`
- `203` = `FailedInitialPing`
- `301` = `GetInfoFailed`
- `302` = `InvalidHeader`
- `401` = `CantGetPiece`
- `402` = `CantParseBoC`
- `403` = `ProofCheckFailed`

Полный список: [`internal/domain/reason.go`](../internal/domain/reason.go).

Бэкенд:
- UPDATE `providers.storage_contracts SET reason, reason_timestamp = NOW()`.

### `lookup_ipinfo`

**Output (`LookupIPInfoResult`)**:
```json
{
  "items": [
    {
      "public_key": "abc123...",
      "ip":         "1.2.3.4",
      "info": {
        "country":     "United States",
        "country_iso": "US",
        "city":        "New York",
        "timezone":    "America/New_York",
        "ip":          "1.2.3.4"
      }
    }
  ]
}
```

Бэкенд:
- UPDATE `providers.providers SET ip_info = <json>` для каждого item'а.

## Совместимость

**Правила эволюции схем**:
1. **Только additive changes** — добавлять поля можно, удалять нельзя.
2. **Опциональные новые поля** — теги `omitempty` на агенте, `*Type` или
   default-значения на бэкенде.
3. **При несовместимом изменении** — вводить новое имя цикла (например,
   `probe_rates_v2`) и переходный период с обоими консьюмерами.

Не используем поля для версионирования envelope (`version`) — пока обходимся
без них; если потребуется — добавим в `TriggerEnvelope`/`ResultEnvelope`.

## Stream-keys constants

Имена стримов формируются как `<prefix>:<kind>:<type>`. По умолчанию:

```go
// agent
streams.scanMasterTrigger  = "mtpa:cycle:scan_master"
streams.scanMasterResult   = "mtpa:result:scan_master"
// ... etc
```

`<prefix>` — `Redis.StreamPrefix` в конфиге обоих сервисов. Обязательно
должен совпадать; если нет — backend и agent будут общаться через разные
ключи и не увидят друг друга.

## Ограничения размера сообщений

- Redis Streams не имеет жёсткого лимита, но XADD одного value field в
  Redis 7 может быть до ~512 MB (теоретически).
- На практике: при `probe_rates` для 10000 провайдеров payload будет ~500KB.
  Это нормально, но если станет слишком тяжело — можно ввести батчинг
  (`probe_rates` обрабатывает диапазон pubkey'ев, диспетчер раскидывает по
  батчам).
- `MAXLEN ~ 100000` (approx) на result-стримах не даст разрастаться
  безгранично; старые сообщения будут вытесняться.

## Где в коде это объявлено

- **Агент**: [`internal/jobs/types.go`](../internal/jobs/types.go).
- **Бэкенд**: [`pkg/jobs/types.go`](../../mytonprovider-backend/pkg/jobs/types.go)
  + методы `ToDB()` для конвертации в `pkg/models/db` структуры.

Эти два файла — **дублирующая правда контракта**. При изменении схемы
обязательно править оба синхронно. Юнит-тесты будущего стоит написать на
сериализацию-десериализацию через JSON, чтобы зловить расхождения.
