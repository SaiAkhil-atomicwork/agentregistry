#!/usr/bin/env bash
# Install the agentgateway nightly-restart systemd timer on EC2-B.
# Run as a user with sudo. Idempotent — safe to re-run.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

sudo install -m 0644 "$DIR/agentgateway-nightly-restart.service" /etc/systemd/system/
sudo install -m 0644 "$DIR/agentgateway-nightly-restart.timer"   /etc/systemd/system/

sudo systemctl daemon-reload
sudo systemctl enable --now agentgateway-nightly-restart.timer

echo "=== installed; current timer state ==="
systemctl status --no-pager agentgateway-nightly-restart.timer | head -15
echo
echo "=== next 3 fires ==="
systemctl list-timers --all --no-pager agentgateway-nightly-restart.timer | head -5
