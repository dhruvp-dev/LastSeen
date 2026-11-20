# Deploying Last Seen on Heroku with Docker & Turso DB

This guide walks through deploying Last Seen to Heroku using Docker containers and [Turso](https://turso.tech) (libSQL) as the persistent database.

---

## 1. Prerequisites from Your Side

You will need:
1. The **Heroku CLI** logged in (`heroku login`).
2. A free **Turso** account (`turso auth signup` or `turso auth login`).

---

## 2. Set Up Turso Database

1. Create a database:
   ```bash
   turso db create lastseen
   ```
2. Get the database URL:
   ```bash
   turso db show lastseen --url
   # Output looks like: libsql://lastseen-[your-org].turso.io
   ```
3. Create an authentication token:
   ```bash
   turso db tokens create lastseen
   # Output is your secret token string
   ```

---

## 3. Create & Configure the Heroku App

1. Create the Heroku app:
   ```bash
   heroku create <your-app-name>
   ```
2. Set the stack to `container` so Heroku builds and runs via Docker:
   ```bash
   heroku stack:set container -a <your-app-name>
   ```
3. Generate your admin password hash locally:
   ```bash
   go run ./cmd/lastseen hash-password
   ```
4. Configure required environment variables:
   ```bash
   # Turso Database
   heroku config:set LASTSEEN_TURSO_URL="libsql://lastseen-[your-org].turso.io" -a <your-app-name>
   heroku config:set LASTSEEN_TURSO_AUTH_TOKEN="<your-turso-token>" -a <your-app-name>

   # Base URL & Admin Password
   heroku config:set LASTSEEN_BASE_URL="https://<your-app-name>.herokuapp.com" -a <your-app-name>
   heroku config:set LASTSEEN_ADMIN_PASSWORD_HASH='<bcrypt-hash-from-step-3>' -a <your-app-name>

   # Optional SMTP (for down/recovered email alerts)
   heroku config:set LASTSEEN_SMTP_HOST="smtp.mailgun.org" -a <your-app-name>
   heroku config:set LASTSEEN_SMTP_PORT="587" -a <your-app-name>
   heroku config:set LASTSEEN_SMTP_USER="postmaster@yourdomain.com" -a <your-app-name>
   heroku config:set LASTSEEN_SMTP_PASS="secret" -a <your-app-name>
   heroku config:set LASTSEEN_SMTP_FROM="lastseen@yourdomain.com" -a <your-app-name>
   heroku config:set LASTSEEN_SMTP_TO="admin@yourdomain.com" -a <your-app-name>
   ```

---

## 4. Deploy

Push your repository to Heroku:
```bash
git push heroku main
```

Heroku uses [heroku.yml](../heroku.yml) to build the [Dockerfile](../Dockerfile) and launch:
- `web`: the HTTP status page and admin dashboard (automatically listens on Heroku's dynamic `$PORT`).
- `worker`: the health checker and alert scheduler.

Ensure both dynos are running:
```bash
heroku ps:scale web=1 worker=1 -a <your-app-name>
```

Check the logs to verify everything started properly:
```bash
heroku logs --tail -a <your-app-name>
```
