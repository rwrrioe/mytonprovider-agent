# Operations runbook

Что смотреть, как чинить, как масштабировать.

## Verification (после деплоя)

### 1. Стримы и группы созданы

```bash
docker compose exec redis redis-cli
> KEYS mtpa:*
1) "mtpa:cycle:probe_rates"
2) "mtpa:cycle:scan_master"
... (после первых триггеров от бэкенда)
N) "mtpa:result:probe_rates"
... (после первых результатов от агента)

> XINFO GROUPS mtpa:cycle:probe_rates
1)  1) "name"
    2) "mtpa"
    3) "consumers"     [N — сколько активных агентов]
    4) "pending"       [сколько in-flight у этой группы]
    5) "lag"           [сколько недоставленных]
```

Если `consumers=0` — ни один агент не подключился. Проверить:
- Сетевая связность агента → Redis.
- Конфиг `redis.group` совпадает.
- `redis.stream_prefix` совпадает у backend и agent.

### 2. Триггеры приходят

```bash
docker compose logs -f app | grep "triggered"
# {"msg":"triggered","cycle":"probe_rates","job_id":"<uuid>"}
```

Каждый цикл должен быть видно по интервалу из `dev.yaml`. Если нет —
проверить `cycles.<type>.enabled: true`.

### 3. Результаты применяются

```bash
docker compose logs -f app | grep "result applied"
# {"msg":"result applied","cycle":"probe_rates","job_id":"<uuid>","status":"ok"}
```

Если видишь `apply tx failed` — handler упал. Смотри текст ошибки, чаще
всего это:
- DB constraint violation (нужно дебажить handler).
- Несовпадение JSON-схемы (агент и бэкенд из разных версий).

### 4. Дедуп работает

```bash
docker compose exec db psql -U pguser -d providerdb \
  -c "SELECT type, count(*) FROM system.processed_jobs GROUP BY type;"
```

Должны быть видны записи по всем включённым циклам. `processed_at` — время
последнего успешно обработанного job'а этого типа.

### 5. Нет зависших сообщений

```bash
docker compose exec redis redis-cli XPENDING mtpa:cycle:probe_rates mtpa
# 1) "0"        — сколько pending (in-flight у consumer'а)
# 2) ""         — самый старый id
# 3) ""         — самый новый id
# 4) (empty array) — список consumer'ов с pending
```

Если pending > 0 и не падает — агент завис или умер во время обработки.

### 6. Метрики

```bash
curl localhost:2112/metrics | grep mtpa_
```

Что смотреть:
- `mtpa_cycles_total{cycle, status}` — должны расти.
- `mtpa_cycle_duration_seconds` — перцентиль 99 не должен превышать
  настроенный timeout.
- `mtpa_cycles_inflight` — обычно 0–1; если стабильно > pool — дашба перегружена.
- `mtpa_redis_errors_total` — должно быть 0; рост = проблемы с Redis.
- `mtpa_publish_errors_total` — должно быть 0.

## Тестовая нагрузка

Утилита [`cmd/loadtest`](../cmd/loadtest/main.go) кидает N триггеров и
ждёт N результатов:

```bash
# собрать
go build -o /tmp/loadtest ./cmd/loadtest

# скопировать в агент-контейнер (чтобы dns 'redis' резолвился через bridge)
docker cp /tmp/loadtest mtpa_agent:/tmp/loadtest

# запустить
docker exec mtpa_agent /tmp/loadtest \
    -addr redis:6379 \
    -cycle scan_master \
    -count 10 \
    -timeout 60s
```

**Что покажет**:
- `→ trigger #1 job_id=<uuid>` — XADD'ы прошли.
- `← ok job_id=<uuid> agent=<id>` — каждый job'ом отработан.
- `results: ok=N error=0 missing=0 total=N` — все доехали.

**Замечания**:
- Если pubkey'и в БД случайные (сидинг через `gen_random_bytes`), реальные
  TON-запросы будут таймаутить (`ProbeTimeout=14s`). 50 циклов × 14s × pool=1
  = ~12 минут. В load-тесте используйте мастер-кошелёк из mainnet или
  занизьте `ProbeTimeout` в config.yaml до 1-2s.
- Для проверки дедупа: отправить XADD с одним и тем же `job_id` дважды.
  Бэкенд первый apply'ит, второй пропускает (`duplicate result, skipped`).

## Troubleshooting

