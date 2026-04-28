# Deployment

## Локальный запуск с docker-compose

Симулирует двух-машинное развёртывание: backend stack и agent stack живут в
разных compose-проектах, общаются через external Docker network.

### 1. Создать общий network

```bash
docker network create mtpa_bridge
```

Эта сеть симулирует «канал между машинами» — через неё agent контейнер
сможет дотянуться до backend's redis и postgres (но не до backend's app
напрямую — он остаётся в private network).

### 2. Backend stack

```
mytonprovider-backend/
├── docker-compose.yml
├── .env
└── config/dev.yaml
```

`docker-compose.yml`:
```yaml
services:
  app:
    build: .
    depends_on:
      db_migrate: { condition: service_completed_successfully }
      redis:      { condition: service_healthy }
    ports: ["${SYSTEM_PORT}:${SYSTEM_PORT}"]
    env_file: [.env]
    environment:
      - SYSTEM_PORT=${SYSTEM_PORT}
      - CONFIG_PATH=${CONFIG_PATH}
    volumes: ["./config:/app/config:ro"]
    networks: [backend]            # private — agent сюда не достучится

  redis:
    image: redis:7-alpine
    ports: ["6379:6379"]
    healthcheck: ["CMD", "redis-cli", "ping"]
    networks: [backend, bridge_net]  # доступен и backend'у, и агенту

  db:
    image: postgres:15
    environment: { POSTGRES_USER, POSTGRES_PASSWORD, POSTGRES_DB }
    ports: ["${DB_PORT}:5432"]
    volumes: [postgres_data:/var/lib/postgresql/data]
    healthcheck: ["CMD-SHELL", "pg_isready ..."]
    networks: [backend, bridge_net]  # доступен и backend'у, и агенту

  db_migrate:
    image: migrate/migrate:4
    depends_on: { db: { condition: service_healthy } }
    volumes: ["./db:/db"]
    command: ["-path", "/db", "-database", "postgres://...?sslmode=disable", "up"]
    networks: [backend]

networks:
  backend:
  bridge_net:
    external: true
    name: mtpa_bridge

volumes:
  postgres_data:
```

`.env`:
```
DB_HOST=db
DB_USER=pguser
DB_PASSWORD=secret
DB_NAME=providerdb
DB_PORT=5432
SYSTEM_PORT=9090
CONFIG_PATH=/app/config/dev.yaml
MASTER_ADDRESS=<your master wallet address>
```

Запуск:
```bash
cd mytonprovider-backend
docker compose up -d
docker compose logs -f app
# ожидаем: "cycle scheduled" для всех 7 циклов, потом "triggered" каждые N минут
```

### 3. Agent stack

```
mytonprovider-agent/
├── docker-compose.yml
├── .env
└── config/config.yaml
```

`docker-compose.yml`:
```yaml
services:
  agent:
    build: .
    container_name: mtpa_agent
    env_file: [.env]
    environment: [CONFIG_PATH=/app/config/config.yaml]
    ports:
      - "2112:2112"        # /metrics
      - "16167:16167/udp"  # ADNL
    volumes: ["./config:/app/config:ro"]
    networks: [bridge_net]  # ТОЛЬКО bridge — нет доступа к backend.app
    restart: unless-stopped

networks:
  bridge_net:
    external: true
    name: mtpa_bridge
```

Агент **не** видит сервис `app` бэкенда. Он подключается только к `redis` и
`db` через bridge.

`.env`:
```
CONFIG_PATH=/app/config/config.yaml
MASTER_ADDRESS=<same as backend>
TON_CONFIG_URL=https://ton-blockchain.github.io/global.config.json

# через bridge видит redis и db бэкенда
REDIS_ADDR=redis:6379
DB_HOST=db
DB_PORT=5432
DB_USER=pguser
DB_PASSWORD=secret
DB_NAME=providerdb

# auto-gen на старте
SYSTEM_KEY=
AGENT_ID=auto
```

`config/config.yaml`:
```yaml
system: { agent_id: auto, adnl_port: "16167", log_level: 1 }
redis:  { addr: redis:6379, group: mtpa, stream_prefix: mtpa, result_maxlen: 100000 }
ton:    { config_url: https://ton-blockchain.github.io/global.config.json }
workers:
  discovery:        { enabled: true, pool: 1, timeout: 30m, concurrency: 16, endpoint_ttl: 30m }
  poll:             { enabled: true, pool: 1, timeout: 10m, concurrency: 30 }
  proof:            { enabled: true, pool: 1, timeout: 60m, concurrency: 30, endpoint_ttl: 1h }
  update:           { enabled: true, pool: 1, timeout: 60m }
metrics: { enabled: true, port: "2112" }
```

Запуск:
```bash
cd mytonprovider-agent
docker compose up -d
docker compose logs -f agent
# ожидаем: "starting agent" → "cycle consumer started" × 7
# через минуту: "cycle ok" с job_id для probe_rates
```

### 4. Проверка end-to-end

