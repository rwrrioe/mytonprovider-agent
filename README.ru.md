# mytonprovider-agent

> [English version](README.md)

Распределённый флот stateless-воркеров для сканирования, проверки и верификации провайдеров хранилища в сети TON.

## Обзор

Система разделена на два сервиса:

- **mytonprovider-backend** — единственный писатель в Postgres, оркестрирует циклы работы по крону, публикует задачи через Redis Streams.
- **mytonprovider-agent** — флот stateless-воркеров, выполняет всю тяжёлую сетевую работу (ADNL, DHT, TON liteserver, IP-lookup).

Redis Streams — шина сообщений между ними. Идемпотентность гарантируется таблицей `system.processed_jobs` с дедупликацией по UUID.

## Рабочие циклы

| Цикл | Назначение |
|---|---|
| `scan_master` | Сканирует мастер-чейн TON на новых провайдеров хранилища |
| `scan_wallets` | Сканирует контракты кошельков провайдеров |
| `resolve_endpoints` | Резолвит ADNL/DHT эндпоинты |
| `probe_rates` | Запрашивает тарифы хранения у провайдеров |
| `inspect_contracts` | Проверяет состояние контрактов |
| `check_proofs` | Верифицирует доказательства хранения |
| `lookup_ipinfo` | Определяет геолокацию по IP |

## Документация

| Документ | Аудитория |
|---|---|
| [Архитектура](docs/01-architecture.md) | Все |
| [Контракты данных](docs/02-data-contracts.md) | Разработчики |
| [Устройство агента](docs/03-agent.md) | Разработчики |
| [Устройство бэкенда](docs/04-backend.md) | Разработчики |
| [Деплой (Docker)](docs/05-deployment.md) | DevOps |
| [Деплой (VPS + Tailscale)](docs/08-deployment-vps-tailscale.md) | DevOps |
| [Операционный справочник](docs/06-operations.md) | Ops |
| [История изменений](docs/07-changelog.md) | Все |

## Быстрый старт

Локальный запуск через docker-compose — [docs/05-deployment.md](docs/05-deployment.md). Деплой на VPS с Tailscale — [docs/08-deployment-vps-tailscale.md](docs/08-deployment-vps-tailscale.md).
