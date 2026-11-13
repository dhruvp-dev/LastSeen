#!/usr/bin/env bash
# One-time server setup. Run as root on the VPS from the directory containing these files:
#   sudo ./install.sh
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
here="$(cd "$(dirname "$0")" && pwd)"

id lastseen >/dev/null 2>&1 || useradd --system --home-dir /var/lib/lastseen --shell /usr/sbin/nologin lastseen
install -d -m 0755 /opt/lastseen
install -d -m 0750 -o root -g root /etc/lastseen
if [ ! -f /etc/lastseen/lastseen.env ]; then
  install -m 0600 -o root -g root "$here/lastseen.env.example" /etc/lastseen/lastseen.env
  echo ">> Created /etc/lastseen/lastseen.env - edit it (password hash, SMTP) before starting."
fi
for f in lastseen-web.service lastseen-worker.service lastseen-backup.service lastseen-backup.timer; do
  install -m 0644 "$here/$f" /etc/systemd/system/$f
done
systemctl daemon-reload
systemctl enable lastseen-web.service lastseen-worker.service lastseen-backup.timer
cat <<MSG

Next steps:
  1. Copy the binary to /opt/lastseen/lastseen        (deploy.sh does this)
  2. Edit /etc/lastseen/lastseen.env
  3. Install Caddy (https://caddyserver.com/docs/install#debian-ubuntu-raspbian) and put deploy/Caddyfile at /etc/caddy/Caddyfile
  4. systemctl restart caddy && systemctl start lastseen-web lastseen-worker lastseen-backup.timer
  5. curl -fsS http://127.0.0.1:8080/healthz
MSG
