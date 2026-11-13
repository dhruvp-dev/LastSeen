CREATE TABLE monitors (
  id              INTEGER PRIMARY KEY,
  name            TEXT    NOT NULL,
  slug            TEXT    NOT NULL UNIQUE,
  url             TEXT    NOT NULL,
  method          TEXT    NOT NULL DEFAULT 'GET' CHECK (method IN ('GET','HEAD')),
  expected_status TEXT    NOT NULL DEFAULT '200-299',
  timeout_ms      INTEGER NOT NULL DEFAULT 10000 CHECK (timeout_ms BETWEEN 1000 AND 60000),
  enabled         INTEGER NOT NULL DEFAULT 1,
  is_public       INTEGER NOT NULL DEFAULT 1,
  sort_order      INTEGER NOT NULL DEFAULT 0,
  state           TEXT    NOT NULL DEFAULT 'unknown' CHECK (state IN ('unknown','up','down')),
  last_checked_at INTEGER,
  next_check_at   INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);
CREATE INDEX idx_monitors_due ON monitors(enabled, next_check_at);

CREATE TABLE checks (
  id          INTEGER PRIMARY KEY,
  monitor_id  INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  checked_at  INTEGER NOT NULL,
  ok          INTEGER NOT NULL,
  status_code INTEGER,
  response_ms INTEGER,
  attempts    INTEGER NOT NULL DEFAULT 1,
  error       TEXT
);
CREATE INDEX idx_checks_monitor_time ON checks(monitor_id, checked_at);
CREATE INDEX idx_checks_time ON checks(checked_at);

CREATE TABLE incidents (
  id          INTEGER PRIMARY KEY,
  monitor_id  INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  started_at  INTEGER NOT NULL,
  resolved_at INTEGER,
  cause       TEXT,
  note        TEXT
);
CREATE UNIQUE INDEX idx_incidents_one_open ON incidents(monitor_id) WHERE resolved_at IS NULL;
CREATE INDEX idx_incidents_started ON incidents(started_at);

CREATE TABLE notifications (
  id              INTEGER PRIMARY KEY,
  incident_id     INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  kind            TEXT    NOT NULL CHECK (kind IN ('down','recovered')),
  status          TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sent','failed')),
  attempts        INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT,
  next_attempt_at INTEGER NOT NULL,
  created_at      INTEGER NOT NULL,
  sent_at         INTEGER,
  UNIQUE (incident_id, kind)
);
CREATE INDEX idx_notifications_pending ON notifications(status, next_attempt_at);

CREATE TABLE sessions (
  token_hash TEXT PRIMARY KEY,
  csrf_token TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);

CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE meta     (key TEXT PRIMARY KEY, value TEXT NOT NULL);
