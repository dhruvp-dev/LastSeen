package notifications

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dhruvp/lastseen/internal/config"
	"github.com/dhruvp/lastseen/internal/database"
	"github.com/dhruvp/lastseen/internal/incidents"
	"github.com/dhruvp/lastseen/internal/monitor"
)

type fakeSender struct {
	mu   sync.Mutex
	sent []string
	err  error
}

func (f *fakeSender) Send(_ context.Context, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, subject)
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func setup(t *testing.T) (*database.DB, int64) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m := &database.Monitor{Name: "My Blog", Slug: "blog", URL: "https://blog.example", Method: "GET", ExpectedStat: "200",
		TimeoutMS: 5000, RetryDelayS: 30, MaxRetries: 0, Enabled: true}
	if err := db.CreateMonitor(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return db, m.ID
}

var t0 = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

func open(t *testing.T, db *database.DB, id int64) {
	t.Helper()
	if _, err := incidents.Apply(context.Background(), db, id, monitor.Result{Error: "unexpected status 502"}, t0, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
}

func status(t *testing.T, db *database.DB) (st string, attempts int) {
	t.Helper()
	if err := db.QueryRow(`SELECT status, attempts FROM notifications ORDER BY id LIMIT 1`).Scan(&st, &attempts); err != nil {
		t.Fatal(err)
	}
	return
}

func TestSendsOnceNoDuplicates(t *testing.T) {
	db, id := setup(t)
	open(t, db, id)
	fs := &fakeSender{}
	p := NewProcessor(db, fs, quiet(), "https://status.example")
	for i := 0; i < 3; i++ { // repeated passes must not resend
		if err := p.Process(context.Background(), t0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if len(fs.sent) != 1 || fs.sent[0] != "[Last Seen] My Blog is down" {
		t.Errorf("sent=%v", fs.sent)
	}
	if st, a := status(t, db); st != "sent" || a != 1 {
		t.Errorf("status=%s attempts=%d", st, a)
	}
	// Recovery sends exactly one more.
	incidents.Apply(context.Background(), db, id, monitor.Result{OK: true, StatusCode: 200, ResponseMS: 5}, t0.Add(30*time.Minute), 20*time.Minute)
	p.Process(context.Background(), t0.Add(31*time.Minute))
	p.Process(context.Background(), t0.Add(32*time.Minute))
	if len(fs.sent) != 2 || fs.sent[1] != "[Last Seen] My Blog is back up" {
		t.Errorf("sent=%v", fs.sent)
	}
}

func TestFailureBackoffAndPermanentFailure(t *testing.T) {
	db, id := setup(t)
	open(t, db, id)
	fs := &fakeSender{err: errors.New("connection refused")}
	p := NewProcessor(db, fs, quiet(), "")
	now := t0
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		p.Process(context.Background(), now)
		st, a := status(t, db)
		if a != attempt {
			t.Fatalf("attempt %d: attempts=%d", attempt, a)
		}
		if attempt < MaxAttempts {
			if st != "pending" {
				t.Fatalf("attempt %d: status=%s want pending", attempt, st)
			}
			// Not retried before the backoff has elapsed.
			p.Process(context.Background(), now.Add(Backoff(attempt)-time.Second))
			if _, a2 := status(t, db); a2 != attempt {
				t.Fatalf("retried too early after attempt %d", attempt)
			}
			now = now.Add(Backoff(attempt))
		} else if st != "failed" {
			t.Fatalf("final status=%s want failed", st)
		}
	}
	// A failed notification is never retried again.
	fs.err = nil
	p.Process(context.Background(), now.Add(48*time.Hour))
	if len(fs.sent) != 0 {
		t.Error("permanently failed notification was sent")
	}
	var e sql.NullString
	db.QueryRow(`SELECT last_error FROM notifications`).Scan(&e)
	if e.String != "connection refused" {
		t.Errorf("last_error=%q", e.String)
	}
}

func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	for i, w := range want {
		if got := Backoff(i + 1); got != w {
			t.Errorf("Backoff(%d)=%v want %v", i+1, got, w)
		}
	}
	if Backoff(30) != time.Hour {
		t.Errorf("backoff must cap at 1h, got %v", Backoff(30))
	}
}

func TestDisabledSettingSkipsSending(t *testing.T) {
	db, id := setup(t)
	open(t, db, id)
	db.SetSetting(context.Background(), "notifications_enabled", "0")
	fs := &fakeSender{}
	NewProcessor(db, fs, quiet(), "").Process(context.Background(), t0)
	if len(fs.sent) != 0 {
		t.Error("sent while disabled")
	}
	if st, _ := status(t, db); st != "failed" {
		t.Errorf("status=%s", st)
	}
}

func TestComposeContent(t *testing.T) {
	sub, body := Compose("down", "API", "https://api.example", "timeout after 10s", t0.UnixMilli(), sql.NullInt64{}, "https://status.example")
	for _, want := range []string{"API appears to be down", "https://api.example", "timeout after 10s", "https://status.example"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if sub != "[Last Seen] API is down" {
		t.Error(sub)
	}
	_, rec := Compose("recovered", "API", "https://api.example", "", t0.UnixMilli(), sql.NullInt64{Int64: t0.Add(95 * time.Minute).UnixMilli(), Valid: true}, "")
	if !strings.Contains(rec, "1h 35m") {
		t.Errorf("duration missing:\n%s", rec)
	}
}

func TestHeaderInjectionStripped(t *testing.T) {
	msg := string(buildMessage("a@x.com", "b@x.com", "Evil\r\nBcc: attacker@x.com", "body"))
	head := strings.SplitN(msg, "\r\n\r\n", 2)[0]
	for _, line := range strings.Split(head, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Errorf("injected header present: %q", head)
		}
	}
}

// fakeSMTP is a minimal plaintext SMTP server capturing one message.
func fakeSMTP(t *testing.T) (port int, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		w := func(s string) { c.Write([]byte(s + "\r\n")) }
		w("220 fake ESMTP")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					got <- data.String()
					w("250 queued")
					continue
				}
				data.WriteString(line + "\n")
				continue
			}
			switch cmd := strings.ToUpper(line); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				w("250 fake")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				w("250 ok")
			case cmd == "DATA":
				inData = true
				w("354 go")
			case cmd == "QUIT":
				w("221 bye")
				return
			default:
				w("250 ok")
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

func TestSMTPSenderDelivers(t *testing.T) {
	port, got := fakeSMTP(t)
	s := NewSMTPSender(config.SMTP{Host: "127.0.0.1", Port: port, From: "lastseen@example.com", To: "me@example.com"})
	if err := s.Send(context.Background(), "[Last Seen] Blog is down", "line one\nline two"); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-got:
		for _, want := range []string{"From: lastseen@example.com", "To: me@example.com", "Subject: [Last Seen] Blog is down", "line two"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message missing %q:\n%s", want, msg)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no message received")
	}
}

func TestSMTPRefusesCredentialsWithoutTLS(t *testing.T) {
	port, _ := fakeSMTP(t)
	s := NewSMTPSender(config.SMTP{Host: "127.0.0.1", Port: port, From: "a@x.com", To: "b@x.com", User: "u", Pass: "supersecret"})
	err := s.Send(context.Background(), "s", "b")
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("want STARTTLS refusal, got %v", err)
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Error("error leaks password")
	}
}

func TestSMTPNotConfigured(t *testing.T) {
	if err := NewSMTPSender(config.SMTP{}).Send(context.Background(), "s", "b"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("got %v", err)
	}
}
