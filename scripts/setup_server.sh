#!/bin/bash

# Main agent setup script.
#
# Two modes:
#   MODE=local   — agent on the SAME host as the backend (uses external docker
#                  network mtpa_bridge to reach backend's redis/postgres).
#   MODE=remote  — agent on a DIFFERENT host, reaches backend over tailscale.
#                  Requires BACKEND_TS_HOST (tailscale IP or MagicDNS name of
#                  the backend host).
#
# Usage (local, same VPS as backend):
#   MODE=local \
#   MASTER_ADDRESS=<wallet> \
#   DB_USER=pguser DB_PASSWORD=secret DB_NAME=providerdb \
#   NEWSUDOUSER=mtpa NEWUSER_PASSWORD=<password> \
#   ./setup_server.sh
#
# Usage (remote, second VPS — tailscale required):
#   MODE=remote \
#   TAILSCALE_AUTHKEY=tskey-auth-... \
#   BACKEND_TS_HOST=100.64.0.1 \
#   MASTER_ADDRESS=<wallet> \
#   DB_USER=mtpa_agent DB_PASSWORD=<strong> DB_NAME=providerdb \
#   NEWSUDOUSER=mtpa NEWUSER_PASSWORD=<password> \
#   ./setup_server.sh

set -e

GITHUB_REPO="${GITHUB_REPO:-dearjohndoe/mytonprovider-agent}"
GITHUB_BRANCH="${GITHUB_BRANCH:-master}"
WORK_DIR="${WORK_DIR:-/opt/provider-agent}"
MODE="${MODE:-local}"

DB_PORT="${DB_PORT:-5432}"
ADNL_PORT="${ADNL_PORT:-16167}"
METRICS_PORT="${METRICS_PORT:-2112}"
TON_CONFIG_URL="${TON_CONFIG_URL:-https://ton-blockchain.github.io/global.config.json}"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
info()    { echo -e "${BLUE}[INFO]${NC} $1"; }
ok()      { echo -e "${GREEN}[OK]${NC} $1"; }
warn()    { echo -e "${YELLOW}[WARN]${NC} $1"; }
err()     { echo -e "${RED}[ERROR]${NC} $1"; }

