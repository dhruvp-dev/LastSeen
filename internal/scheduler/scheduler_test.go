package scheduler

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dhruvp/lastseen/internal/database"
	"github.com/dhruvp/lastseen/internal/monitor"
)

type fakeChecker struct {
	mu      sync.Mutex
	results []monitor.Result // consumed in order; last one repeats
	calls   int
	active  int32
	maxSeen int32
	delay   time.Duration
}

func (f *fakeChecker) Check(ctx context.Context, t monitor.Target) monitor.Result {
	n := atomic.AddInt32(&f.active, 1)
	for {
		m := atomic.LoadInt32(&f.maxSeen)
		if n <= m || atomic.CompareAndSwapInt32(&f.maxSeen, m, n) {
			break
		}
	}
	time.Sleep(f.delay)
	atomic.AddInt32(&f.active, -1)
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	f.calls++
	return f.results[i]
}

var (
	fail = monitor.Result{StatusCode: 503, ResponseMS: 20, Error: "unexpected status 503"}
	pass = monitor.Result{OK: true, StatusCode: 200, ResponseMS: 50}
	t0   = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
)

type harness struct {
	db  *database.DB
	s   *Scheduler
	now time.Time
	fc  *fakeChecker
}

func newHarness(t *testing.T, fc *fakeChecker) *harness {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &harness{db: db, fc: fc, now: t0}
	h.s = New(db, fc, slog.New(slog.NewTextHandler(io.Discard, nil)), 20*time.Minute)
	h.s.Now = func() time.Time { return h.now }
	return h
}

