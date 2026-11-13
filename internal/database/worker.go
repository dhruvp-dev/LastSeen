package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// DueMonitors returns enabled monitors whose next check is due at or before nowMS.
func (db *DB) DueMonitors(ctx context.Context, nowMS int64) ([]*Monitor, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+monitorCols+` FROM monitors WHERE enabled=1 AND next_check_at<=? ORDER BY next_check_at`, nowMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Monitor
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// StaggerOverdue spreads monitors that are already overdue (e.g. after downtime)
// across the next spreadMS milliseconds so a restart does not fire every check at once.
func (db *DB) StaggerOverdue(ctx context.Context, nowMS, spreadMS int64) error {
	_, err := db.ExecContext(ctx, `UPDATE monitors SET next_check_at = ? + (abs(random()) % ?) WHERE enabled=1 AND next_check_at<=?`,
		nowMS, spreadMS, nowMS)
	return err
}

func (db *DB) SetMeta(ctx context.Context, key, value string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// GetMeta returns the value, or "" when unset.
func (db *DB) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// Heartbeat records that the worker is alive.
func (db *DB) Heartbeat(ctx context.Context, nowMS int64) error {
	return db.SetMeta(ctx, "worker_heartbeat", strconv.FormatInt(nowMS, 10))
}

// Purge removes checks older than checksBeforeMS and expired sessions.
func (db *DB) Purge(ctx context.Context, checksBeforeMS, nowMS int64) (checks int64, err error) {
	res, err := db.ExecContext(ctx, `DELETE FROM checks WHERE checked_at < ?`, checksBeforeMS)
	if err != nil {
		return 0, err
	}
	checks, _ = res.RowsAffected()
	_, err = db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, nowMS)
	return checks, err
}