check_required_vars() {
    local required=(
        "MODE" "MASTER_ADDRESS"
        "DB_USER" "DB_PASSWORD" "DB_NAME"
        "NEWSUDOUSER" "NEWUSER_PASSWORD"
    )
    if [[ "$MODE" == "remote" ]]; then
        required+=("BACKEND_TS_HOST" "TAILSCALE_AUTHKEY")
    fi
    local missing=()
    for v in "${required[@]}"; do
        [[ -z "${!v}" ]] && missing+=("$v")
    done
    if (( ${#missing[@]} > 0 )); then
        err "Missing required env vars: ${missing[*]}"
        exit 1
    fi
    case "$MODE" in
        local|remote) ;;
        *) err "MODE must be 'local' or 'remote' (got: $MODE)"; exit 1 ;;
    esac
}

install_deps() {
    info "Installing system dependencies..."
    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get -y -o Dpkg::Options::="--force-confdef" -o Dpkg::Options::="--force-confold" upgrade
    apt-get install -y curl git ca-certificates gnupg lsb-release
}

install_docker() {
    if command -v docker &>/dev/null && docker compose version &>/dev/null; then
        info "Docker already installed: $(docker --version)"
        return
    fi
    local os_id
    os_id=$(. /etc/os-release && echo "$ID")
    [[ "$os_id" != "debian" && "$os_id" != "ubuntu" ]] && os_id="ubuntu"

    info "Installing Docker (repo for $os_id)..."
    install -m 0755 -d /etc/apt/keyrings
    curl -fsSL "https://download.docker.com/linux/$os_id/gpg" \
        | gpg --dearmor --yes -o /etc/apt/keyrings/docker.gpg
    chmod a+r /etc/apt/keyrings/docker.gpg
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/$os_id $(lsb_release -cs) stable" \
        > /etc/apt/sources.list.d/docker.list
    apt-get update
    apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
    systemctl enable docker
    systemctl start docker
    ok "Docker installed."
}

install_tailscale() {
    if command -v tailscale &>/dev/null; then
        info "Tailscale already installed: $(tailscale version | head -n1)"
    else
        info "Installing Tailscale..."
        curl -fsSL https://tailscale.com/install.sh | sh
    fi

    if tailscale status &>/dev/null && tailscale ip -4 &>/dev/null; then
        info "Tailscale already up: $(tailscale ip -4 | head -n1)"
    else
        info "Bringing up Tailscale (authkey)..."
        local hostname_arg=""
        [[ -n "$TAILSCALE_HOSTNAME" ]] && hostname_arg="--hostname=$TAILSCALE_HOSTNAME"
        tailscale up --authkey="$TAILSCALE_AUTHKEY" --ssh=false $hostname_arg
        ok "Tailscale up: $(tailscale ip -4 | head -n1)"
    fi
}

check_backend_reachable() {
    info "Checking reachability of backend at ${BACKEND_TS_HOST}:${DB_PORT} and :6379..."
    # nc may not be installed; use bash /dev/tcp
    local ok_db=0 ok_redis=0
    timeout 5 bash -c ">/dev/tcp/${BACKEND_TS_HOST}/${DB_PORT}" 2>/dev/null && ok_db=1
    timeout 5 bash -c ">/dev/tcp/${BACKEND_TS_HOST}/6379" 2>/dev/null && ok_redis=1
    if (( ok_db == 0 )); then
        warn "Cannot reach Postgres ${BACKEND_TS_HOST}:${DB_PORT} (continuing — backend may not be up yet)."
    else
        ok "Postgres reachable."
    fi
    if (( ok_redis == 0 )); then
        warn "Cannot reach Redis ${BACKEND_TS_HOST}:6379 (continuing)."
    else
        ok "Redis reachable."
    fi
}

clone_repo() {
    info "Setting up repository in $WORK_DIR..."
    if [ -d "$WORK_DIR/.git" ]; then
        info "Repo exists, fetching $GITHUB_BRANCH..."
        git -C "$WORK_DIR" fetch origin "$GITHUB_BRANCH"
        git -C "$WORK_DIR" checkout "$GITHUB_BRANCH"
        git -C "$WORK_DIR" reset --hard "origin/$GITHUB_BRANCH"
    else
        git clone --branch "$GITHUB_BRANCH" "https://github.com/$GITHUB_REPO" "$WORK_DIR"
    fi
    ok "Repository ready."
}

ensure_bridge_network() {
    if docker network inspect mtpa_bridge &>/dev/null; then
        info "Docker network mtpa_bridge already exists."
    else
        info "Creating docker network mtpa_bridge..."
        docker network create mtpa_bridge
    fi
}

create_env_file() {
    info "Creating .env (mode=$MODE)..."
    local redis_addr db_host
    if [[ "$MODE" == "local" ]]; then
        redis_addr="redis:6379"
        db_host="db"
    else
        redis_addr="${BACKEND_TS_HOST}:6379"
        db_host="${BACKEND_TS_HOST}"
    fi

    cat > "$WORK_DIR/.env" <<EOL
CONFIG_PATH=/app/config/config.yaml

# TON
MASTER_ADDRESS=${MASTER_ADDRESS}
TON_CONFIG_URL=${TON_CONFIG_URL}

# Postgres
DB_HOST=${db_host}
DB_PORT=${DB_PORT}
DB_USER=${DB_USER}
DB_PASSWORD=${DB_PASSWORD}
DB_NAME=${DB_NAME}

# Redis
REDIS_ADDR=${redis_addr}
REDIS_PASSWORD=${REDIS_PASSWORD:-}
REDIS_DB=${REDIS_DB:-0}

# Identity (auto = generate UUID on start; set explicitly for stable PEL across restarts)
SYSTEM_KEY=
AGENT_ID=${AGENT_ID:-mtpa-agent-$(hostname -s)}
EOL
    chmod 600 "$WORK_DIR/.env"
    ok ".env created."
}

create_config_file() {
    if [ -f "$WORK_DIR/config/config.yaml" ]; then
        info "config/config.yaml already exists, leaving as-is."
        return
    fi
    info "Creating config/config.yaml from example..."
    cp "$WORK_DIR/config/config.example.yaml" "$WORK_DIR/config/config.yaml"
    # In remote mode, point redis.addr in the yaml too (some env-vars may not
    # override yaml fields depending on cleanenv tags — keep both consistent).
    if [[ "$MODE" == "remote" ]]; then
        sed -i "s|^  addr: redis:6379|  addr: ${BACKEND_TS_HOST}:6379|" "$WORK_DIR/config/config.yaml"
    fi
    ok "config.yaml created."
}

start_app() {
    info "Starting agent..."
    local compose_file="docker-compose.yml"
    if [[ "$MODE" == "remote" ]]; then
        compose_file="docker-compose.remote.yml"
        if [ ! -f "$WORK_DIR/$compose_file" ]; then
            err "$WORK_DIR/$compose_file not found (needed for remote mode)."
            exit 1
        fi
    else
        ensure_bridge_network
    fi
    docker compose -f "$WORK_DIR/$compose_file" up -d --build
    ok "Agent started (compose: $compose_file)."
}

execute_script() {
    local script="$WORK_DIR/scripts/$1"
    [[ ! -f "$script" ]] && { err "Script not found: $script"; exit 1; }
    bash "$script" || { err "Script $1 failed"; exit 1; }
}

main() {
    [[ $EUID -ne 0 ]] && { err "Run as root"; exit 1; }

    info "Starting agent setup (mode=$MODE)..."
    check_required_vars
    install_deps
    install_docker

    if [[ "$MODE" == "remote" ]]; then
        install_tailscale
        check_backend_reachable
    fi

    if [[ "${SKIP_CLONE:-false}" == "true" ]]; then
        warn "SKIP_CLONE=true — using existing $WORK_DIR."
        [[ ! -d "$WORK_DIR" ]] && { err "$WORK_DIR not found"; exit 1; }
    else
        clone_repo
    fi

    create_env_file
    create_config_file

    info "Securing the server..."
    export PASSWORD="$NEWUSER_PASSWORD"
    export ADNL_PORT METRICS_PORT MODE
    execute_script "secure_server.sh"

    if [[ "${SKIP_APP_START:-false}" == "true" ]]; then
        warn "SKIP_APP_START=true — skipping docker compose up."
    else
        start_app
    fi

    ok "Agent setup completed!"
    echo ""
    echo "Summary:"
    echo "  Mode:         $MODE"
    echo "  Work dir:     $WORK_DIR"
    echo "  SSH user:     $NEWSUDOUSER"
    [[ "$MODE" == "remote" ]] && echo "  Backend:      $BACKEND_TS_HOST (via tailscale)"
    [[ "$MODE" == "remote" ]] && echo "  Tailscale IP: $(tailscale ip -4 2>/dev/null | head -n1 || echo unknown)"
    echo ""
    echo "Useful commands:"
    if [[ "$MODE" == "remote" ]]; then
        echo "  Logs:    docker compose -f $WORK_DIR/docker-compose.remote.yml logs -f agent"
        echo "  Restart: docker compose -f $WORK_DIR/docker-compose.remote.yml restart agent"
    else
        echo "  Logs:    docker compose -f $WORK_DIR/docker-compose.yml logs -f agent"
        echo "  Restart: docker compose -f $WORK_DIR/docker-compose.yml restart agent"
    fi
    echo "  Metrics: http://localhost:${METRICS_PORT}/metrics"
}

main "$@"