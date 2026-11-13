package notifications

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log/slog"
	"time"

	"github.com/dhruvp/lastseen/internal/database"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

const (
	MaxAttempts    = 6
	baseBackoff    = 30 * time.Second
	maxBackoff     = time.Hour
	settingEnabled = "notifications_enabled"
)

// Backoff returns the wait before the next attempt after `attempts` failures (1-based).
func Backoff(attempts int) time.Duration {
	d := baseBackoff
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= maxBackoff {
			return maxBackoff
		}
	}
	return d
}

type Processor struct {
	DB      *database.DB
	Sender  Sender
	Log     *slog.Logger
	BaseURL string
	Every   time.Duration
}

func NewProcessor(db *database.DB, s Sender, log *slog.Logger, baseURL string) *Processor {
	return &Processor{DB: db, Sender: s, Log: log, BaseURL: baseURL, Every: 10 * time.Second}
}

// Run drains the outbox until ctx is cancelled.
func (p *Processor) Run(ctx context.Context) {
	t := time.NewTicker(p.Every)
	defer t.Stop()
	for {
		if err := p.Process(ctx, time.Now()); err != nil && ctx.Err() == nil {
			p.Log.Error("notifications", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type job struct {
	id, incidentID int64
	kind           string
	attempts       int
	monitor, url   string
	startedAt      int64
	resolvedAt     sql.NullInt64
	cause          sql.NullString
}

// Process sends every notification due at now. It is safe to call repeatedly; each row is
// sent at most once because status moves to 'sent' before the next pass and UNIQUE(incident_id, kind)
// prevents duplicates being queued in the first place.
func (p *Processor) Process(ctx context.Context, now time.Time) error {
	rows, err := p.DB.QueryContext(ctx, `SELECT n.id, n.incident_id, n.kind, n.attempts, m.name, m.url, i.started_at, i.resolved_at, i.cause
		FROM notifications n JOIN incidents i ON i.id=n.incident_id JOIN monitors m ON m.id=i.monitor_id
		WHERE n.status='pending' AND n.next_attempt_at <= ? ORDER BY n.id LIMIT 20`, now.UnixMilli())
	if err != nil {
		return err
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.incidentID, &j.kind, &j.attempts, &j.monitor, &j.url, &j.startedAt, &j.resolvedAt, &j.cause); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if len(jobs) == 0 {
		return nil
	}
	enabled, err := p.DB.GetSetting(ctx, settingEnabled, "1")
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return nil
		}
		if enabled != "1" {
			if err := p.finish(ctx, j, now, "failed", "notifications disabled in settings"); err != nil {
				return err
			}
			continue
		}
		subject, body := Compose(j.kind, j.monitor, j.url, j.cause.String, j.startedAt, j.resolvedAt, p.BaseURL)
		if err := p.Sender.Send(ctx, subject, body); err != nil {
			if err := p.fail(ctx, j, now, err); err != nil {
				return err
			}
			continue
		}
		if err := p.finish(ctx, j, now, "sent", ""); err != nil {
			return err
		}
		p.Log.Info("notification sent", "kind", j.kind, "monitor", j.monitor, "incident", j.incidentID)
	}
	return nil
}

func (p *Processor) finish(ctx context.Context, j job, now time.Time, status, errText string) error {
	var sent any
	if status == "sent" {
		sent = now.UnixMilli()
	}
	var e any
	if errText != "" {
		e = errText
	}
	_, err := p.DB.ExecContext(ctx, `UPDATE notifications SET status=?, attempts=attempts+1, last_error=?, sent_at=? WHERE id=? AND status='pending'`,
		status, e, sent, j.id)
	return err
}

func (p *Processor) fail(ctx context.Context, j job, now time.Time, sendErr error) error {
	attempts := j.attempts + 1
	msg := sendErr.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	if attempts >= MaxAttempts {
		p.Log.Error("notification failed permanently", "kind", j.kind, "monitor", j.monitor, "attempts", attempts, "err", msg)
		_, err := p.DB.ExecContext(ctx, `UPDATE notifications SET status='failed', attempts=?, last_error=? WHERE id=?`, attempts, msg, j.id)
		return err
	}
	next := now.Add(Backoff(attempts))
	p.Log.Warn("notification attempt failed, will retry", "kind", j.kind, "monitor", j.monitor, "attempts", attempts, "retry_at", next.UTC().Format(time.RFC3339), "err", msg)
	_, err := p.DB.ExecContext(ctx, `UPDATE notifications SET attempts=?, last_error=?, next_attempt_at=? WHERE id=?`, attempts, msg, next.UnixMilli(), j.id)
	return err
}

// Compose renders the subject and plain-text body for an incident email.
func Compose(kind, monitor, url, cause string, startedMS int64, resolved sql.NullInt64, baseURL string) (subject, body string) {
	started := time.UnixMilli(startedMS).UTC()
	switch kind {
	case "recovered":
		end := time.Now().UTC()
		if resolved.Valid {
			end = time.UnixMilli(resolved.Int64).UTC()
		}
		subject = fmt.Sprintf("[Last Seen] %s is back up", monitor)
		body = fmt.Sprintf("%s has recovered.\n\nURL:       %s\nDown at:   %s\nUp at:     %s\nDowntime:  %s\n\nStatus page: %s\n",
			monitor, url, started.Format(stamp), end.Format(stamp), HumanDuration(end.Sub(started)), baseURL)
	default:
		if cause == "" {
			cause = "check failed"
		}
		subject = fmt.Sprintf("[Last Seen] %s is down", monitor)
		body = fmt.Sprintf("%s appears to be down.\n\nURL:      %s\nSince:    %s\nReason:   %s\n\nYou will get another email when it recovers.\nStatus page: %s\n",
			monitor, url, started.Format(stamp), cause, baseURL)
	}
	return subject, body
}

const stamp = "2 Jan 2006, 15:04:05 UTC"

// HumanDuration renders d as e.g. "4m", "2h 5m", "3d 1h".
func HumanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}
