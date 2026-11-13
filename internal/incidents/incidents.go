// Package incidents decides when a monitor is down or recovered and records it atomically.
package incidents

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/dhruvp/lastseen/internal/database"
	"github.com/dhruvp/lastseen/internal/monitor"
)

type Action int

const (
	None    Action = iota
	Open           // failure confirmed: open an incident, queue "down" email
	Resolve        // recovered: close the incident, queue "recovered" email
)

// State is the monitor state the decision is based on.
type State struct {
	Monitor    string // "unknown" | "up" | "down"
	FailStreak int    // consecutive failed attempts not yet confirmed
	MaxRetries int
}

type Decision struct {
	NewState  string
	NewStreak int
	Action    Action
	Retry     bool // schedule the next attempt after retry_delay instead of the normal interval
}

// Evaluate is a pure function: given the current state and one attempt's outcome,
// it returns what to do. An outage is confirmed only when the first attempt and all
// MaxRetries retries fail consecutively.
func Evaluate(s State, ok bool) Decision {
	if ok {
		d := Decision{NewState: "up"}
		if s.Monitor == "down" {
			d.Action = Resolve
		}
		return d
	}
	if s.Monitor == "down" {
		return Decision{NewState: "down"} // already down: no retries, no new incident
	}
	streak := s.FailStreak + 1
	if streak > s.MaxRetries {
		return Decision{NewState: "down", Action: Open}
	}
	return Decision{NewState: s.Monitor, NewStreak: streak, Retry: true}
}

// Outcome describes what Apply did, for logging and tests.
type Outcome struct {
	Decision   Decision
	IncidentID int64
	NextCheck  time.Time
	Skipped    bool // monitor was deleted or disabled while the check ran
}

// Apply records one check attempt and its consequences in a single transaction:
// the check row, monitor state/schedule, and any incident + notification outbox row.
func Apply(ctx context.Context, db *database.DB, monitorID int64, res monitor.Result, now time.Time, interval time.Duration) (Outcome, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Outcome{}, err
	}
	defer tx.Rollback()

	var st State
	var delayS int
	var enabled bool
	err = tx.QueryRowContext(ctx, `SELECT state, fail_streak, max_retries, retry_delay_s, enabled FROM monitors WHERE id=?`, monitorID).
		Scan(&st.Monitor, &st.FailStreak, &st.MaxRetries, &delayS, &enabled)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !enabled) {
		return Outcome{Skipped: true}, nil
	}
	if err != nil {
		return Outcome{}, err
	}

	d := Evaluate(st, res.OK)
	nowMS := now.UnixMilli()
	next := now.Add(interval)
	if d.Retry {
		next = now.Add(time.Duration(delayS) * time.Second)
	}

	var code, ms any
	if res.StatusCode != 0 {
		code = res.StatusCode
	}
	if res.ResponseMS > 0 || res.OK {
		ms = res.ResponseMS
	}
	var errText any
	if res.Error != "" {
		errText = res.Error
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO checks(monitor_id, checked_at, ok, status_code, response_ms, attempts, error)
		VALUES (?,?,?,?,?,?,?)`, monitorID, nowMS, res.OK, code, ms, st.FailStreak+1, errText); err != nil {
		return Outcome{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE monitors SET state=?, fail_streak=?, last_checked_at=?, next_check_at=? WHERE id=?`,
		d.NewState, d.NewStreak, nowMS, next.UnixMilli(), monitorID); err != nil {
		return Outcome{}, err
	}

	out := Outcome{Decision: d, NextCheck: next}
	switch d.Action {
	case Open:
		cause := res.Error
		if cause == "" {
			cause = "check failed"
		}
		r, err := tx.ExecContext(ctx, `INSERT INTO incidents(monitor_id, started_at, cause) VALUES (?,?,?)`, monitorID, nowMS, cause)
		if err != nil {
			return Outcome{}, err
		}
		out.IncidentID, _ = r.LastInsertId()
		if err := queue(ctx, tx, out.IncidentID, "down", nowMS); err != nil {
			return Outcome{}, err
		}
	case Resolve:
		err := tx.QueryRowContext(ctx, `UPDATE incidents SET resolved_at=? WHERE monitor_id=? AND resolved_at IS NULL RETURNING id`,
			nowMS, monitorID).Scan(&out.IncidentID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Outcome{}, err
		}
		if out.IncidentID != 0 {
			if err := queue(ctx, tx, out.IncidentID, "recovered", nowMS); err != nil {
				return Outcome{}, err
			}
		}
	}
	return out, tx.Commit()
}

// queue inserts an outbox row; UNIQUE(incident_id, kind) makes duplicates impossible.
func queue(ctx context.Context, tx *sql.Tx, incidentID int64, kind string, nowMS int64) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notifications(incident_id, kind, status, next_attempt_at, created_at)
		VALUES (?,?, 'pending', ?, ?)`, incidentID, kind, nowMS, nowMS)
	return err
}
