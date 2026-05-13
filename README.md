# mytonprovider-agent

> [Русская версия](README.ru.md)

A distributed fleet of stateless workers that scan, probe, and verify storage providers in the TON network.

## Overview

The system is split into two services:

- **mytonprovider-backend** — single Postgres writer, orchestrates cron-triggered work cycles, dispatches tasks via Redis Streams.
- **mytonprovider-agent** — stateless worker fleet, handles all heavy network I/O (ADNL, DHT, TON liteserver, IP lookups).

Redis Streams act as the message bus between them. Idempotency is guaranteed via the `system.processed_jobs` table with UUID-based deduplication.

## Work Cycles

| Cycle | What it does |
|---|---|
| `scan_master` | Scans the TON master chain for new storage providers |
| `scan_wallets` | Scans provider wallet contracts |
| `resolve_endpoints` | Resolves ADNL/DHT endpoints |
| `probe_rates` | Probes storage rates from providers |
| `inspect_contracts` | Inspects contract state |
| `check_proofs` | Verifies storage proofs |
| `lookup_ipinfo` | Resolves IP geolocation info |

## Documentation

| Doc | Audience |
|---|---|
| [Architecture](docs/01-architecture.md) | Everyone |
| [Data Contracts](docs/02-data-contracts.md) | Developers |
| [Agent Internals](docs/03-agent.md) | Developers |
| [Backend Internals](docs/04-backend.md) | Developers |
| [Deployment (Docker)](docs/05-deployment.md) | DevOps |
| [Deployment (VPS + Tailscale)](docs/08-deployment-vps-tailscale.md) | DevOps |
| [Operations Runbook](docs/06-operations.md) | Ops |
| [Changelog](docs/07-changelog.md) | Everyone |

## Quick Start

See [docs/05-deployment.md](docs/05-deployment.md) for local docker-compose setup and [docs/08-deployment-vps-tailscale.md](docs/08-deployment-vps-tailscale.md) for production VPS deployment with Tailscale.
