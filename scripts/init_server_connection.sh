#!/bin/bash

# Bootstraps SSH key auth on a fresh server. Identical purpose to the backend's
# script but uses ed25519 by default and avoids double-quoting the password.
#
# Usage: USERNAME=root HOST=1.2.3.4 PASSWORD=yourpassword ./init_server_connection.sh

set -e

if [ -z "$USERNAME" ] || [ -z "$HOST" ] || [ -z "$PASSWORD" ]; then
    echo "❌ Missing USERNAME / HOST / PASSWORD"
    echo "Usage: USERNAME=root HOST=1.2.3.4 PASSWORD=yourpassword $0"
    exit 1
fi

if ! command -v sshpass &>/dev/null; then
    echo "❌ sshpass not found. Install: sudo apt-get install sshpass"
    exit 1
fi

KEY="${HOME}/.ssh/id_ed25519"
KEY_PUB="${KEY}.pub"
if [ ! -f "$KEY_PUB" ]; then
    # Fall back to existing RSA key if ed25519 doesn't exist
    if [ -f "${HOME}/.ssh/id_rsa.pub" ]; then
        KEY_PUB="${HOME}/.ssh/id_rsa.pub"
    else
        echo "Generating ed25519 SSH key..."
        mkdir -p "${HOME}/.ssh"
        ssh-keygen -t ed25519 -f "$KEY" -N ""
    fi
fi

PUBKEY=$(cat "$KEY_PUB")

if [ "$USERNAME" = "root" ]; then
    SSH_DIR="/root/.ssh"
else
    SSH_DIR="/home/$USERNAME/.ssh"
fi

# sshpass reads password from env to avoid shell escaping issues
export SSHPASS="$PASSWORD"

sshpass -e ssh -o StrictHostKeyChecking=no -o PubkeyAuthentication=no \
    "$USERNAME@$HOST" bash <<EOF
set -e
mkdir -p "$SSH_DIR"
chmod 700 "$SSH_DIR"
grep -qxF '$PUBKEY' "$SSH_DIR/authorized_keys" 2>/dev/null \
    || echo '$PUBKEY' >> "$SSH_DIR/authorized_keys"
chmod 600 "$SSH_DIR/authorized_keys"
EOF

echo "✅ Public key installed."

sshpass -e ssh -o StrictHostKeyChecking=no -o PubkeyAuthentication=no \
    "$USERNAME@$HOST" bash <<'EOF'
set -e
sed -i -E 's/^#?\s*PasswordAuthentication\s+\S+/PasswordAuthentication no/'        /etc/ssh/sshd_config
sed -i -E 's/^#?\s*ChallengeResponseAuthentication\s+\S+/ChallengeResponseAuthentication no/' /etc/ssh/sshd_config
sed -i -E 's/^#?\s*PubkeyAuthentication\s+\S+/PubkeyAuthentication yes/'           /etc/ssh/sshd_config
systemctl restart ssh 2>/dev/null || systemctl restart sshd 2>/dev/null \
    || service ssh restart 2>/dev/null || service sshd restart 2>/dev/null || true
EOF

unset SSHPASS

sleep 3
if ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=no \
    "$USERNAME@$HOST" "echo ok" >/dev/null; then
    echo "✅ SSH key auth verified."
else
    echo "❌ Could not verify key auth — check sshd config on the host."
    exit 1
fi
