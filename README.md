# Last Seen

A small, self-hosted uptime monitor for my own sites. One Go binary, SQLite, server-rendered pages
with a little HTMX. Public status page at `status.dhruvp.tech`, private dashboard under `/admin`.
Single user by design: no registration, billing or teams.

## How it works

One binary, two long-running services that share a SQLite file (WAL mode):

| Command | Role |
| --- | --- |
| `lastseen serve` | Web server: public status page, login, dashboard. |
| `lastseen worker` | Runs checks, opens/resolves incidents, sends emails. |
| `lastseen backup [dir]` | Consistent snapshot of the live database, keeps the newest 14. |
| `lastseen hash-password` | Prints a bcrypt hash for `LASTSEEN_ADMIN_PASSWORD_HASH`. |
| `lastseen migrate` | Applies migrations (both services also do this at startup). |

**Checks.** Every enabled monitor is checked every 20 minutes (`LASTSEEN_CHECK_INTERVAL`). Each attempt
records status code, response time and any error.

**Avoiding false alarms.** Each monitor has *Retries* (0–5) and *Wait between retries* (10 s – 30 min).
After a failed check the worker retries after that wait. An incident opens, and one "down" email goes out,
only when the first attempt and every retry fail in a row. Retries are scheduled in the database, so they
survive restarts. The next successful check resolves the incident and sends one "recovered" email.
Time to confirm an outage is roughly `retries × wait`.

**Email.** Messages go through an outbox table with a `UNIQUE(incident, kind)` constraint, so one incident
can never produce two "down" or two "recovered" emails. Failed sends retry with exponential backoff
(30 s doubling, capped at 1 h) and are marked failed after 6 attempts. The dashboard shows the delivery log.

**Status page honesty.** If the worker heartbeat is older than 2 minutes the public page says
"Monitoring paused" instead of showing green, and `/healthz` returns 503.

## Local development

```sh
make css                                    # builds internal/web/static/app.css (needs tools/tailwindcss)
export LASTSEEN_ADMIN_PASSWORD_HASH="$(go run ./cmd/lastseen hash-password)"
export LASTSEEN_DEV_INSECURE_COOKIES=1      # lets the login cookie work over http://localhost
go run ./cmd/lastseen serve &               # http://127.0.0.1:8080
go run ./cmd/lastseen worker
make test                                   # go vet + go test
```

The Tailwind standalone CLI (no Node) is downloaded to `tools/tailwindcss`:
<https://github.com/tailwindlabs/tailwindcss/releases> (v3.4.x, `tailwindcss-linux-x64`).
The compiled CSS is committed, so building the Go binary does not need it.

## Configuration

All settings are environment variables (see `deploy/lastseen.env.example`).

| Variable | Default | Purpose |
| --- | --- | --- |
| `LASTSEEN_ADMIN_PASSWORD_HASH` | required for `serve` | bcrypt hash of the admin password |
| `LASTSEEN_ADDR` | `127.0.0.1:8080` | web listen address (keep on loopback behind Caddy) |
| `LASTSEEN_DB` | `./data/lastseen.db` | SQLite file |
| `LASTSEEN_BASE_URL` | `https://status.dhruvp.tech` | link used in emails |
| `LASTSEEN_SMTP_HOST` / `_PORT` / `_USER` / `_PASS` / `_FROM` / `_TO` | port 587 | SMTP (587 = STARTTLS, 465 = TLS) |
| `LASTSEEN_CHECK_INTERVAL` | `20m` | gap between scheduled checks |
| `LASTSEEN_BACKUP_KEEP` | `14` | snapshots retained |
| `LASTSEEN_DEV_ALLOW_PRIVATE` | unset | **dev only**: disables the SSRF guard |
| `LASTSEEN_DEV_INSECURE_COOKIES` | unset | **dev only**: session cookie without `Secure` |

SMTP credentials exist only in the environment. They are never written to the database, logs or pages.

## Deploying to the VPS (Ubuntu/Debian, 1 vCPU / 1 GB is plenty)

1. **DNS:** point an `A`/`AAAA` record for `status.dhruvp.tech` at the server. Open ports 80 and 443.
2. **Provision** (as root, once): copy the `deploy/` folder to the server and run `sudo ./install.sh`.
   It creates the `lastseen` user, `/opt/lastseen`, `/etc/lastseen/lastseen.env`, and installs and enables
   the systemd units and the backup timer.
