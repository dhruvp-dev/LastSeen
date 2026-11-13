package database

import (
	"context"
	"database/sql"
	"errors"
)

// Icon is a cached site icon. Data is empty when the site had none.
type Icon struct {
	ForURL      string // monitor URL the icon was fetched for; a changed URL invalidates the cache
	ContentType string
	Data        []byte
	FetchedAt   int64 // unix ms
}

// GetIcon returns the cached icon row for a monitor, or ErrNotFound if it was never fetched.
func (db *DB) GetIcon(ctx context.Context, monitorID int64) (*Icon, error) {
	var ic Icon
	err := db.QueryRowContext(ctx, `SELECT for_url, content_type, data, fetched_at FROM monitor_icons WHERE monitor_id = ?`, monitorID).
		Scan(&ic.ForURL, &ic.ContentType, &ic.Data, &ic.FetchedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ic, nil
}

// SaveIcon stores (or replaces) a monitor's icon. Pass empty Data to remember that none was found.
func (db *DB) SaveIcon(ctx context.Context, monitorID int64, ic Icon) error {
	var data any
	if len(ic.Data) > 0 {
		data = ic.Data
	}
	_, err := db.ExecContext(ctx, `INSERT INTO monitor_icons (monitor_id, for_url, content_type, data, fetched_at) VALUES (?,?,?,?,?)
		ON CONFLICT(monitor_id) DO UPDATE SET for_url=excluded.for_url, content_type=excluded.content_type, data=excluded.data, fetched_at=excluded.fetched_at`,
		monitorID, ic.ForURL, ic.ContentType, data, ic.FetchedAt)
	return err
}
