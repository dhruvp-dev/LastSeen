-- Cached site icons, fetched server-side (the CSP only allows images from our own origin).
-- data NULL means "looked, found nothing", so a site without an icon is not re-fetched on every page view.
CREATE TABLE monitor_icons (
  monitor_id   INTEGER PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE,
  for_url      TEXT    NOT NULL,
  content_type TEXT    NOT NULL DEFAULT '',
  data         BLOB,
  fetched_at   INTEGER NOT NULL
);
