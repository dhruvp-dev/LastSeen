package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/dhruvp/lastseen/internal/config"
	"github.com/dhruvp/lastseen/internal/database"
	"github.com/dhruvp/lastseen/internal/incidents"
	"github.com/dhruvp/lastseen/internal/monitor"
)

const testPassword = "correct horse battery"

type fakeSender struct {
	err  error
	sent []string
}

func (f *fakeSender) Send(_ context.Context, subject, body string) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, subject)
	return nil
}

type env struct {
	db     *database.DB
	srv    *Server
	h      http.Handler
	sender *fakeSender
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	hash, _ := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	cfg := &config.Config{BaseURL: "https://status.example", AdminPasswordHash: string(hash),
		CheckInterval: 20 * time.Minute, SMTP: config.SMTP{Host: "smtp.example", Port: 587, User: "alice@example.com", Pass: "TOPSECRETPASS", From: "ls@example.com", To: "me@example.com"}}
	fs := &fakeSender{}
	srv, err := New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)), fs)
	if err != nil {
		t.Fatal(err)
	}
	return &env{db: db, srv: srv, h: srv.Handler(), sender: fs}
}

func (e *env) add(t *testing.T, name string, public bool) int64 {
	t.Helper()
	m := &database.Monitor{Name: name, Slug: strings.ToLower(strings.ReplaceAll(name, " ", "-")), URL: "https://" + strings.ToLower(strings.ReplaceAll(name, " ", "")) + ".example",
		Method: "GET", ExpectedStat: "200", TimeoutMS: 5000, RetryDelayS: 30, MaxRetries: 0, Enabled: true, IsPublic: public}
	if err := e.db.CreateMonitor(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m.ID
}

func (e *env) apply(t *testing.T, id int64, ok bool, at time.Time) {
	t.Helper()
	res := monitor.Result{OK: ok, StatusCode: 200, ResponseMS: 42}
	if !ok {
		res = monitor.Result{StatusCode: 500, ResponseMS: 7, Error: "unexpected status 500 from internal-db-host-7"}
	}
	if _, err := incidents.Apply(context.Background(), e.db, id, res, at, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
}

func (e *env) heartbeat(t *testing.T, at time.Time) {
	t.Helper()
	e.db.Heartbeat(context.Background(), at.UnixMilli())
}

func (e *env) get(path string) (int, string) {
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Code, rec.Body.String()
}

func TestPublicAllOperational(t *testing.T) {
	e := newEnv(t)
	id := e.add(t, "My Blog", true)
	e.apply(t, id, true, time.Now())
	e.heartbeat(t, time.Now())
	code, body := e.get("/")
	if code != 200 || !strings.Contains(body, "All systems operational") || !strings.Contains(body, "My Blog") || !strings.Contains(body, "42 ms") {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if n := strings.Count(body, "<rect "); n != 90 {
		t.Errorf("want 90 day bars, got %d", n)
	}
	if !strings.Contains(body, "Last checked") || strings.Contains(body, "No checks recorded") {
		t.Error("last checked missing")
	}
}

func TestPublicHidesPrivateMonitorsAndLeaksNoErrorDetail(t *testing.T) {
	e := newEnv(t)
	pub := e.add(t, "Public Site", true)
	priv := e.add(t, "Secret Admin Tool", false)
	e.apply(t, pub, false, time.Now()) // retries=0 -> incident opens
	e.apply(t, priv, false, time.Now())
	e.heartbeat(t, time.Now())
	_, body := e.get("/")
	if strings.Contains(body, "Secret Admin Tool") {
		t.Error("private monitor visible on public page")
	}
	if strings.Contains(body, "internal-db-host-7") || strings.Contains(body, "unexpected status") {
		t.Error("raw internal error text leaked to public page")
	}
	if !strings.Contains(body, "1 service is down") && !strings.Contains(body, "Major outage") {
		t.Errorf("outage not reflected:\n%s", body)
	}
	if !strings.Contains(body, "Active incidents") || !strings.Contains(body, "Public Site") {
		t.Error("active incident missing")
	}
}

func TestPublicResolvedIncidentAndNote(t *testing.T) {
	e := newEnv(t)
	id := e.add(t, "API", true)
	e.apply(t, id, false, time.Now().Add(-2*time.Hour))
	e.apply(t, id, true, time.Now().Add(-time.Hour))
	e.db.Exec(`UPDATE incidents SET note='Database failover took longer than expected.'`)
	e.heartbeat(t, time.Now())
	_, body := e.get("/")
	if strings.Contains(body, "Active incidents") {
		t.Error("resolved incident shown as active")
	}
	for _, want := range []string{"Past incidents", "Resolved · 1h", "Database failover took longer", "All systems operational"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestPublicStaleWorkerNeverShowsGreen(t *testing.T) {
	e := newEnv(t)
	id := e.add(t, "Blog", true)
	e.apply(t, id, true, time.Now())
	for name, setup := range map[string]func(){
		"no heartbeat":    func() {},
		"stale heartbeat": func() { e.heartbeat(t, time.Now().Add(-10*time.Minute)) },
	} {
		setup()
		_, body := e.get("/")
		if !strings.Contains(body, "Monitoring paused") || strings.Contains(body, "All systems operational") {
			t.Errorf("%s: must show paused banner", name)
		}
	}
}

func TestPublicEscapesMonitorName(t *testing.T) {
	e := newEnv(t)
	id := e.add(t, `<script>alert(1)</script>`, true)
	e.apply(t, id, true, time.Now())
	e.heartbeat(t, time.Now())
	_, body := e.get("/")
	if strings.Contains(body, "<script>alert(1)") {
		t.Error("monitor name not escaped")
	}
}

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	if code, _ := e.get("/healthz"); code != 503 {
		t.Errorf("no heartbeat: want 503, got %d", code)
	}
	e.heartbeat(t, time.Now())
	if code, body := e.get("/healthz"); code != 200 || strings.TrimSpace(body) != "ok" {
		t.Errorf("fresh heartbeat: %d %q", code, body)
	}
	e.heartbeat(t, time.Now().Add(-3*time.Minute))
	if code, _ := e.get("/healthz"); code != 503 {
		t.Errorf("stale heartbeat: want 503, got %d", code)
	}
}

func TestFormatPctNeverRoundsOutageToPerfect(t *testing.T) {
	cases := map[float64]string{100: "100%", 99.999: "99.99%", 99.5: "99.50%", 0: "0.00%"}
	for in, want := range cases {
		if got := formatPct(in); got != want {
			t.Errorf("formatPct(%v)=%s want %s", in, got, want)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("security headers missing")
	}
}

// ---------- admin / auth ----------

type client struct {
	t      *testing.T
	e      *env
	cookie *http.Cookie
	csrf   string
}

var csrfRe = regexp.MustCompile(`X-CSRF-Token":"([^"]+)"`)

func (e *env) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) login(t *testing.T) *client {
	t.Helper()
	rec := e.do(formReq("POST", "/login", url.Values{"password": {testPassword}}, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status %d: %s", rec.Code, rec.Body)
	}
	c := &client{t: t, e: e}
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == cookieName {
			c.cookie = ck
		}
	}
	if c.cookie == nil {
		t.Fatal("no session cookie")
	}
	rec = c.get("/admin/settings")
	m := csrfRe.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("csrf token not found in page (status %d)", rec.Code)
	}
	c.csrf = m[1]
	return c
}

func formReq(method, path string, v url.Values, ck *http.Cookie) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if ck != nil {
		req.AddCookie(ck)
	}
	return req
}

func (c *client) get(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(c.cookie)
	return c.e.do(req)
}

// post submits a form with a valid CSRF token unless v already sets one.
func (c *client) post(path string, v url.Values) *httptest.ResponseRecorder {
	if v == nil {
		v = url.Values{}
	}
	if v.Get("csrf_token") == "" {
		v.Set("csrf_token", c.csrf)
	}
	return c.e.do(formReq("POST", path, v, c.cookie))
}

func TestAdminRequiresLogin(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/admin", "/admin/monitors", "/admin/monitors/new", "/admin/incidents", "/admin/notifications", "/admin/settings", "/admin/monitors/1"} {
		rec := e.do(httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("%s: %d -> %q", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	for _, r := range []*http.Request{
		formReq("POST", "/admin/monitors", url.Values{"name": {"x"}}, nil),
		httptest.NewRequest("DELETE", "/admin/monitors/1", nil),
		formReq("POST", "/admin/settings", url.Values{}, nil),
	} {
		if rec := e.do(r); rec.Code != http.StatusSeeOther {
			t.Errorf("%s %s without session: %d", r.Method, r.URL.Path, rec.Code)
		}
	}
	// HTMX gets a 401 + HX-Redirect rather than a redirect it would swap into the page.
	req := httptest.NewRequest("POST", "/admin/monitors/1/toggle", nil)
	req.Header.Set("HX-Request", "true")
	if rec := e.do(req); rec.Code != 401 || rec.Header().Get("HX-Redirect") != "/login" {
		t.Errorf("htmx unauth: %d %v", rec.Code, rec.Header())
	}
	// Forged / garbage cookies are rejected.
	req = httptest.NewRequest("GET", "/admin", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "forged"})
	if rec := e.do(req); rec.Code != http.StatusSeeOther {
		t.Errorf("forged cookie accepted: %d", rec.Code)
	}
}

func TestLoginCookieFlagsAndFailure(t *testing.T) {
	e := newEnv(t)
	rec := e.do(formReq("POST", "/login", url.Values{"password": {"nope-nope-nope"}}, nil))
	if rec.Code != 401 || len(rec.Result().Cookies()) != 0 {
		t.Errorf("bad password: %d cookies=%v", rec.Code, rec.Result().Cookies())
	}
	rec = e.do(formReq("POST", "/login", url.Values{"password": {testPassword}}, nil))
	var ck *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			ck = c
		}
	}
	if ck == nil || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" || ck.Expires.IsZero() {
		t.Errorf("cookie flags wrong: %+v", ck)
	}
}

