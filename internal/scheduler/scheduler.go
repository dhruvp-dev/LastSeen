// Package scheduler runs recurring monitor checks. Each tick it finds monitors that are due,
// checks them with bounded concurrency and records every attempt through incidents.Apply.
// Retries are scheduled through the database (next_check_at), never by sleeping, so a
// 30-minute retry delay survives worker restarts.
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/dhruvp/lastseen/internal/database"
	"github.com/dhruvp/lastseen/internal/incidents"
	"github.com/dhruvp/lastseen/internal/monitor"
)

// Checker performs one check attempt. *monitor.Checker satisfies it.
type Checker interface {
	Check(ctx context.Context, t monitor.Target) monitor.Result
}

type Scheduler struct {
	DB          *database.DB
	Checker     Checker
	Log         *slog.Logger
	Interval    time.Duration // normal gap between scheduled checks
	TickEvery   time.Duration
	Concurrency int
	Retention   time.Duration
	Now         func() time.Time // injectable clock; defaults to time.Now
}

func New(db *database.DB, c Checker, log *slog.Logger, interval time.Duration) *Scheduler {
	return &Scheduler{DB: db, Checker: c, Log: log, Interval: interval, TickEvery: 15 * time.Second,
		Concurrency: 4, Retention: 120 * 24 * time.Hour, Now: time.Now}
}

// Run blocks until ctx is cancelled, then waits for in-flight checks to finish.
func (s *Scheduler) Run(ctx context.Context) error {
	now := s.Now()
	if err := s.DB.StaggerOverdue(ctx, now.UnixMilli(), 60_000); err != nil {
		return err
	}
	s.Log.Info("scheduler started", "interval", s.Interval.String(), "tick", s.TickEvery.String())
	var lastPurge time.Time
	t := time.NewTicker(s.TickEvery)
	defer t.Stop()
	for {
		now = s.Now()
		if err := s.DB.Heartbeat(ctx, now.UnixMilli()); err != nil {
			s.Log.Error("heartbeat", "err", err)
		}
		if err := s.Tick(ctx, now); err != nil && ctx.Err() == nil {
			s.Log.Error("tick", "err", err)
		}
		if now.Sub(lastPurge) > 24*time.Hour {
			if n, err := s.DB.Purge(ctx, now.Add(-s.Retention).UnixMilli(), now.UnixMilli()); err != nil {
				s.Log.Error("purge", "err", err)
			} else {
				s.Log.Info("purged old data", "checks_deleted", n)
				lastPurge = now
			}
		}
		select {
		case <-ctx.Done():
			s.Log.Info("scheduler stopped")
			return nil
		case <-t.C:
		}
	}
}

// Tick checks every monitor due at now and waits for all of them to finish.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) error {
	due, err := s.DB.DueMonitors(ctx, now.UnixMilli())
	if err != nil {
		return err
	}
	sem := make(chan struct{}, max(1, s.Concurrency))
	var wg sync.WaitGroup
	for _, m := range due {
		sem <- struct{}{}
		wg.Add(1)
		go func(m *database.Monitor) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if v := recover(); v != nil {
					s.Log.Error("check panicked", "monitor", m.ID, "panic", v)
				}
			}()
			s.checkOne(ctx, m)
		}(m)
	}
	wg.Wait()
	return nil
}

func (s *Scheduler) checkOne(ctx context.Context, m *database.Monitor) {
	var res monitor.Result
	matcher, err := monitor.ParseExpectedStatus(m.ExpectedStat)
	if err != nil {
		res = monitor.Result{Error: "invalid expected status configuration"}
	} else {
		res = s.Checker.Check(ctx, monitor.Target{
			URL: m.URL, Method: m.Method, Timeout: time.Duration(m.TimeoutMS) * time.Millisecond, Expected: matcher,
		})
	}
	if ctx.Err() != nil {
		return // shutting down mid-check: do not record a bogus failure
	}
	// Record at the completion time of the attempt, not the tick start.
	out, err := incidents.Apply(ctx, s.DB, m.ID, res, s.Now(), s.Interval)
	if err != nil {
		s.Log.Error("apply result", "monitor", m.ID, "err", err)
		return
	}
	if out.Skipped {
		return
	}
	s.Log.Info("checked", "monitor", m.Name, "ok", res.OK, "status", res.StatusCode, "ms", res.ResponseMS,
		"error", res.Error, "retry_scheduled", out.Decision.Retry, "next", out.NextCheck.UTC().Format(time.RFC3339))
	switch out.Decision.Action {
	case incidents.Open:
		s.Log.Warn("incident opened", "monitor", m.Name, "incident", out.IncidentID)
	case incidents.Resolve:
		s.Log.Info("incident resolved", "monitor", m.Name, "incident", out.IncidentID)
	}
}
