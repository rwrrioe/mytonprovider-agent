#!/bin/bash

# Local development setup for the agent.
# Assumes backend stack is already running (creates mtpa_bridge if missing).
#
# Usage: bash scripts/setup_local.sh

set -e

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$ROOT_DIR/.env"
ENV_EXAMPLE="$ROOT_DIR/.env.example"
CONFIG_FILE="$ROOT_DIR/config/config.yaml"
CONFIG_EXAMPLE="$ROOT_DIR/config/config.example.yaml"

RED='\033[0;31m'; GREEN='\033[0;32m'; BLUE='\033[0;34m'; NC='\033[0m'
info()  { echo -e "${BLUE}[INFO]${NC} $1"; }
ok()    { echo -e "${GREEN}[OK]${NC} $1"; }
err()   { echo -e "${RED}[ERROR]${NC} $1"; }

check_docker() {
    command -v docker &>/dev/null    || { err "Docker not found"; exit 1; }
    docker info &>/dev/null          || { err "Docker daemon is not running"; exit 1; }
    docker compose version &>/dev/null \
        || { err "docker compose plugin not found"; exit 1; }
}

ensure_bridge_network() {
    if docker network inspect mtpa_bridge &>/dev/null; then
        info "Network mtpa_bridge already exists."
    else
        info "Creating network mtpa_bridge..."
        docker network create mtpa_bridge
    fi
}

ensure_env() {
    if [ -f "$ENV_FILE" ]; then
        info ".env already exists, skipping."
        return
    fi
    [ -f "$ENV_EXAMPLE" ] || { err ".env.example not found"; exit 1; }
    cp "$ENV_EXAMPLE" "$ENV_FILE"
    ok "Created .env from .env.example — review MASTER_ADDRESS and DB_* values."
}

ensure_config() {
    if [ -f "$CONFIG_FILE" ]; then
        info "config/config.yaml already exists, skipping."
        return
    fi
    [ -f "$CONFIG_EXAMPLE" ] || { err "config/config.example.yaml not found"; exit 1; }
    cp "$CONFIG_EXAMPLE" "$CONFIG_FILE"
    ok "Created config/config.yaml from example."
}

main() {
    info "Local agent setup..."
    check_docker
    ensure_bridge_network
    ensure_env
    ensure_config

    info "Building and starting agent..."
    docker compose -f "$ROOT_DIR/docker-compose.yml" up -d --build

    ok "Agent started."
    echo ""
    echo "Logs:    docker compose -f docker-compose.yml logs -f agent"
    echo "Stop:    docker compose -f docker-compose.yml down"
    echo "Metrics: http://localhost:2112/metrics"
    echo ""
    echo "NOTE: backend stack (mytonprovider-backend) must already be running"
    echo "      and connected to the mtpa_bridge network."
}

main "$@"