func TestLoginFailsClosedWithoutConfiguredHash(t *testing.T) {
	e := newEnv(t)
	e.srv.cfg.AdminPasswordHash = ""
	for _, pw := range []string{"", testPassword} {
		if rec := e.do(formReq("POST", "/login", url.Values{"password": {pw}}, nil)); rec.Code != 401 {
			t.Errorf("empty hash accepted %q: %d", pw, rec.Code)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		if rec := e.do(formReq("POST", "/login", url.Values{"password": {"wrong-wrong-wrong"}}, nil)); rec.Code != 401 {
			t.Fatalf("attempt %d: %d", i+1, rec.Code)
		}
	}
	rec := e.do(formReq("POST", "/login", url.Values{"password": {testPassword}}, nil))
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Errorf("want 429 even for the right password, got %d", rec.Code)
	}
	// A different client address is unaffected.
	req := formReq("POST", "/login", url.Values{"password": {testPassword}}, nil)
	req.RemoteAddr = "198.51.100.7:4000"
	if rec := e.do(req); rec.Code != http.StatusSeeOther {
		t.Errorf("other IP blocked: %d", rec.Code)
	}
}

func TestCSRFEnforced(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	form := url.Values{"name": {"Site"}, "url": {"https://8.8.8.8"}, "expected_status": {"200"}, "timeout_s": {"10"},
		"max_retries": {"2"}, "retry_delay_s": {"30"}, "enabled": {"on"}}
	// Missing and wrong tokens.
	req := formReq("POST", "/admin/monitors", form, c.cookie)
	if rec := e.do(req); rec.Code != 403 {
		t.Errorf("no token: %d", rec.Code)
	}
	bad := url.Values{}
	for k, v := range form {
		bad[k] = v
	}
	bad.Set("csrf_token", "wrong")
	if rec := e.do(formReq("POST", "/admin/monitors", bad, c.cookie)); rec.Code != 403 {
		t.Errorf("wrong token: %d", rec.Code)
	}
	if n := countMonitors(t, e); n != 0 {
		t.Fatalf("monitor created without valid CSRF (%d)", n)
	}
	// Valid token works.
	if rec := c.post("/admin/monitors", form); rec.Code != http.StatusSeeOther {
		t.Errorf("valid token: %d %s", rec.Code, rec.Body)
	}
	if countMonitors(t, e) != 1 {
		t.Fatal("monitor not created")
	}
	// DELETE needs the header token.
	req = httptest.NewRequest("DELETE", "/admin/monitors/1", nil)
	req.AddCookie(c.cookie)
	if rec := e.do(req); rec.Code != 403 {
		t.Errorf("delete without header: %d", rec.Code)
	}
	req = httptest.NewRequest("DELETE", "/admin/monitors/1", nil)
	req.AddCookie(c.cookie)
	req.Header.Set("X-CSRF-Token", c.csrf)
	if rec := e.do(req); rec.Code != 200 || countMonitors(t, e) != 0 {
		t.Errorf("delete with header: %d", rec.Code)
	}
}

