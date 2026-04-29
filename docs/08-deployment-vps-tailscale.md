# Deployment: VPS (backend + agent) + дополнительные агенты через Tailscale

Покрывает три способа подключения агентов к одному бэкенду:

1. **Агент на той же VPS, что бэкенд** — `MODE=local`, ходит через
   docker-bridge без tailscale. См. § 2.
2. **Агент на отдельной VPS** — `MODE=remote`, ходит через tailscale,
   разворачивается тем же `setup_server.sh`. См. § 3a.
3. **Агент на локальной машине** — ходит через tailscale, конфиг пишется
   руками (скрипт setup_server.sh для Linux/root, на Windows/macOS
   разворачиваем напрямую через docker compose). См. § 3.

Бэкенд при этом всегда один и тот же: VPS, на которой redis/postgres
биндятся ТОЛЬКО на tailscale-интерфейс (через `BIND_HOST` в `.env`),
публично — закрыты.

```
                              tailnet (100.x/10)
   ┌──────────── VPS (debian-vm-06) ────────────┐
   │                                            │
   │  backend stack (docker compose)            │
   │   ├─ app          :9090 → nginx :80        │
   │   ├─ redis        100.87.150.19:6379       │   ◄── через tailscale
   │   └─ postgres     100.87.150.19:5432       │   ◄── через tailscale
   │                                            │
   │  agent stack (docker compose)              │
   │   └─ agent №1 → docker-DNS redis/db        │
   │                                            │
   └────────────────────────────────────────────┘
                          │
                          │  tailscale
                          ▼
   ┌──────── Локальный ПК (Windows) ───────────┐
   │  agent stack (docker-compose.remote.yml)   │
   │   └─ agent №2 → 100.87.150.19:6379/5432   │
   └────────────────────────────────────────────┘
```

## Что нужно заранее

- VPS: Debian 12 / Ubuntu 22+, root SSH, минимум 2 GB RAM, 20 GB диска.
- TON master wallet address (`MASTER_ADDRESS`).
- Аккаунт Tailscale: https://login.tailscale.com/admin/settings/keys
  - **Generate auth key**, опции: **Reusable**, не **Ephemeral**, expiration 90 дней.
  - Один ключ переиспользуем для VPS и локалки.
- Локально установлен Tailscale: https://tailscale.com/download
- Локально установлен Docker Desktop с поднятым docker compose plugin.
- Оба репозитория (`mytonprovider-backend`, `mytonprovider-agent`) клонированы
  локально для редактирования.

## 1. VPS: бэкенд

### 1.1 Bootstrap SSH-ключа

Этот скрипт сам подключится к серверу и положит ваш публичный ключ в `/root/.ssh/authorized_keys`.

```bash
USERNAME=root HOST=<VPS_IP> PASSWORD=<root_pass> \
  bash mytonprovider-agent/scripts/init_server_connection.sh
```

### 1.2 Запуск установки

На VPS:
```bash
SKIP_CLONE=true \
WORK_DIR=/opt/provider \
DB_HOST=db DB_USER=pguser DB_PASSWORD=<strong> DB_NAME=providerdb DB_PORT=5432 \
MASTER_ADDRESS=<твой_TON_master> \
TAILSCALE_AUTHKEY=tskey-auth-... \
TAILSCALE_HOSTNAME=mtpa-backend \
NEWSUDOUSER=mtpa NEWUSER_PASSWORD=<sudo-pass> \
NEWFRONTENDUSER=frontend \
INSTALL_SSL=false SKIP_FRONTEND=true \
bash /opt/provider/scripts/setup_server.sh
```

