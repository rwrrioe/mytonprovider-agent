#!/bin/bash

# Hardens the agent host: UFW, fail2ban, dedicated sudo user, SSH lock-down.
# Tailored for the mytonprovider-agent: only ADNL/UDP and SSH need to be open.
# Uses tailscale interface (if present) for redis/postgres traffic.
#
# Usage: NEWSUDOUSER=<u> PASSWORD=<p> [ADNL_PORT=16167] [MODE=local|remote] $0

set -e

[[ $EUID -ne 0 ]] && { echo "❌ Run as root"; exit 1; }
[[ -z "$NEWSUDOUSER" || -z "$PASSWORD" ]] && {
    echo "❌ Missing NEWSUDOUSER or PASSWORD"
    exit 1
}

ADNL_PORT="${ADNL_PORT:-16167}"
MODE="${MODE:-local}"

svc_restart() {
    if [ -d /run/systemd/system ] && command -v systemctl &>/dev/null; then
        systemctl restart "$1" || true
    elif command -v service &>/dev/null; then
        service "$1" restart || service "$1" start || true
    fi
}

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get -y install unattended-upgrades fail2ban ufw sudo

# Non-interactive enable of unattended-upgrades
echo 'unattended-upgrades unattended-upgrades/enable_auto_updates boolean true' \
    | debconf-set-selections
dpkg-reconfigure -f noninteractive unattended-upgrades

echo "Configuring UFW..."
ufw --force reset
ufw default deny incoming
ufw default allow outgoing
ufw allow 22/tcp comment 'SSH'
ufw allow ${ADNL_PORT}/udp comment 'ADNL'

# Tailscale: trust the VPN interface fully (only needed in remote mode, but
# safe to apply always — interface won't exist if tailscale not installed).
if ip link show tailscale0 &>/dev/null; then
    echo "Allowing all traffic on tailscale0..."
    ufw allow in on tailscale0 comment 'tailscale'
fi

ufw --force enable || echo "⚠️  ufw enable failed (kernel modules?) — skipping."

echo "Configuring Fail2ban..."
cat > /etc/fail2ban/jail.local <<EOL
[sshd]
enabled  = true
port     = ssh
filter   = sshd
logpath  = /var/log/auth.log
maxretry = 5
bantime  = 3600
findtime = 600
EOL
svc_restart fail2ban

echo "Creating sudo user $NEWSUDOUSER..."
if ! id "$NEWSUDOUSER" &>/dev/null; then
    useradd -m -s /bin/bash "$NEWSUDOUSER"
fi
usermod -aG sudo "$NEWSUDOUSER"
usermod -aG docker "$NEWSUDOUSER" 2>/dev/null || true
echo "$NEWSUDOUSER:$PASSWORD" | chpasswd

mkdir -p /home/"$NEWSUDOUSER"/.ssh
chmod 700 /home/"$NEWSUDOUSER"/.ssh
chown "$NEWSUDOUSER":"$NEWSUDOUSER" /home/"$NEWSUDOUSER"/.ssh

if [ -f /root/.ssh/authorized_keys ]; then
    cp /root/.ssh/authorized_keys /home/"$NEWSUDOUSER"/.ssh/
    chmod 600 /home/"$NEWSUDOUSER"/.ssh/authorized_keys
    chown "$NEWSUDOUSER":"$NEWSUDOUSER" /home/"$NEWSUDOUSER"/.ssh/authorized_keys
else
    echo "⚠️  /root/.ssh/authorized_keys not found — skipping key copy."
fi

WORK_DIR="${WORK_DIR:-/opt/provider-agent}"
[[ -d "$WORK_DIR" ]] && chown -R "$NEWSUDOUSER":"$NEWSUDOUSER" "$WORK_DIR"

echo "Hardening SSH..."
if [ -f /etc/ssh/sshd_config ]; then
    sed -i -E 's/^#?\s*PermitRootLogin\s+\S+/PermitRootLogin no/' /etc/ssh/sshd_config
    sed -i -E 's/^#?\s*PasswordAuthentication\s+\S+/PasswordAuthentication no/' /etc/ssh/sshd_config
    sed -i -E 's/^#?\s*ChallengeResponseAuthentication\s+\S+/ChallengeResponseAuthentication no/' /etc/ssh/sshd_config
    sed -i -E 's/^#?\s*PubkeyAuthentication\s+\S+/PubkeyAuthentication yes/' /etc/ssh/sshd_config

    # Idempotent AllowUsers (replace any existing line).
    if grep -qE '^AllowUsers\s' /etc/ssh/sshd_config; then
        sed -i -E "s/^AllowUsers\s.*/AllowUsers $NEWSUDOUSER/" /etc/ssh/sshd_config
    else
        echo "AllowUsers $NEWSUDOUSER" >> /etc/ssh/sshd_config
    fi

    svc_restart ssh || svc_restart sshd || true
else
    echo "⚠️  /etc/ssh/sshd_config not found — skipping SSH hardening."
fi

echo "✅ Server secured (mode=$MODE, ADNL=$ADNL_PORT/udp open)."