3. **Configure:** generate a hash with `/opt/lastseen/lastseen hash-password` (after the first deploy) or
   locally, then edit `/etc/lastseen/lastseen.env` (password hash, SMTP, base URL).
4. **Caddy:** install it (<https://caddyserver.com/docs/install#debian-ubuntu-raspbian>), copy
   `deploy/Caddyfile` to `/etc/caddy/Caddyfile`, `sudo systemctl reload caddy`. HTTPS is automatic.
5. **Ship the binary** from your machine: `deploy/deploy.sh user@server`. It runs the tests, builds a
   static Linux binary, snapshots the database, swaps the binary atomically, restarts both services and
   checks `/healthz`.
6. Open `https://status.dhruvp.tech/login`, add monitors, then **Notifications → Send test email**.

Services restart automatically (`Restart=always`, 5 s delay), run as an unprivileged user with a
sandboxed filesystem, and are capped at 200 MB RAM each.

## Operations

- **Logs:** JSON to journald. `journalctl -u lastseen-worker -f`, `journalctl -u lastseen-web -f`.
- **Health:** `curl -fsS https://status.dhruvp.tech/healthz` → `ok` (200) when the database answers
  and the worker heartbeat is fresh; 503 otherwise. Point an external monitor at it so you also learn
  when the whole server (and therefore Last Seen itself) is down.
- **Backups:** `lastseen-backup.timer` writes `/var/lib/lastseen/backups/lastseen-<timestamp>.db` daily at
  ~03:30 (mode 0600, newest 14 kept). Check with `systemctl list-timers lastseen-backup.timer`.
  Copy them off the machine, e.g. `rsync -a server:/var/lib/lastseen/backups/ ~/lastseen-backups/`
  from a cron job on another machine.
- **Restore:** `sudo systemctl stop lastseen-web lastseen-worker`, then
  `sudo -u lastseen cp /var/lib/lastseen/backups/<file>.db /var/lib/lastseen/lastseen.db`,
  delete any `lastseen.db-wal`/`-shm` files, then start both services.
- **Upgrade:** `deploy/deploy.sh user@server` again. Migrations run automatically at startup.
- **Data retention:** raw checks older than 120 days are purged nightly.

## Security notes

- Passwords: bcrypt (cost 12), minimum 12 characters. Login is limited to 5 failures per 15 minutes
  per client address. If no hash is configured the server refuses to start.
- Sessions: random 32-byte tokens, only a SHA-256 hash is stored, 30-day expiry, cookie is
  `HttpOnly; Secure; SameSite=Lax`. Every state-changing request needs a CSRF token and a same-origin
  `Origin`/`Referer`. Logout deletes the session server-side.
- SSRF: monitor URLs must be http(s) with no credentials. Loopback, private (RFC 1918), link-local,
  cloud metadata (`169.254.169.254`, `metadata.google.internal`), CGNAT, multicast and other reserved ranges
  are rejected when saving **and** again on every connection the checker makes, after DNS resolution,
  including each redirect hop (max 5, http/https only). That defeats DNS rebinding.
- Checker reads at most 64 KB of any response and never follows proxy environment variables.
- Public page: shows only monitors marked public, and never raw error text, URLs or causes.
  The optional per-incident note is the only free text shown.
- Strict CSP (`script-src 'self'`, no inline script or style), `X-Frame-Options: DENY`, `nosniff`.
  All fonts and scripts are self-hosted; the browser contacts no third party.
- SMTP password is never rendered, logged or stored in the database; the username is masked in the UI.

## Layout

```
cmd/lastseen/          entry point and subcommands
internal/config        environment configuration
internal/database      SQLite (WAL), migrations, queries, backups
internal/monitor       URL validation, SSRF-guarded HTTP checker
internal/scheduler     due-monitor loop, bounded concurrency, retention, heartbeat
internal/incidents     outage/recovery state machine, transactional apply
internal/notifications outbox processor, SMTP sender, email text
internal/auth          password hashing, sessions, CSRF, rate limiting
internal/web           handlers, templates, static assets, SVG charts
deploy/                systemd units, Caddyfile, env template, install/deploy scripts
```
