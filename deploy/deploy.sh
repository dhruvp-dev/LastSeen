#!/usr/bin/env bash
# Build, test and deploy to the VPS:   deploy/deploy.sh user@host
set -euo pipefail
HOST="${1:?usage: deploy/deploy.sh user@host}"
cd "$(dirname "$0")/.."

make test
make release

echo ">> uploading"
scp bin/lastseen-linux-amd64 "$HOST:/tmp/lastseen.new"

echo ">> installing"
ssh "$HOST" 'set -e
  sudo systemctl start lastseen-backup.service 2>/dev/null || true   # snapshot before upgrading
  sudo install -m 0755 -o root -g root /tmp/lastseen.new /opt/lastseen/lastseen.next
  sudo mv -f /opt/lastseen/lastseen.next /opt/lastseen/lastseen      # atomic swap
  rm -f /tmp/lastseen.new
  sudo systemctl restart lastseen-web lastseen-worker
  sleep 4
  systemctl is-active lastseen-web lastseen-worker
  curl -fsS http://127.0.0.1:8080/healthz'
echo ">> done"
