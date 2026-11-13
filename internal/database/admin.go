package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type CheckRow struct {
	ID, CheckedAt          int64
	OK                     bool
	StatusCode, ResponseMS sql.NullInt64
	Attempts               int
	Error                  sql.NullString
}

// ChecksPage returns checks newest first. beforeID=0 starts at the latest; otherwise only rows with id < beforeID.
func (db *DB) ChecksPage(ctx context.Context, monitorID, beforeID int64, limit int) ([]CheckRow, error) {
	if beforeID <= 0 {
		beforeID = 1<<62 - 1
	}
	rows, err := db.QueryContext(ctx, `SELECT id, checked_at, ok, status_code, response_ms, attempts, error
		FROM checks WHERE monitor_id=? AND id<? ORDER BY id DESC LIMIT ?`, monitorID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckRow
	for rows.Next() {
		var c CheckRow
		if err := rows.Scan(&c.ID, &c.CheckedAt, &c.OK, &c.StatusCode, &c.ResponseMS, &c.Attempts, &c.Error); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MonitorWindow aggregates one monitor's checks in [fromMS, toMS).
func (db *DB) MonitorWindow(ctx context.Context, monitorID, fromMS, toMS int64) (Window, error) {
	var w Window
	var up sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT COUNT(*), SUM(ok), AVG(CASE WHEN ok=1 THEN response_ms END)
		FROM checks WHERE monitor_id=? AND checked_at >= ? AND checked_at < ?`, monitorID, fromMS, toMS).Scan(&w.Total, &up, &w.AvgMS)
	w.Up = int(up.Int64)
	return w, err
}

// IncidentsForMonitor lists one monitor's incidents, newest first.
func (db *DB) IncidentsForMonitor(ctx context.Context, monitorID int64, limit int) ([]IncidentRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT i.id, i.monitor_id, m.name, i.started_at, i.resolved_at, i.cause, i.note
		FROM incidents i JOIN monitors m ON m.id=i.monitor_id WHERE i.monitor_id=? ORDER BY i.started_at DESC LIMIT ?`, monitorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IncidentRow
	for rows.Next() {
		var r IncidentRow
		if err := rows.Scan(&r.ID, &r.MonitorID, &r.MonitorName, &r.StartedAt, &r.ResolvedAt, &r.Cause, &r.Note); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveIncident manually closes an open incident. The monitor is reset to "unknown" with a
// clean retry streak and queued for an immediate re-check, so if the service is still failing a
// fresh incident opens through the normal retry chain instead of staying silently resolved.
// No "recovered" email is queued: a manual resolve is not evidence of recovery.
func (db *DB) ResolveIncident(ctx context.Context, id int64, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var monitorID int64
	err = tx.QueryRowContext(ctx, `UPDATE incidents SET resolved_at=? WHERE id=? AND resolved_at IS NULL RETURNING monitor_id`, now.UnixMilli(), id).Scan(&monitorID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE monitors SET state='unknown', fail_streak=0, next_check_at=? WHERE id=?`, now.UnixMilli(), monitorID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetIncidentNote sets the public note shown on the status page (empty clears it).
func (db *DB) SetIncidentNote(ctx context.Context, id int64, note string) error {
	var v any
	if note != "" {
		v = note
	}
	res, err := db.ExecContext(ctx, `UPDATE incidents SET note=? WHERE id=?`, v, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DailyStatsFor returns per-UTC-day aggregates for one monitor since fromMS.
func (db *DB) DailyStatsFor(ctx context.Context, monitorID, fromMS int64) ([]DayStat, error) {
	rows, err := db.QueryContext(ctx, `SELECT date(checked_at/1000,'unixepoch') d, COUNT(*), SUM(ok),
		AVG(CASE WHEN ok=1 THEN response_ms END) FROM checks WHERE monitor_id=? AND checked_at >= ? GROUP BY d ORDER BY d`, monitorID, fromMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayStat
	for rows.Next() {
		var d DayStat
		if err := rows.Scan(&d.Day, &d.Total, &d.Up, &d.AvgMS); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