Что произойдёт:
1. apt update + установка Docker + установка Tailscale (`tailscale up`).
2. `create_env_file` сам подтянет tailscale-IP в `BIND_HOST` (см. патч в
   [backend/scripts/setup_server.sh:142-167](../../mytonprovider-backend/scripts/setup_server.sh#L142-L167)).
3. `docker compose up -d` поднимет app/redis/postgres/migrate.
4. Nginx на 80 проксирует на app:9090.
5. `secure_server.sh`: UFW (22/80/443 + tailscale0), fail2ban, юзер `mtpa`,
   отключение root login и парольного SSH.

### 1.3 Если tailscale «отвалился» после secure_server.sh

`ufw enable` иногда сбивает long-lived коннекты `tailscaled`. Если
`tailscale status` показывает `offline` и жалуется на coordination server:

```bash
systemctl restart tailscaled
tailscale status
```

### 1.4 Проверка

```bash
# redis/postgres слушают ТОЛЬКО tailscale-IP, не публично
ss -ltn | grep -E ':6379|:5432'
# должно быть: 100.87.150.19:6379 и 100.87.150.19:5432

# backend поднялся
docker compose -f /opt/provider/docker-compose.yml logs --tail=50 app
# ждём "cycle scheduled" × 7
```

С локалки (после установки tailscale):
```bash
tailscale ping 100.87.150.19
nc -vz 100.87.150.19 6379
nc -vz 100.87.150.19 5432
```

## 2. VPS: агент №1 (MODE=local)

На той же VPS. Агент сидит в docker-сети `mtpa_bridge` вместе с redis/db,
ходит по docker-DNS — без tailscale.

### 2.1 Положить репо агента

```bash
# на VPS
git clone https://github.com/dearjohndoe/mytonprovider-agent /opt/provider-agent
```

(Или rsync'ом локального — `docker-compose.yml` у агента в репо есть.)

### 2.2 Запуск

```bash
MODE=local \
SKIP_CLONE=true WORK_DIR=/opt/provider-agent \
MASTER_ADDRESS=<тот_же_master> \
DB_USER=pguser DB_PASSWORD=<strong> DB_NAME=providerdb \
NEWSUDOUSER=mtpa NEWUSER_PASSWORD=<sudo-pass> \
bash /opt/provider-agent/scripts/setup_server.sh
```

Скрипт пропишет в `.env`:
- `REDIS_ADDR=redis:6379`
- `DB_HOST=db`

— это docker-DNS внутри `mtpa_bridge`, не tailscale.

`config/config.yaml` копируется из `config.example.yaml` без изменений.

### 2.3 Проверка

```bash
docker compose -f /opt/provider-agent/docker-compose.yml logs -f agent
# ждём: "starting agent" → "cycle consumer started" × 7
curl localhost:2112/metrics | head
```

## 3. Локальный ПК: агент №2 (через tailscale)

### 3.1 Подключить локалку в tailnet

```bash
# Linux/macOS
tailscale up --authkey=tskey-auth-...
# Windows: установить Tailscale через .exe и залогиниться

tailscale ip -4
# твой локальный tailscale-IP — запомни
tailscale ping 100.87.150.19
```

### 3.2 Подготовить `.env`

В `c:/Users/Administrator/Documents/mytonprovider-agent/.env`:

```env
CONFIG_PATH=/app/config/config.yaml

# TON
MASTER_ADDRESS=<тот_же_master>
TON_CONFIG_URL=https://ton-blockchain.github.io/global.config.json

# Postgres на VPS-бэкенде (через tailscale)
DB_HOST=100.87.150.19
DB_PORT=5432
DB_USER=pguser
DB_PASSWORD=<тот_же_что_на_бэкенде>
DB_NAME=providerdb

# Redis на VPS-бэкенде (через tailscale)
REDIS_ADDR=100.87.150.19:6379
REDIS_PASSWORD=
REDIS_DB=0

# Identity — обязательно ФИКСИРОВАННЫЙ AGENT_ID
SYSTEM_KEY=
AGENT_ID=mtpa-agent-local
```

> `AGENT_ID` фиксированный, не `auto`. Иначе при каждом рестарте контейнера
> старые сообщения в Redis PEL зависнут на «исчезнувшем» consumer'е.

### 3.3 Подготовить `config/config.yaml`

```bash
cp config/config.example.yaml config/config.yaml
```

Поменять `redis.addr`:
```yaml
redis:
  addr: 100.87.150.19:6379
  group: mtpa
  stream_prefix: mtpa
  result_maxlen: 100000
```

**Если ты за NAT без проброса UDP 16167** — выключить воркеры,
которым нужен входящий ADNL:
```yaml
workers:
  discovery:
    enabled: false
    pool: 1
    timeout: 30m
    block_ms: 5000
    concurrency: 16
    endpoint_ttl: 30m
  proof:
    enabled: false
    pool: 1
    timeout: 60m
    block_ms: 5000
    concurrency: 30
    endpoint_ttl: 1h
  poll:
    enabled: true
    pool: 1
    timeout: 10m
    block_ms: 5000
    concurrency: 30
  update:
    enabled: true
    pool: 1
    timeout: 60m
    block_ms: 5000
```

Тогда локальный агент берёт только `poll` и `update` задачи.

### 3.4 Запуск

```bash
docker compose -f docker-compose.remote.yml up -d --build
docker compose -f docker-compose.remote.yml logs -f agent
```

`docker-compose.remote.yml` использует **дефолтный docker-bridge** (не
`mtpa_bridge`), поэтому DNS-имена `redis`/`db` не нужны — агент идёт по
IP из `.env`.

### 3.5 Проверка

```bash
curl localhost:2112/metrics | grep mtpa_
# ожидаем метрики, в т.ч. mtpa_cycle_runs_total

docker compose -f docker-compose.remote.yml logs --tail=30 agent
# ожидаем: "starting agent" → "cycle consumer started" × N
# (N = число включённых воркеров)
```

## 3a. Альтернатива: агент на отдельной VPS (MODE=remote)

Тот же агент №2, но не на локалке, а на **второй VPS**, которая ходит к
бэкенду через tailscale. Агентский `setup_server.sh` это умеет
из коробки — `MODE=remote`. Скрипт сам ставит tailscale, проверяет
доступность бэкенда и патчит `redis.addr` в `config.yaml`.

```
       VPS-1 (бэкенд) ◄─── tailnet ───► VPS-2 (агент)
   100.87.150.19                    100.x.x.y
   redis :6379                       agent
   postgres :5432                    docker-compose.remote.yml
```

### 3a.1 Bootstrap SSH-ключа на VPS-2

Скрипт пробравсывает ssh на сервер автоматически

```bash
USERNAME=root HOST=<VPS2_IP> PASSWORD=<root_pass> \
  bash mytonprovider-agent/scripts/init_server_connection.sh
```

### 3a.2 Положить репо агента на VPS-2


```bash
ssh root@<VPS2_IP>
git clone https://github.com/dearjohndoe/mytonprovider-agent /opt/provider-agent
```

### 3a.3 Запуск агента в remote-режиме

На VPS-2:
```bash
MODE=remote \
SKIP_CLONE=true WORK_DIR=/opt/provider-agent \
BACKEND_TS_HOST=100.87.150.19 \
TAILSCALE_AUTHKEY=tskey-auth-... \
TAILSCALE_HOSTNAME=mtpa-agent-2 \
MASTER_ADDRESS=<тот_же_master> \
DB_USER=pguser DB_PASSWORD=<тот_же_что_на_бэкенде> DB_NAME=providerdb \
NEWSUDOUSER=mtpa NEWUSER_PASSWORD=<sudo-pass> \
AGENT_ID=mtpa-agent-vps2 \
bash /opt/provider-agent/scripts/setup_server.sh
```

Что произойдёт ([scripts/setup_server.sh:235-269](../scripts/setup_server.sh#L235-L269)):

1. Установит зависимости и Docker.
2. Установит Tailscale, поднимет с auth-ключом, получит свой tailnet-IP.
3. `check_backend_reachable` — проверит, что `BACKEND_TS_HOST:6379` и `:5432`
   отвечают с tailnet (warning, если нет — продолжит).
4. Положит/обновит репо в `WORK_DIR`.
5. `create_env_file` (MODE=remote, [scripts/setup_server.sh:160-196](../scripts/setup_server.sh#L160-L196))
   запишет в `.env`:
   - `REDIS_ADDR=100.87.150.19:6379`
   - `DB_HOST=100.87.150.19`
6. `create_config_file` — скопирует `config.example.yaml` → `config.yaml`
   и **сам пропатчит** `redis.addr` через sed
   ([scripts/setup_server.sh:206-209](../scripts/setup_server.sh#L206-L209)).
7. `secure_server.sh` — UFW (22/tcp + 16167/udp + tailscale0), fail2ban,
   sudo-юзер, отключение root login.
8. `start_app` — `docker compose -f docker-compose.remote.yml up -d --build`.

### 3a.4 Если за NAT нет проблемы — но воркеры всё равно хочется ограничить

VPS обычно имеет публичный IP, ADNL UDP 16167 открывается через UFW автоматически.
Но если сервер за CG-NAT (некоторые дешёвые провайдеры), UDP 16167 не достанется —
тогда руками выключить `discovery` и `proof` в `/opt/provider-agent/config/config.yaml`
(см. § 3.3 выше) и `docker compose restart agent`.

### 3a.5 Проверка

```bash
# на VPS-2
docker compose -f /opt/provider-agent/docker-compose.remote.yml logs -f agent
# ожидаем "starting agent" → "cycle consumer started"

curl localhost:2112/metrics | head

# tailscale
tailscale status
# должно быть видно: 100.87.150.19 mtpa-backend
tailscale ping 100.87.150.19
```

### 3a.6 Шаблон env-файла для скрипта-обёртки

Если будешь автоматизировать развёртывание агента №2 на VPS-2 (например,
из CI или через Ansible), стандартный набор переменных такой:

```bash
# secrets (не коммитим)
export TAILSCALE_AUTHKEY=tskey-auth-...
export DB_PASSWORD=<...>
export NEWUSER_PASSWORD=<...>

# config
export MODE=remote
export BACKEND_TS_HOST=100.87.150.19
export MASTER_ADDRESS=UQ...
export DB_USER=pguser
export DB_NAME=providerdb
export NEWSUDOUSER=mtpa
export NEWFRONTENDUSER=frontend     # не используется в агенте, но переменная в env прозрачна
export TAILSCALE_HOSTNAME=mtpa-agent-2
export AGENT_ID=mtpa-agent-vps2
export WORK_DIR=/opt/provider-agent
export SKIP_CLONE=true               # репо уже залит rsync'ом

# запуск
bash /opt/provider-agent/scripts/setup_server.sh
```

### 3a.7 Несколько агентов на разных VPS

`redis.group` (в [config.example.yaml:8](../config/config.example.yaml#L8)) у
всех агентов одинаковый (`mtpa`). Redis Streams сам распределит сообщения
между всеми consumer'ами в группе.

Уникальным должен быть только **`AGENT_ID`** — для каждой ноды задавай
свой явно (`mtpa-agent-vps2`, `mtpa-agent-vps3` и т.д.).

## 4. Проверка end-to-end

На VPS:
```bash
docker exec -it $(docker ps -qf name=provider.*redis) redis-cli
> XINFO GROUPS mtpa:cycle:poll
# должно быть 2 consumers: mtpa-agent-<vps-host> и mtpa-agent-local
> XLEN mtpa:cycle:poll
> XLEN mtpa:result:poll
```

Метрики бэкенда:
```bash
curl http://localhost:9090/metrics | grep cycle
```

## 5. Sources of truth

| Параметр | Backend | Агент на той же VPS (MODE=local) | Агент на отдельной VPS (MODE=remote) | Локальный агент |
|---|---|---|---|---|
| `DB_HOST` | `db` (внутри compose) | `db` (через mtpa_bridge) | `100.87.150.19` (скрипт пропишет сам) | `100.87.150.19` (вручную) |
| `REDIS_ADDR` | `redis:6379` | `redis:6379` | `100.87.150.19:6379` (скрипт пропишет сам) | `100.87.150.19:6379` (вручную) |
| `BIND_HOST` | `100.87.150.19` (auto из `tailscale ip -4`) | — | — | — |
| `AGENT_ID` | — | автогенерация по hostname | **`mtpa-agent-vps2`** (явно через env) | **`mtpa-agent-local`** (явно в `.env`) |
| `MODE` | — | `local` | `remote` | — (используется `docker-compose.remote.yml` напрямую) |
| Compose-файл | `docker-compose.yml` | `docker-compose.yml` (агента) | `docker-compose.remote.yml` | `docker-compose.remote.yml` |
| Tailscale на хосте | да (server) | не нужно | да (peer) | да (peer) |
| Сеть до бэкенда | — | docker-bridge `mtpa_bridge` | tailnet (UDP/TCP) | tailnet (UDP/TCP) |
| `setup_server.sh` подходит | да (backend repo) | да (`MODE=local`) | да (`MODE=remote`) | нет (Linux/root only) |

## 6. Безопасность

- Redis и Postgres биндятся **только** на tailscale-интерфейс
  (`BIND_HOST=100.87.150.19`). Публично — закрыты, UFW дополнительно режет.
- UFW на VPS:
  - 22/tcp (SSH)
  - 80/tcp, 443/tcp (nginx)
  - 16167/udp (ADNL агента №1)
  - in on tailscale0 (всё внутри tailnet)
  - всё остальное — deny.
- SSH: только ключи, root login отключён.
- `.env` — `chmod 600`.
- На локалке `.env` в `.gitignore` — не коммитим.

Для prod дополнительно:
- Завести отдельного Postgres-юзера `mtpa_agent` с урезанным GRANT
  ([05-deployment.md § Postgres GRANT для агента](05-deployment.md#postgres-grant-для-агента))
  и подставить его в `DB_USER` обоих агентов вместо `pguser`.
- Включить TLS для Postgres
  ([05-deployment.md § Postgres TLS](05-deployment.md#postgres-tls-для-production)).
- В Tailscale ACL ограничить, чтобы только агент-ноды могли коннектиться
  на `:6379`/`:5432` бэкенда.

## 7. Возможные ошибки

| Симптом | Причина | Фикс |
|---|---|---|
| `open .../docker-compose.yml: no such file or directory` после `setup_server.sh` бэкенда | Файл не запушен в публичный GitHub, скрипт делает чистый `git clone` | `rsync` локального репо + `SKIP_CLONE=true WORK_DIR=...` |
| `SKIP_CLONE=TRUE` игнорируется | bash сравнивает регистрозависимо со строкой `"true"` | Передавать в нижнем регистре: `SKIP_CLONE=true` |
| `tailscale status` показывает `offline` после `setup_server.sh` | `ufw enable` дропнул long-lived TCP к coordination server | `systemctl restart tailscaled` |
| Redis/Postgres слушают `0.0.0.0` (публично) | `BIND_HOST` не задан в `.env` бэкенда | Применить патч из [backend/scripts/setup_server.sh:142-167](../../mytonprovider-backend/scripts/setup_server.sh#L142-L167) или вручную: `echo "BIND_HOST=$(tailscale ip -4)" >> /opt/provider/.env && docker compose up -d` |
| Локальный агент не может достучаться до redis | tailscale на локалке не запущен/не в той tailnet | `tailscale status`, `tailscale ping 100.87.150.19` |
| Локальный агент берёт discovery-задачу и валится по ADNL timeout | UDP 16167 закрыт на NAT | `discovery.enabled: false`, `proof.enabled: false` в `config.yaml` |
| Два агента с одинаковым `AGENT_ID` после рестарта | `AGENT_ID=auto` сгенерил новый UUID, старый PEL «осиротел» | Зафиксировать `AGENT_ID=<стабильное_имя>` в `.env` |
| `mtpa_bridge` сеть not found на VPS-агенте | Бэкенд не объявил её или не запустился | `docker network create mtpa_bridge` (бэкенд compose это делает сам, если он стоит первым) |

## 8. Полезные команды

VPS:
```bash
# логи
docker compose -f /opt/provider/docker-compose.yml logs -f app
docker compose -f /opt/provider-agent/docker-compose.yml logs -f agent

# рестарт
docker compose -f /opt/provider/docker-compose.yml restart app
docker compose -f /opt/provider-agent/docker-compose.yml restart agent

# tailscale
tailscale status
tailscale ping <peer>
systemctl restart tailscaled

# UFW
ufw status verbose

# биндинги
ss -ltn | grep -E ':6379|:5432|:9090'
```

Локалка:
```bash
docker compose -f docker-compose.remote.yml logs -f agent
docker compose -f docker-compose.remote.yml restart agent
docker compose -f docker-compose.remote.yml down

tailscale status
tailscale ping 100.87.150.19
```
