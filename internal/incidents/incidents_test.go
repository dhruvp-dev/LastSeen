package incidents

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dhruvp/lastseen/internal/database"
	"github.com/dhruvp/lastseen/internal/monitor"
)

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name string
		in   State
		ok   bool
		want Decision
	}{
		{"first success", State{"unknown", 0, 2}, true, Decision{NewState: "up"}},
		{"steady success", State{"up", 0, 2}, true, Decision{NewState: "up"}},
		{"success clears streak, no incident", State{"up", 2, 2}, true, Decision{NewState: "up"}},
		{"first failure retries", State{"up", 0, 2}, false, Decision{NewState: "up", NewStreak: 1, Retry: true}},
		{"second failure retries", State{"up", 1, 2}, false, Decision{NewState: "up", NewStreak: 2, Retry: true}},
		{"third failure opens", State{"up", 2, 2}, false, Decision{NewState: "down", Action: Open}},
		{"unknown state also opens", State{"unknown", 2, 2}, false, Decision{NewState: "down", Action: Open}},
		{"retries=0 opens at once", State{"up", 0, 0}, false, Decision{NewState: "down", Action: Open}},
		{"retries=5 needs six failures", State{"up", 4, 5}, false, Decision{NewState: "up", NewStreak: 5, Retry: true}},
		{"retries=5 sixth opens", State{"up", 5, 5}, false, Decision{NewState: "down", Action: Open}},
		{"down + failure: no new incident", State{"down", 0, 2}, false, Decision{NewState: "down"}},
		{"down + success resolves", State{"down", 0, 2}, true, Decision{NewState: "up", Action: Resolve}},
	}
	for _, c := range cases {
		if got := Evaluate(c.in, c.ok); got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
}

func setup(t *testing.T, retries, delay int) (*database.DB, int64) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m := &database.Monitor{Name: "A", Slug: "a", URL: "https://example.com", Method: "GET", ExpectedStat: "200",
		TimeoutMS: 5000, RetryDelayS: delay, MaxRetries: retries, Enabled: true}
	if err := db.CreateMonitor(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return db, m.ID
}

func count(t *testing.T, db *database.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var (
	fail = monitor.Result{OK: false, StatusCode: 500, ResponseMS: 40, Error: "unexpected status 500"}
	pass = monitor.Result{OK: true, StatusCode: 200, ResponseMS: 80}
	t0   = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	iv   = 20 * time.Minute
)

func TestApplyRetryChainThenIncident(t *testing.T) {
	ctx := context.Background()
	db, id := setup(t, 2, 1800) // 30 minute retry delay
	now := t0

	for i := 0; i < 2; i++ {
		o, err := Apply(ctx, db, id, fail, now, iv)
		if err != nil || !o.Decision.Retry {
			t.Fatalf("attempt %d: %+v err=%v", i+1, o, err)
		}
		if want := now.Add(30 * time.Minute); !o.NextCheck.Equal(want) {
			t.Errorf("next check %v want %v (retry delay)", o.NextCheck, want)
		}
		if n := count(t, db, `SELECT COUNT(*) FROM incidents`); n != 0 {
			t.Fatalf("incident opened too early after %d failures", i+1)
		}
		now = now.Add(30 * time.Minute)
	}
	o, err := Apply(ctx, db, id, fail, now, iv)
	if err != nil || o.Decision.Action != Open || o.IncidentID == 0 {
		t.Fatalf("third failure should open: %+v err=%v", o, err)
	}
	if want := now.Add(iv); !o.NextCheck.Equal(want) {
		t.Errorf("after confirm, next check %v want normal interval %v", o.NextCheck, want)
	}
	m, _ := db.GetMonitor(ctx, id)
	if m.State != "down" || m.FailStreak != 0 {
		t.Errorf("monitor: state=%s streak=%d", m.State, m.FailStreak)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM checks WHERE monitor_id=?`, id); n != 3 {
		t.Errorf("want 3 check rows, got %d", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM notifications WHERE kind='down' AND status='pending'`); n != 1 {
		t.Errorf("want 1 pending down notification, got %d", n)
	}
}

func TestApplyNoDuplicatesWhileDown(t *testing.T) {
	ctx := context.Background()
	db, id := setup(t, 0, 30)
	now := t0
	for i := 0; i < 5; i++ { // five failures in a row, retries=0
		if _, err := Apply(ctx, db, id, fail, now, iv); err != nil {
			t.Fatal(err)
		}
		now = now.Add(iv)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM incidents`); n != 1 {
		t.Errorf("want exactly 1 incident, got %d", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM notifications`); n != 1 {
		t.Errorf("want exactly 1 notification, got %d", n)
	}
}

func TestApplyRecovery(t *testing.T) {
	ctx := context.Background()
	db, id := setup(t, 0, 30)
	Apply(ctx, db, id, fail, t0, iv)
	o, err := Apply(ctx, db, id, pass, t0.Add(iv), iv)
	if err != nil || o.Decision.Action != Resolve || o.IncidentID == 0 {
		t.Fatalf("%+v err=%v", o, err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM incidents WHERE resolved_at IS NOT NULL`); n != 1 {
		t.Error("incident not resolved")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM notifications WHERE kind='recovered'`); n != 1 {
		t.Error("want one recovered notification")
	}
	// Further successes must not queue anything else.
	Apply(ctx, db, id, pass, t0.Add(2*iv), iv)
	if n := count(t, db, `SELECT COUNT(*) FROM notifications`); n != 2 {
		t.Errorf("want 2 notifications total, got %d", n)
	}
	// A new outage creates a second incident with its own notifications.
	Apply(ctx, db, id, fail, t0.Add(3*iv), iv)
	if n := count(t, db, `SELECT COUNT(*) FROM incidents`); n != 2 {
		t.Errorf("want 2 incidents, got %d", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM notifications`); n != 3 {
		t.Errorf("want 3 notifications, got %d", n)
	}
}

func TestApplyBlipRecoversWithoutIncident(t *testing.T) {
	ctx := context.Background()
	db, id := setup(t, 2, 30)
	Apply(ctx, db, id, fail, t0, iv)
	Apply(ctx, db, id, fail, t0.Add(30*time.Second), iv)
	o, _ := Apply(ctx, db, id, pass, t0.Add(time.Minute), iv)
	if o.Decision.Action != None {
		t.Errorf("blip must not alert: %+v", o)
	}
	m, _ := db.GetMonitor(ctx, id)
	if m.State != "up" || m.FailStreak != 0 {
		t.Errorf("state=%s streak=%d", m.State, m.FailStreak)
	}
	if count(t, db, `SELECT COUNT(*) FROM incidents`) != 0 || count(t, db, `SELECT COUNT(*) FROM notifications`) != 0 {
		t.Error("blip created incident/notification")
	}
}

func TestApplySkipsDeletedOrDisabled(t *testing.T) {
	ctx := context.Background()
	db, id := setup(t, 0, 30)
	db.SetMonitorEnabled(ctx, id, false)
	o, err := Apply(ctx, db, id, fail, t0, iv)
	if err != nil || !o.Skipped {
		t.Fatalf("disabled: %+v err=%v", o, err)
	}
	db.DeleteMonitor(ctx, id)
	if o, err := Apply(ctx, db, id, fail, t0, iv); err != nil || !o.Skipped {
		t.Fatalf("deleted: %+v err=%v", o, err)
	}
	if count(t, db, `SELECT COUNT(*) FROM checks`) != 0 {
		t.Error("results for disabled/deleted monitor must be discarded")
	}
}