func countMonitors(t *testing.T, e *env) int {
	t.Helper()
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM monitors`).Scan(&n)
	return n
}

func TestCrossOriginPostsBlocked(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	req := formReq("POST", "/admin/settings", url.Values{"csrf_token": {c.csrf}, "site_title": {"Hacked"}}, c.cookie)
	req.Header.Set("Origin", "https://evil.example")
	if rec := e.do(req); rec.Code != 403 {
		t.Errorf("cross-origin settings POST: %d", rec.Code)
	}
	req = formReq("POST", "/login", url.Values{"password": {testPassword}}, nil)
	req.Header.Set("Origin", "https://evil.example")
	if rec := e.do(req); rec.Code != 403 {
		t.Errorf("cross-origin login: %d", rec.Code)
	}
	// Same-origin Origin header is fine.
	req = formReq("POST", "/login", url.Values{"password": {testPassword}}, nil)
	req.Header.Set("Origin", "http://"+req.Host)
	if rec := e.do(req); rec.Code != http.StatusSeeOther {
		t.Errorf("same-origin login: %d", rec.Code)
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	if rec := c.get("/admin"); rec.Code != 200 {
		t.Fatalf("pre-logout: %d", rec.Code)
	}
	if rec := c.e.do(formReq("POST", "/logout", url.Values{}, c.cookie)); rec.Code != 403 {
		t.Errorf("logout without CSRF: %d", rec.Code)
	}
	if rec := c.post("/logout", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("logout: %d", rec.Code)
	}
	if rec := c.get("/admin"); rec.Code != http.StatusSeeOther {
		t.Errorf("session still valid after logout: %d", rec.Code)
	}
}

func TestAdminPagesRender(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	id := e.add(t, "My Blog", true)
	e.apply(t, id, false, time.Now())
	e.apply(t, id, true, time.Now().Add(time.Minute))
	for _, p := range []string{"/admin", "/admin/monitors", "/admin/monitors/new", "/admin/monitors/1", "/admin/monitors/1/edit",
		"/admin/incidents", "/admin/notifications", "/admin/settings"} {
		rec := c.get(p)
		if rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: admin pages must be no-store", p)
		}
	}
	if rec := c.get("/admin/monitors/999"); rec.Code != 404 {
		t.Errorf("missing monitor: %d", rec.Code)
	}
	body := c.get("/admin/monitors/1").Body.String()
	for _, want := range []string{"My Blog", "Check history", "Uptime (24h)", "unexpected status 500"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page missing %q", want)
		}
	}
}

func TestIncidentNoteAndManualResolve(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	id := e.add(t, "API", true)
	e.apply(t, id, false, time.Now()) // retries=0 -> opens
	e.heartbeat(t, time.Now())
	if rec := c.post("/admin/incidents/1/note", url.Values{"note": {"Investigating."}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("note: %d", rec.Code)
	}
	if _, body := e.get("/"); !strings.Contains(body, "Investigating.") {
		t.Error("note not shown publicly")
	}
	if rec := c.post("/admin/incidents/1/note", url.Values{"note": {strings.Repeat("x", 501)}}); rec.Code != 422 {
		t.Errorf("oversized note: %d", rec.Code)
	}
	if rec := c.post("/admin/incidents/1/resolve", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("resolve: %d", rec.Code)
	}
	var state string
	var resolved int
	e.db.QueryRow(`SELECT state FROM monitors WHERE id=1`).Scan(&state)
	e.db.QueryRow(`SELECT COUNT(*) FROM incidents WHERE resolved_at IS NOT NULL`).Scan(&resolved)
	if state != "unknown" || resolved != 1 {
		t.Errorf("state=%s resolved=%d", state, resolved)
	}
	var notes int
	e.db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE kind='recovered'`).Scan(&notes)
	if notes != 0 {
		t.Error("manual resolve must not send a recovery email")
	}
	if rec := c.post("/admin/incidents/1/resolve", nil); rec.Code != 404 {
		t.Errorf("resolving twice: %d", rec.Code)
	}
	if rec := c.post("/admin/incidents/abc/resolve", nil); rec.Code != 404 {
		t.Errorf("bad id: %d", rec.Code)
	}
	// Still failing after a manual resolve -> a fresh incident opens (retries=0).
	e.apply(t, id, false, time.Now().Add(time.Minute))
	var open int
	e.db.QueryRow(`SELECT COUNT(*) FROM incidents WHERE resolved_at IS NULL`).Scan(&open)
	if open != 1 {
		t.Errorf("expected a new open incident, got %d", open)
	}
}