См. [operations.md § Verification](06-operations.md#verification).

## Production deployment

### Сетевая модель (рекомендуемая)

**Один регион / VPC**:
```
┌────────────────────────────────────┐
│ Private VPC (10.0.0.0/16)          │
│                                    │
│  Backend host (10.0.1.10)          │
│  ├─ postgres :5432 (private only)  │
│  ├─ redis :6379  (private + agent VPC) │
│  └─ backend app :9090 (public via LB)  │
│                                    │
│  Agent host #1 (10.0.2.10)         │
│  └─ agent (no public ports)        │
│                                    │
│  Agent host #2 (10.0.2.11)         │
│  └─ agent                          │
└────────────────────────────────────┘
```

**Cross-cloud / cross-region**:
- Используйте VPN (Tailscale, WireGuard) или PrivateLink/PSC.
- На бэкенд-хосте redis биндится только на VPN-интерфейс:
  ```yaml
  redis:
    ports: ["100.64.0.1:6379:6379"]  # tailscale-IP
  ```
- На агенте `REDIS_ADDR=100.64.0.1:6379`.

### Postgres GRANT для агента

Агент работает в режиме «read-only + только LT-write». Минимальная роль:

```sql
CREATE ROLE mtpa_agent LOGIN PASSWORD '<strong-password>';

GRANT CONNECT ON DATABASE providerdb TO mtpa_agent;
GRANT USAGE   ON SCHEMA providers, system TO mtpa_agent;

-- Read
GRANT SELECT  ON providers.providers,
                 providers.storage_contracts
              TO mtpa_agent;

-- LT-чекпоинты
GRANT SELECT, UPDATE ON system.params TO mtpa_agent;
GRANT UPDATE (last_tx_lt) ON providers.providers TO mtpa_agent;
```

Колоночный `GRANT UPDATE` не даст агенту записать что-то ещё, кроме
`last_tx_lt`. Это важная защита — даже если в код агента запихнут баг,
который пишет «не туда», БД запретит.

### Postgres TLS (для production)

В `postgresql.conf`:
```ini
listen_addresses = '0.0.0.0'  # или конкретный VPN IP
ssl = on
ssl_cert_file = '/etc/postgresql/server.crt'
ssl_key_file  = '/etc/postgresql/server.key'
ssl_ca_file   = '/etc/postgresql/ca.crt'
```

В `pg_hba.conf`:
```
hostssl  providerdb  mtpa_agent  10.0.0.0/8  scram-sha-256
hostssl  providerdb  pguser      10.0.0.0/8  scram-sha-256
host     all         all         0.0.0.0/0   reject
```

В DSN агента (правка `internal/lib/config/config.go`):
```go
return fmt.Sprintf(
    "postgres://%s:%s@%s:%s/%s?sslmode=verify-full&sslrootcert=/etc/ssl/agent/ca.crt",
    url.QueryEscape(p.User), url.QueryEscape(p.Password),
    p.Host, p.Port, p.Name,
)
```

### pgxpool лимиты

В [`internal/adapters/outbound/postgres/storage.go`](../internal/adapters/outbound/postgres/storage.go)
рекомендуется выставить умеренные лимиты на агенте:

```go
cfg.MaxConns        = 8
cfg.MinConns        = 1
cfg.MaxConnLifetime = time.Hour
cfg.MaxConnIdleTime = 10 * time.Minute
```

При N агентах * 8 коннектов = 8N к Postgres. Бэкенд использует свой пул
(MaxConns=12). Postgres `max_connections` (по умолч. 100) должен покрывать
8N + 12 + резерв.

### Multi-agent

Все агенты запускаются с одинаковым `redis.group` (`mtpa`). Redis Streams
равномерно раскидывает сообщения между consumer'ами одной группы.

**`AGENT_ID`** должен быть уникален. По умолчанию `auto` → UUID на старте,
поэтому два агента с одинаковым конфигом получат разные ID.

**Важно**: если агент рестартует с новым AGENT_ID, его старые сообщения в
PEL зависнут. Решения:
- Зафиксировать AGENT_ID в env (например, по hostname): `AGENT_ID=mtpa-agent-1`.
- Запустить XAUTOCLAIM-reaper отдельным cron-job'ом (см. operations.md).

### Multi-backend (НЕ поддерживается из коробки)

Для нескольких backend-инстансов потребуется distributed lock на dispatcher:
```go
// перед XADD триггера
ok, _ := rdb.SetNX(ctx, "mtpa:lock:scan_master", instanceID, 10*time.Minute).Result()
if !ok { return }
// XADD; lock TTL'ится сам
```

Текущий код предполагает 1 backend-инстанс.

## Миграции

Используется `migrate/migrate` (golang-migrate). Файлы — `db/NNNNNN_<name>.up.sql`
и `down.sql`.

Запуск из docker-compose: сервис `db_migrate` стартует один раз перед app:
```yaml
db_migrate:
  image: migrate/migrate:4
  depends_on: { db: { condition: service_healthy } }
  volumes: ["./db:/db"]
  command: ["-path", "/db", "-database", "postgres://...?sslmode=disable", "up"]
```

После апа сервис завершается (`Exited (0)`) — это нормально.

Текущие миграции:
- `000001_init.up.sql` — изначальная схема.
- `000002_processed_jobs.up.sql` — таблица дедупа.

При откате на старую архитектуру: `000002_processed_jobs.down.sql` дропает
`system.processed_jobs`. Остальные данные не теряются (схема `providers.*`
не менялась).

## CI/CD

Не входит в скоуп этого документа, но базовые рекомендации:
- Тестировать сборку: `go build ./...` + `go vet ./...` (оба проекта).
- Lint: `golangci-lint run`.
- Smoke-тест: docker-compose up в CI runner'е, отправить триггер
  через `loadtest`, проверить результат.
- Образы пушить в registry с тегом коммита.

## Откат

Если новая архитектура показала проблемы:

1. **Backend**:
   ```bash
   git checkout <pre-redis-commit>
   go build ./...
   docker compose build app && docker compose up -d
   ```
2. **Postgres**: схема совместимая, `processed_jobs` можно оставить (не мешает).
   Если хочется удалить — `migrate -path db -database ... down 1`.
3. **Redis**: можно стопнуть, не нужен старой архитектуре.
4. **Agent**: остановить контейнеры; не нужны старой архитектуре.

Никаких миграций данных не требуется.
