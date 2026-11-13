package database

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateIdempotentAndWAL(t *testing.T) {
	db := openTemp(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var mode string
	db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode=%s", mode)
	}
	var fk int
	db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Error("foreign keys off")
	}
}

func TestMonitorCRUD(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	m := &Monitor{Name: "Blog", Slug: "blog", URL: "https://example.com", Method: "GET", ExpectedStat: "200-299", TimeoutMS: 10000, RetryDelayS: 30, MaxRetries: 2, Enabled: true, IsPublic: true}
	if err := db.CreateMonitor(ctx, m); err != nil || m.ID == 0 {
		t.Fatalf("create: %v", err)
	}
	dup := *m
	dup.ID = 0
	if err := db.CreateMonitor(ctx, &dup); !errors.Is(err, ErrSlugTaken) {
		t.Errorf("want ErrSlugTaken, got %v", err)
	}
	m.Name = "Blog 2"
	if err := db.UpdateMonitor(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetMonitor(ctx, m.ID)
	if got.Name != "Blog 2" || got.State != "unknown" {
		t.Errorf("unexpected %+v", got)
	}
	if err := db.SetMonitorEnabled(ctx, m.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteMonitor(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetMonitor(ctx, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestOneOpenIncidentPerMonitor(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	m := &Monitor{Name: "A", Slug: "a", URL: "https://example.com", Method: "GET", ExpectedStat: "200", TimeoutMS: 5000, RetryDelayS: 30, MaxRetries: 2}
	db.CreateMonitor(ctx, m)
	ins := `INSERT INTO incidents(monitor_id, started_at) VALUES (?, 1)`
	if _, err := db.Exec(ins, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ins, m.ID); err == nil {
		t.Error("second open incident should violate unique index")
	}
}

func TestRetrySettingsConstraints(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	base := Monitor{Name: "A", URL: "https://example.com", Method: "GET", ExpectedStat: "200", TimeoutMS: 5000, RetryDelayS: 30, MaxRetries: 2}
	cases := []struct {
		slug         string
		delay, retry int
		ok           bool
	}{
		{"min", 10, 0, true}, {"max", 1800, 5, true}, {"mid", 900, 3, true},
		{"delay-low", 9, 2, false}, {"delay-high", 1801, 2, false}, {"retry-high", 30, 6, false}, {"retry-neg", 30, -1, false},
	}
	for _, c := range cases {
		m := base
		m.Slug, m.RetryDelayS, m.MaxRetries = c.slug, c.delay, c.retry
		err := db.CreateMonitor(ctx, &m)
		if (err == nil) != c.ok {
			t.Errorf("%s: delay=%d retries=%d err=%v want ok=%v", c.slug, c.delay, c.retry, err, c.ok)
		}
	}
	m := base
	m.Slug, m.RetryDelayS, m.MaxRetries = "rt", 1800, 4
	db.CreateMonitor(ctx, &m)
	got, _ := db.GetMonitor(ctx, m.ID)
	if got.RetryDelayS != 1800 || got.MaxRetries != 4 || got.FailStreak != 0 {
		t.Errorf("round trip: %+v", got)
	}
}

func TestCheckWindowAndServiceRows(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	m := &Monitor{Name: "A", Slug: "a", URL: "https://example.com", Method: "GET", ExpectedStat: "200", TimeoutMS: 5000, RetryDelayS: 30, MaxRetries: 2}
	db.CreateMonitor(ctx, m)
	now := Now()
	ins := `INSERT INTO checks(monitor_id, checked_at, ok, status_code, response_ms) VALUES (?,?,?,?,?)`
	db.Exec(ins, m.ID, now-1000, 1, 200, 100)
	db.Exec(ins, m.ID, now-2000, 1, 200, 300)
	db.Exec(ins, m.ID, now-3000, 0, 500, 50)
	w, err := db.CheckWindow(ctx, now-10000, now+1)
	if err != nil || w.Total != 3 || w.Up != 2 {
		t.Fatalf("window: %+v err=%v", w, err)
	}
	if p, ok := w.UptimePct(); !ok || p < 66.6 || p > 66.7 {
		t.Errorf("uptime=%v ok=%v", p, ok)
	}
	if !w.AvgMS.Valid || w.AvgMS.Float64 != 200 {
		t.Errorf("avg=%+v (failed checks must be excluded)", w.AvgMS)
	}
	empty, _ := db.CheckWindow(ctx, 0, 1)
	if _, ok := empty.UptimePct(); ok {
		t.Error("empty window must report no data, not 100%")
	}
	rows, err := db.ServiceRows(ctx)
	if err != nil || len(rows) != 1 || !rows[0].LastResponseMS.Valid || rows[0].LastResponseMS.Int64 != 300 {
		t.Errorf("rows: %+v err=%v", rows, err)
	}
}

func TestBackupSnapshotAndRetention(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	m := &Monitor{Name: "Keep Me", Slug: "keep-me", URL: "https://example.com", Method: "GET", ExpectedStat: "200", TimeoutMS: 5000, RetryDelayS: 30, MaxRetries: 2}
	if err := db.CreateMonitor(ctx, m); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "backups")
	base := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("unrelated"), 0o600) // must never be pruned

	var last string
	for i := 0; i < 5; i++ {
		p, err := db.Backup(ctx, dir, 3, base.Add(time.Duration(i)*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		last = p
	}
	entries, _ := os.ReadDir(dir)
	var dbs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".db") {
			dbs = append(dbs, e.Name())
		}
		if strings.HasSuffix(e.Name(), ".partial") {
			t.Errorf("leftover partial file %s", e.Name())
		}
	}
	if len(dbs) != 3 || dbs[0] != "lastseen-20260103-030000.db" || dbs[2] != "lastseen-20260105-030000.db" {
		t.Errorf("retention kept %v", dbs)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Error("unrelated file was pruned")
	}
	if fi, _ := os.Stat(last); fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %v, want 0600", fi.Mode().Perm())
	}
	// The snapshot is a complete, openable database containing the data.
	snap, err := Open(last)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	var name string
	if err := snap.QueryRow(`SELECT name FROM monitors`).Scan(&name); err != nil || name != "Keep Me" {
		t.Errorf("snapshot content: %q %v", name, err)
	}
	// Refuses to overwrite and rejects nonsense retention.
	if _, err := db.Backup(ctx, dir, 3, base.Add(4*24*time.Hour)); err == nil {
		t.Error("overwrote an existing backup")
	}
	if _, err := db.Backup(ctx, dir, 0, base); err == nil {
		t.Error("keep=0 accepted")
	}
}