func TestSettings(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	if rec := c.post("/admin/settings", url.Values{"site_title": {"  "}}); rec.Code != 422 {
		t.Errorf("blank title: %d", rec.Code)
	}
	if rec := c.post("/admin/settings", url.Values{"site_title": {"dhruvp status"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d", rec.Code)
	}
	if v, _ := e.db.GetSetting(context.Background(), "notifications_enabled", "1"); v != "0" {
		t.Errorf("unchecked box should disable notifications, got %q", v)
	}
	e.heartbeat(t, time.Now())
	if _, body := e.get("/"); !strings.Contains(body, "dhruvp status") {
		t.Error("title not applied to public page")
	}
}

func TestNotificationsPageHidesSecretsAndTestEmail(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	body := c.get("/admin/notifications").Body.String()
	if strings.Contains(body, "TOPSECRETPASS") {
		t.Fatal("SMTP password rendered")
	}
	if strings.Contains(body, "alice@example.com") {
		t.Error("SMTP username not masked")
	}
	if !strings.Contains(body, "a•••@example.com") {
		t.Error("masked username missing")
	}
	rec := c.post("/admin/notifications/test", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Test email sent") || len(e.sender.sent) != 1 {
		t.Errorf("test email: %d %s sent=%v", rec.Code, rec.Body, e.sender.sent)
	}
	e.sender.err = errors.New(`dial <script>alert(1)</script> failed`)
	rec = c.post("/admin/notifications/test", nil)
	if !strings.Contains(rec.Body.String(), "Failed") || strings.Contains(rec.Body.String(), "<script>") {
		t.Errorf("failure fragment unsafe: %s", rec.Body)
	}
}

func TestCheckHistoryPagination(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	id := e.add(t, "Busy", true)
	base := time.Now().Add(-2 * time.Hour)
	for i := 0; i < 30; i++ {
		e.apply(t, id, true, base.Add(time.Duration(i)*time.Minute))
	}
	page := c.get("/admin/monitors/1").Body.String()
	if !strings.Contains(page, "Load older checks") {
		t.Fatal("pagination button missing")
	}
	m := regexp.MustCompile(`checks\?before=(\d+)`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("before cursor missing")
	}
	frag := c.get("/admin/monitors/1/checks?before=" + m[1]).Body.String()
	if got := strings.Count(frag, "<tr>"); got != 5 { // 30 - 25 remaining rows, no further button
		t.Errorf("fragment rows=%d want 5", got)
	}
	if strings.Contains(frag, "Load older checks") {
		t.Error("unexpected extra button on last page")
	}
}

func TestClientIPTrustsForwardedOnlyFromLoopback(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:1111"
	r.Header.Set("X-Forwarded-For", "6.6.6.6")
	if ip := clientIP(r); ip != "203.0.113.9" {
		t.Errorf("spoofed XFF trusted from public peer: %s", ip)
	}
	r.RemoteAddr = "127.0.0.1:1111"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.4")
	if ip := clientIP(r); ip != "198.51.100.4" {
		t.Errorf("proxy XFF not used (rightmost): %s", ip)
	}
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if ip := clientIP(r); ip != "127.0.0.1" {
		t.Errorf("garbage XFF: %s", ip)
	}
}

func newGet(path string) *http.Request { return httptest.NewRequest("GET", path, nil) }
