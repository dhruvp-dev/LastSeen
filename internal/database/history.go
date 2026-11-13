package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// DayStat aggregates one UTC day of checks for one monitor.
type DayStat struct {
	Day       string // YYYY-MM-DD (UTC)
	Total, Up int
	AvgMS     sql.NullFloat64 // average response time of successful checks
}

func (d DayStat) UptimePct() (float64, bool) {
	if d.Total == 0 {
		return 0, false
	}
	return float64(d.Up) * 100 / float64(d.Total), true
}

// DailyStatsAll returns per-monitor, per-UTC-day aggregates since fromMS, ordered by day.
func (db *DB) DailyStatsAll(ctx context.Context, fromMS int64) (map[int64][]DayStat, error) {
	rows, err := db.QueryContext(ctx, `SELECT monitor_id, date(checked_at/1000,'unixepoch') d, COUNT(*), SUM(ok),
		AVG(CASE WHEN ok=1 THEN response_ms END) FROM checks WHERE checked_at >= ? GROUP BY monitor_id, d ORDER BY d`, fromMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]DayStat{}
	for rows.Next() {
		var id int64
		var d DayStat
		if err := rows.Scan(&id, &d.Day, &d.Total, &d.Up, &d.AvgMS); err != nil {
			return nil, err
		}
		out[id] = append(out[id], d)
	}
	return out, rows.Err()
}

// SumDays totals day stats whose Day >= fromDay (YYYY-MM-DD).
func SumDays(days []DayStat, fromDay string) Window {
	var w Window
	for _, d := range days {
		if d.Day >= fromDay {
			w.Total += d.Total
			w.Up += d.Up
		}
	}
	return w
}

// DayString formats t as a UTC YYYY-MM-DD.
func DayString(t time.Time) string { return t.UTC().Format("2006-01-02") }

// ---- incidents ----

type IncidentRow struct {
	ID          int64
	MonitorID   int64
	MonitorName string
	StartedAt   int64
	ResolvedAt  sql.NullInt64
	Cause, Note sql.NullString
}

// Incidents lists incidents newest first. When publicOnly is set, only incidents of public
// monitors are returned. Open incidents are always included; resolved ones only if started since sinceMS.
func (db *DB) Incidents(ctx context.Context, publicOnly bool, sinceMS int64, limit int) ([]IncidentRow, error) {
	q := `SELECT i.id, i.monitor_id, m.name, i.started_at, i.resolved_at, i.cause, i.note
		FROM incidents i JOIN monitors m ON m.id = i.monitor_id
		WHERE (i.resolved_at IS NULL OR i.started_at >= ?)`
	if publicOnly {
		q += ` AND m.is_public = 1`
	}
	q += ` ORDER BY i.started_at DESC LIMIT ?`
	rows, err := db.QueryContext(ctx, q, sinceMS, limit)
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

// ---- settings & worker liveness ----

func (db *DB) GetSetting(ctx context.Context, key, def string) (string, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	return v, err
}

func (db *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// WorkerStaleAfter is how old the heartbeat may be before monitoring is considered paused.
const WorkerStaleAfter = 2 * time.Minute

// WorkerAlive reports whether the worker heartbeat is recent, and the heartbeat time if known.
func (db *DB) WorkerAlive(ctx context.Context, now time.Time) (bool, time.Time) {
	v, err := db.GetMeta(ctx, "worker_heartbeat")
	if err != nil || v == "" {
		return false, time.Time{}
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return false, time.Time{}
	}
	t := time.UnixMilli(ms)
	return now.Sub(t) < WorkerStaleAfter, t
}

// NotificationRow is a delivery-log entry.
type NotificationRow struct {
	ID          int64
	IncidentID  int64
	MonitorName string
	Kind        string
	Status      string
	Attempts    int
	LastError   sql.NullString
	CreatedAt   int64
	SentAt      sql.NullInt64
}

func (db *DB) Notifications(ctx context.Context, limit int) ([]NotificationRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT n.id, n.incident_id, m.name, n.kind, n.status, n.attempts, n.last_error, n.created_at, n.sent_at
		FROM notifications n JOIN incidents i ON i.id=n.incident_id JOIN monitors m ON m.id=i.monitor_id
		ORDER BY n.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationRow
	for rows.Next() {
		var r NotificationRow
		if err := rows.Scan(&r.ID, &r.IncidentID, &r.MonitorName, &r.Kind, &r.Status, &r.Attempts, &r.LastError, &r.CreatedAt, &r.SentAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeriesPoint is one raw check for charting.
type SeriesPoint struct {
	At int64
	OK bool
	MS sql.NullInt64
}

// Series returns raw checks since fromMS in time order. monitorID 0 means all monitors.
func (db *DB) Series(ctx context.Context, monitorID, fromMS int64) ([]SeriesPoint, error) {
	q := `SELECT checked_at, ok, response_ms FROM checks WHERE checked_at >= ?`
	args := []any{fromMS}
	if monitorID != 0 {
		q += ` AND monitor_id = ?`
		args = append(args, monitorID)
	}
	rows, err := db.QueryContext(ctx, q+` ORDER BY checked_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesPoint
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.At, &p.OK, &p.MS); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
