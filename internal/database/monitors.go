package database

import (
	"context"
	"database/sql"
	"errors"
)

type Monitor struct {
	ID            int64
	Name, Slug    string
	URL, Method   string
	ExpectedStat  string
	TimeoutMS     int
	RetryDelayS   int // seconds between retries, 10..1800
	MaxRetries    int // 0..5
	FailStreak    int
	Enabled       bool
	IsPublic      bool
	SortOrder     int
	State         string
	LastCheckedAt sql.NullInt64
	NextCheckAt   int64
	CreatedAt     int64
	UpdatedAt     int64
}

var ErrNotFound = errors.New("not found")
var ErrSlugTaken = errors.New("slug already in use")

const monitorCols = `id,name,slug,url,method,expected_status,timeout_ms,retry_delay_s,max_retries,fail_streak,enabled,is_public,sort_order,state,last_checked_at,next_check_at,created_at,updated_at`

func scanMonitor(r interface{ Scan(...any) error }) (*Monitor, error) {
	var m Monitor
	err := r.Scan(&m.ID, &m.Name, &m.Slug, &m.URL, &m.Method, &m.ExpectedStat, &m.TimeoutMS, &m.RetryDelayS, &m.MaxRetries, &m.FailStreak,
		&m.Enabled, &m.IsPublic, &m.SortOrder, &m.State, &m.LastCheckedAt, &m.NextCheckAt, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &m, err
}

func (db *DB) ListMonitors(ctx context.Context) ([]*Monitor, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+monitorCols+` FROM monitors ORDER BY sort_order, name`)
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

func (db *DB) GetMonitor(ctx context.Context, id int64) (*Monitor, error) {
	return scanMonitor(db.QueryRowContext(ctx, `SELECT `+monitorCols+` FROM monitors WHERE id=?`, id))
}

func (db *DB) SlugExists(ctx context.Context, slug string, exceptID int64) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM monitors WHERE slug=? AND id<>?`, slug, exceptID).Scan(&n)
	return n > 0, err
}

// CreateMonitor inserts m (new monitors are due immediately) and sets m.ID.
func (db *DB) CreateMonitor(ctx context.Context, m *Monitor) error {
	now := Now()
	res, err := db.ExecContext(ctx, `INSERT INTO monitors
		(name,slug,url,method,expected_status,timeout_ms,retry_delay_s,max_retries,enabled,is_public,sort_order,state,next_check_at,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?, 'unknown', ?, ?, ?)`,
		m.Name, m.Slug, m.URL, m.Method, m.ExpectedStat, m.TimeoutMS, m.RetryDelayS, m.MaxRetries, m.Enabled, m.IsPublic, m.SortOrder, now, now, now)
	if err != nil {
		return mapUnique(err)
	}
	m.ID, _ = res.LastInsertId()
	return nil
}

// UpdateMonitor updates editable fields. Changing the URL resets state so the next check starts fresh.
func (db *DB) UpdateMonitor(ctx context.Context, m *Monitor) error {
	res, err := db.ExecContext(ctx, `UPDATE monitors SET name=?,slug=?,url=?,method=?,expected_status=?,timeout_ms=?,retry_delay_s=?,max_retries=?,
		enabled=?,is_public=?,sort_order=?,updated_at=?,
		next_check_at=CASE WHEN url<>? OR (enabled=0 AND ?=1) THEN ? ELSE next_check_at END
		WHERE id=?`,
		m.Name, m.Slug, m.URL, m.Method, m.ExpectedStat, m.TimeoutMS, m.RetryDelayS, m.MaxRetries, m.Enabled, m.IsPublic, m.SortOrder, Now(),
		m.URL, m.Enabled, Now(), m.ID)
	if err != nil {
		return mapUnique(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) SetMonitorEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := db.ExecContext(ctx, `UPDATE monitors SET enabled=?, updated_at=?,
		next_check_at=CASE WHEN ?=1 AND enabled=0 THEN ? ELSE next_check_at END WHERE id=?`,
		enabled, Now(), enabled, Now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) DeleteMonitor(ctx context.Context, id int64) error {
	res, err := db.ExecContext(ctx, `DELETE FROM monitors WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func mapUnique(err error) error {
	if err != nil && containsFold(err.Error(), "UNIQUE constraint failed: monitors.slug") {
		return ErrSlugTaken
	}
	return err
}
