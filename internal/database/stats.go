package database

import (
	"context"
	"database/sql"
	"time"
)

// Window aggregates checks in [From, To) (unix ms).
type Window struct {
	Total, Up int
	AvgMS     sql.NullFloat64 // average response time of successful checks
}

// UptimePct returns uptime in percent, or false when there is no data.
func (w Window) UptimePct() (float64, bool) {
	if w.Total == 0 {
		return 0, false
	}
	return float64(w.Up) * 100 / float64(w.Total), true
}

func (db *DB) CheckWindow(ctx context.Context, from, to int64) (Window, error) {
	var w Window
	var up sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT COUNT(*), SUM(ok), AVG(CASE WHEN ok=1 THEN response_ms END)
		FROM checks WHERE checked_at >= ? AND checked_at < ?`, from, to).Scan(&w.Total, &up, &w.AvgMS)
	w.Up = int(up.Int64)
	return w, err
}

func (db *DB) OpenIncidentCount(ctx context.Context) (n int, err error) {
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM incidents WHERE resolved_at IS NULL`).Scan(&n)
	return
}

// ServiceRow is a monitor plus its recent stats for the overview table.
type ServiceRow struct {
	*Monitor
	Uptime24h      Window
	LastResponseMS sql.NullInt64
}

func (db *DB) ServiceRows(ctx context.Context) ([]ServiceRow, error) {
	ms, err := db.ListMonitors(ctx)
	if err != nil {
		return nil, err
	}
	since := time.Now().Add(-24 * time.Hour).UnixMilli()
	wins := map[int64]Window{}
	rows, err := db.QueryContext(ctx, `SELECT monitor_id, COUNT(*), SUM(ok) FROM checks WHERE checked_at >= ? GROUP BY monitor_id`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var w Window
		if err := rows.Scan(&id, &w.Total, &w.Up); err != nil {
			rows.Close()
			return nil, err
		}
		wins[id] = w
	}
	rows.Close()
	last := map[int64]int64{}
	rows, err = db.QueryContext(ctx, `SELECT monitor_id, response_ms FROM checks
		WHERE id IN (SELECT MAX(id) FROM checks WHERE ok=1 GROUP BY monitor_id)`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var ms sql.NullInt64
		if err := rows.Scan(&id, &ms); err != nil {
			rows.Close()
			return nil, err
		}
		if ms.Valid {
			last[id] = ms.Int64
		}
	}
	rows.Close()
	out := make([]ServiceRow, len(ms))
	for i, m := range ms {
		out[i] = ServiceRow{Monitor: m, Uptime24h: wins[m.ID]}
		if v, ok := last[m.ID]; ok {
			out[i].LastResponseMS = sql.NullInt64{Int64: v, Valid: true}
		}
	}
	return out, nil
}
