package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func target(t *testing.T, u, expected string) Target {
	t.Helper()
	m, err := ParseExpectedStatus(expected)
	if err != nil {
		t.Fatal(err)
	}
	return Target{URL: u, Method: "GET", Timeout: 2 * time.Second, Expected: m}
}

func TestCheckStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte("hi"))
		case "/head":
			if r.Method != "HEAD" {
				w.WriteHeader(405)
			}
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := NewChecker(AllowAll)
	ctx := context.Background()

	if r := c.Check(ctx, target(t, srv.URL+"/ok", "200-299")); !r.OK || r.StatusCode != 200 {
		t.Errorf("ok: %+v", r)
	}
	if r := c.Check(ctx, target(t, srv.URL+"/bad", "200-299")); r.OK || r.StatusCode != 500 || !strings.Contains(r.Error, "500") {
		t.Errorf("500: %+v", r)
	}
	if r := c.Check(ctx, target(t, srv.URL+"/bad", "500")); !r.OK {
		t.Errorf("expected 500 should pass: %+v", r)
	}
	h := target(t, srv.URL+"/head", "200")
	h.Method = "HEAD"
	if r := c.Check(ctx, h); !r.OK {
		t.Errorf("head: %+v", r)
	}
}

func TestCheckTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	tg := target(t, srv.URL, "200")
	tg.Timeout = 200 * time.Millisecond
	start := time.Now()
	r := NewChecker(AllowAll).Check(context.Background(), tg)
	if r.OK || !strings.Contains(r.Error, "timeout") {
		t.Errorf("want timeout, got %+v", r)
	}
	if time.Since(start) > time.Second {
		t.Errorf("timeout not enforced: %v", time.Since(start))
	}
	if strings.Contains(r.Error, "127.0.0.1") {
		t.Errorf("error leaks URL: %q", r.Error)
	}
}

func TestCheckHugeBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1<<20)
		for i := 0; i < 200; i++ { // up to 200 MB if the client keeps reading
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	start := time.Now()
	r := NewChecker(AllowAll).Check(context.Background(), target(t, srv.URL, "200"))
	if !r.OK {
		t.Errorf("%+v", r)
	}
	if time.Since(start) > time.Second {
		t.Errorf("body not bounded, took %v", time.Since(start))
	}
}

func TestDefaultGuardBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	r := NewChecker(DefaultGuard).Check(context.Background(), target(t, srv.URL, "200"))
	if r.OK || !strings.Contains(r.Error, "blocked") {
		t.Errorf("loopback must be blocked: %+v", r)
	}
}

func TestRedirects(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("final")) }))
	defer final.Close()
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) }))
	defer forbidden.Close()
	fu, _ := url.Parse(forbidden.URL)
	forbiddenPort := fu.Port()

	var loopURL string
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-final":
			http.Redirect(w, r, final.URL, http.StatusFound)
		case "/to-forbidden":
			http.Redirect(w, r, forbidden.URL, http.StatusFound)
		case "/to-ftp":
			http.Redirect(w, r, "ftp://example.com/x", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, loopURL, http.StatusFound)
		}
	}))
	defer redir.Close()
	loopURL = redir.URL + "/loop"

	// Guard: allow every loopback server except the "forbidden" one, as if it were an internal host.
	guard := func(ap netip.AddrPort) error {
		if ap.Port() == uint16(mustAtoi(forbiddenPort)) {
			return ErrBlockedAddress
		}
		return nil
	}
	c := NewChecker(guard)
	ctx := context.Background()

	if r := c.Check(ctx, target(t, redir.URL+"/to-final", "200")); !r.OK {
		t.Errorf("redirect to allowed host: %+v", r)
	}
	if r := c.Check(ctx, target(t, redir.URL+"/to-forbidden", "200")); r.OK || !strings.Contains(r.Error, "blocked") {
		t.Errorf("redirect to blocked address must fail: %+v", r)
	}
	if r := c.Check(ctx, target(t, redir.URL+"/to-ftp", "200")); r.OK {
		t.Errorf("redirect to ftp must fail: %+v", r)
	}
	if r := c.Check(ctx, target(t, redir.URL+"/loop", "200")); r.OK || !strings.Contains(r.Error, "redirects") {
		t.Errorf("redirect loop must stop: %+v", r)
	}
}

func mustAtoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