### Симптом: агент стартует, но result-стримы пусты

Проверить:
1. Триггеры есть в `mtpa:cycle:<type>`? `XLEN mtpa:cycle:probe_rates`.
2. Агент в правильной consumer-group? `XINFO GROUPS mtpa:cycle:probe_rates`.
3. Логи агента: `docker logs mtpa_agent | grep "cycle ok\|cycle failed"`.
4. У агента есть доступ к БД? Циклы валятся на `GetAllPubkeys`?
   - Проверь DB_HOST/DB_USER/... в `.env` агента.
   - Проверь сеть: `docker exec mtpa_agent nc -vz db 5432`.

### Симптом: `apply tx failed` на бэкенде

Стандартные причины:
- **constraint violation в БД** — handler пытается вставить плохие данные.
  Смотри `error` в логе.
- **`ON CONFLICT cannot affect row a second time`** — handler передаёт
  батч с дубликатами по уникальному ключу. Дедуп на уровне приложения,
  возможно агент шлёт два status'а на один pubkey.

В обоих случаях сообщение НЕ XACK'ится → переедет → опять упадёт. Чтобы
разорвать цикл:
- Поправить код и перезапустить бэкенд (старое сообщение применится с
  исправленным handler'ом, ИЛИ).
- Вручную отметить job как обработанный:
  ```sql
  INSERT INTO system.processed_jobs (job_id, type) VALUES ('<bad-job-id>', 'probe_rates');
  ```
  + XACK:
  ```bash
  redis-cli XACK mtpa:result:probe_rates mtpa-backend <msg-id>
  ```

### Симптом: метрики `cycles_inflight` высокий стабильно

Цикл занимает дольше interval'а. Варианты:
- Увеличить `pool` в config.yaml агента (больше параллельных consumer'ов).
- Увеличить `interval` в `dev.yaml` бэкенда (реже триггерить).
- Профилировать цикл — что именно тормозит (TON liteserver? ADNL?).

### Симптом: результаты приходят, но БД не обновляется

Проверить, что бэкенд apply'ит:
```bash
docker logs app | grep "result applied\|duplicate"
```

Если `duplicate result, skipped` — job_id уже в `processed_jobs`. Это нормально
при ре-поставке одного и того же сообщения.

Если ни того, ни другого — consumer не работает:
- Проверь `mtpa-backend` group существует на result-стриме.
- Проверь логи бэкенда на ошибки старта консьюмера.

### Симптом: агент крашнулся, сообщения зависли в PEL

```bash
redis-cli XPENDING mtpa:cycle:probe_rates mtpa
# 1) (integer) 5
# 2) "1727203341302-0"
# 3) "1727203455123-0"
# 4) 1) 1) "agent-dead-uuid"
#       2) "5"
```

Если 5 сообщений висят на консумере, который мёртв (DEAD-IDLE > 10 минут):

**Ручной reclaim**:
```bash
# определить новый consumer (живой)
redis-cli XINFO CONSUMERS mtpa:cycle:probe_rates mtpa
# claim сообщения у мёртвого
redis-cli XAUTOCLAIM mtpa:cycle:probe_rates mtpa <new-consumer> 600000 0 COUNT 100
```

**Автоматизация** (нужно дописать как отдельный cron-job или внутрь агента):
```bash
*/5 * * * * redis-cli XAUTOCLAIM mtpa:cycle:probe_rates mtpa reaper 600000 0
```

### Симптом: triggers льются, но nothing applied

Backend → Agent через Redis работает, Agent → Backend через Redis — нет.

```bash
# что в result-стриме?
redis-cli XLEN mtpa:result:probe_rates
# 0
```

Если 0 — агент молчит. Возможные причины:
- Агент failed на чтении из БД (нет доступа?).
- Агент таймаутит на TON liteserver (`ProbeTimeout`).
- Цикл сам не публикует пустой результат (но в нашем коде — публикует).

```bash
docker logs mtpa_agent | grep "failed\|cycle ok"
```

## Multi-agent

### Запуск второго агента

```bash
# тот же compose, разные env
cd mytonprovider-agent
cp -r . ../mytonprovider-agent-2
cd ../mytonprovider-agent-2

# поменять имя контейнера и порты в docker-compose.yml
sed -i 's/mtpa_agent/mtpa_agent_2/' docker-compose.yml
sed -i 's/2112:2112/2113:2112/' docker-compose.yml
sed -i 's/16167:16167/16168:16167/' docker-compose.yml

# зафиксировать AGENT_ID
echo "AGENT_ID=mtpa-agent-2" >> .env

docker compose up -d
```

Оба агента в одной consumer-group `mtpa` → Redis раскидывает сообщения
between them. Throughput удвоится (если узкое место — сетевые I/O).

### Проверка распределения

```bash
redis-cli XINFO CONSUMERS mtpa:cycle:probe_rates mtpa
1) 1) "name"        2) "agent-uuid-1"  3) "pending" 4) (integer) 0  5) "idle" 6) (integer) 1234
2) 1) "name"        2) "mtpa-agent-2"  3) "pending" 4) (integer) 0  5) "idle" 6) (integer) 1567
```

Если у одного pending != 0 надолго — он медленнее или подвис.

### Удаление мёртвого consumer'а

```bash
redis-cli XGROUP DELCONSUMER mtpa:cycle:probe_rates mtpa <dead-id>
```

Перед этим обязательно XAUTOCLAIM его сообщения, иначе они потеряются.

## Scaling

### Узкое место — Redis

Redis с одним инстансом легко тянет десятки тысяч XADD/sec. Если
смотреть метрики:
- `redis-cli INFO commandstats` — сколько XADD/XREADGROUP в сек.
- Latency обычно < 1ms на простых командах.

Если упирается:
- Шардить по типам циклов (отдельные redis-инстансы под разные циклы).
- Redis Cluster (но Streams требуют hash-tag в ключе для co-location).

### Узкое место — Postgres writes

Бэкенд пишет в одной транзакции на каждый job. Если баклог растёт:
- Увеличить `redis.parallel` (больше backend-горутин consume'ят результаты
  параллельно). До 4-8 нормально, выше — гонки на одних и тех же UPDATE'ах.
- Размер batch'ей — настраивается в агенте (например, scan_wallets обрабатывает
  все wallet'ы за один трик'ер, а не по одному).
- Connection pool: `cmd/init.go newPostgresConfig` → `defaultMaxConns`.

### Узкое место — TON liteservers

Агент использует `liteclient.ConnectionPool` с retries. Если liteservers
тормозят — увеличить `parrallelRequests` в `internal/adapters/outbound/ton/liteclient/client.go`,
или подключить больше liteserver'ов через свой config.json.

## Backup / restore

- **Postgres**: стандартный pg_dump. `system.processed_jobs` бэкапить
  обязательно — без неё после restore результаты будут двойно применяться.
- **Redis**: `XRANGE` history можно потерять (агенты переотправят результаты
  при ре-делавери; processed_jobs защитит от дублей).

Recovery scenario:
1. Postgres restore из последнего backup.
2. Redis flush (`FLUSHDB`).
3. Backend start — пересоздаст consumer-groups.
4. Agent start — пересоздаст consumer-groups (как trigger reader).
5. Первый trigger пройдёт через 1 интервал (по умолч. 1m для probe_rates,
   5m для scan_master и т.д.).

Длительность downtime ≈ MAX(cycle interval) = 4h для lookup_ipinfo. Для
форсированного переоткрытия — можно вручную через `loadtest`.

## Cleanup processed_jobs

Через несколько недель таблица `system.processed_jobs` может разрастись.
Можно настроить cron на чистку старше N дней:

```sql
DELETE FROM system.processed_jobs
WHERE processed_at < NOW() - INTERVAL '30 days';
```

Безопасно: эти job_id'ы никогда не придут снова (Redis MAXLEN их выкинул
ещё раньше).

## Откат на старый бэкенд

См. [deployment.md § Откат](05-deployment.md#откат). Кратко:
- `git checkout` на pre-pivot коммит.
- `migrate down 1` (опционально).
- `docker compose up -d`.

Никаких потерь данных — схема `providers.*` не менялась.

## Известные ограничения / будущая работа

- **Reaper для XAUTOCLAIM** — не автоматизирован.
- **Multi-instance backend** — не поддерживается (нужен distributed lock).
- **`/healthz` HTTP endpoint** на агенте — нет.
- **OTel-trace propagation** — не реализовано.
- **`verify_bags_batch`** для оптимизации proof-цикла — обсуждается.

См. [01-architecture.md § Что НЕ покрыто](01-architecture.md#что-не-покрыто-текущей-реализацией-известные-todos).