func (h *harness) addMonitor(t *testing.T, slug string, retries, delay int) int64 {
	t.Helper()
	m := &database.Monitor{Name: slug, Slug: slug, URL: "https://example.com", Method: "GET", ExpectedStat: "200-299",
		TimeoutMS: 5000, RetryDelayS: delay, MaxRetries: retries, Enabled: true}
	if err := h.db.CreateMonitor(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	// Due immediately relative to the fake clock (CreateMonitor stamps real time).
	if _, err := h.db.Exec(`UPDATE monitors SET next_check_at=0 WHERE id=?`, m.ID); err != nil {
		t.Fatal(err)
	}
	return m.ID
}

func (h *harness) tick(t *testing.T) {
	t.Helper()
	if err := h.s.Tick(context.Background(), h.now); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) mon(t *testing.T, id int64) *database.Monitor {
	t.Helper()
	m, err := h.db.GetMonitor(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (h *harness) n(t *testing.T, q string) int {
	t.Helper()
	var n int
	if err := h.db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSuccessSchedulesNormalInterval(t *testing.T) {
	h := newHarness(t, &fakeChecker{results: []monitor.Result{pass}})
	id := h.addMonitor(t, "a", 2, 30)
	h.tick(t)
	m := h.mon(t, id)
	if m.State != "up" || m.NextCheckAt != h.now.Add(20*time.Minute).UnixMilli() {
		t.Errorf("state=%s next=%d", m.State, m.NextCheckAt)
	}
	if h.n(t, `SELECT COUNT(*) FROM checks`) != 1 {
		t.Error("want one check row")
	}
}

func TestOnlyDueEnabledMonitorsAreChecked(t *testing.T) {
	h := newHarness(t, &fakeChecker{results: []monitor.Result{pass}})
	h.addMonitor(t, "due", 2, 30)
	idLater := h.addMonitor(t, "later", 2, 30)
	idOff := h.addMonitor(t, "off", 2, 30)
	h.db.Exec(`UPDATE monitors SET next_check_at=? WHERE id=?`, h.now.Add(time.Hour).UnixMilli(), idLater)
	h.db.SetMonitorEnabled(context.Background(), idOff, false)
	h.tick(t)
	if h.fc.calls != 1 {
		t.Errorf("want 1 check, got %d", h.fc.calls)
	}
	// Not due again until its next_check_at.
	h.tick(t)
	if h.fc.calls != 1 {
		t.Errorf("monitor re-checked before due: %d calls", h.fc.calls)
	}
}

func runChain(t *testing.T, delay int) {
	h := newHarness(t, &fakeChecker{results: []monitor.Result{fail}})
	id := h.addMonitor(t, "a", 2, delay)
	d := time.Duration(delay) * time.Second

	for i := 1; i <= 2; i++ { // attempt + first retry: still no incident
		h.tick(t)
		m := h.mon(t, id)
		if m.FailStreak != i || m.State == "down" || m.NextCheckAt != h.now.Add(d).UnixMilli() {
			t.Fatalf("delay=%d attempt %d: streak=%d state=%s next=%d want %d", delay, i, m.FailStreak, m.State, m.NextCheckAt, h.now.Add(d).UnixMilli())
		}
		// A tick before the retry is due must not check again.
		before := h.fc.calls
		h.now = h.now.Add(d - time.Second)
		h.tick(t)
		if h.fc.calls != before {
			t.Fatalf("retry fired %s early", time.Second)
		}
		h.now = h.now.Add(time.Second)
	}
	h.tick(t) // third failure confirms
	m := h.mon(t, id)
	if m.State != "down" || m.FailStreak != 0 || m.NextCheckAt != h.now.Add(20*time.Minute).UnixMilli() {
		t.Errorf("confirmed: state=%s streak=%d", m.State, m.FailStreak)
	}
	if h.n(t, `SELECT COUNT(*) FROM incidents WHERE resolved_at IS NULL`) != 1 ||
		h.n(t, `SELECT COUNT(*) FROM notifications WHERE kind='down'`) != 1 {
		t.Error("want one open incident and one down notification")
	}
}

func TestRetryChainShortDelay(t *testing.T)    { runChain(t, 30) }
func TestRetryChainThirtyMinutes(t *testing.T) { runChain(t, 1800) }

func TestRecoveryAndRecoveredNotification(t *testing.T) {
	h := newHarness(t, &fakeChecker{results: []monitor.Result{fail, pass}})
	id := h.addMonitor(t, "a", 0, 30)
	h.tick(t)
	if h.mon(t, id).State != "down" {
		t.Fatal("should be down")
	}
	h.now = h.now.Add(20 * time.Minute)
	h.tick(t)
	if h.mon(t, id).State != "up" || h.n(t, `SELECT COUNT(*) FROM incidents WHERE resolved_at IS NOT NULL`) != 1 ||
		h.n(t, `SELECT COUNT(*) FROM notifications WHERE kind='recovered'`) != 1 {
		t.Error("recovery not recorded")
	}
}

func TestRetryChainResumesAfterRestart(t *testing.T) {
	h := newHarness(t, &fakeChecker{results: []monitor.Result{fail}})
	id := h.addMonitor(t, "a", 2, 1800)
	h.tick(t) // failure #1 -> streak 1, retry in 30 min
	// "Restart": brand-new scheduler on the same database.
	h2 := &harness{db: h.db, fc: h.fc, now: h.now.Add(30 * time.Minute)}
	h2.s = New(h.db, h.fc, slog.New(slog.NewTextHandler(io.Discard, nil)), 20*time.Minute)
	h2.s.Now = func() time.Time { return h2.now }
	h2.tick(t)
	if m := h.mon(t, id); m.FailStreak != 2 {
		t.Errorf("chain should resume at streak 2, got %d", m.FailStreak)
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	fc := &fakeChecker{results: []monitor.Result{pass}, delay: 40 * time.Millisecond}
	h := newHarness(t, fc)
	for i := 0; i < 10; i++ {
		h.addMonitor(t, string(rune('a'+i)), 2, 30)
	}
	h.s.Concurrency = 3
	h.tick(t)
	if fc.calls != 10 {
		t.Errorf("want 10 checks, got %d", fc.calls)
	}
	if fc.maxSeen > 3 {
		t.Errorf("concurrency exceeded limit: %d", fc.maxSeen)
	}
}

func TestStaggerOverdueSpreadsChecks(t *testing.T) {
	h := newHarness(t, &fakeChecker{results: []monitor.Result{pass}})
	for i := 0; i < 5; i++ {
		h.addMonitor(t, string(rune('a'+i)), 2, 30)
	}
	nowMS := h.now.UnixMilli()
	if err := h.db.StaggerOverdue(context.Background(), nowMS, 60_000); err != nil {
		t.Fatal(err)
	}
	ms, _ := h.db.ListMonitors(context.Background())
	for _, m := range ms {
		if m.NextCheckAt < nowMS || m.NextCheckAt >= nowMS+60_000 {
			t.Errorf("next_check_at %d outside [now, now+60s)", m.NextCheckAt-nowMS)
		}
	}
}

func TestHeartbeatAndPurge(t *testing.T) {
	h := newHarness(t, &fakeChecker{results: []monitor.Result{pass}})
	ctx := context.Background()
	h.db.Heartbeat(ctx, 12345)
	if v, _ := h.db.GetMeta(ctx, "worker_heartbeat"); v != "12345" {
		t.Errorf("heartbeat=%q", v)
	}
	id := h.addMonitor(t, "a", 2, 30)
	old := h.now.Add(-130 * 24 * time.Hour).UnixMilli()
	h.db.Exec(`INSERT INTO checks(monitor_id, checked_at, ok) VALUES (?,?,1)`, id, old)
	h.db.Exec(`INSERT INTO checks(monitor_id, checked_at, ok) VALUES (?,?,1)`, id, h.now.UnixMilli())
	n, err := h.db.Purge(ctx, h.now.Add(-120*24*time.Hour).UnixMilli(), h.now.UnixMilli())
	if err != nil || n != 1 {
		t.Errorf("purged %d err=%v", n, err)
	}
	if h.n(t, `SELECT COUNT(*) FROM checks`) != 1 {
		t.Error("recent check must survive purge")
	}
}
