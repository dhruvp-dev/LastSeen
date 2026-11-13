package web

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/dhruvp/lastseen/internal/auth"
	"github.com/dhruvp/lastseen/internal/database"
	"github.com/dhruvp/lastseen/internal/notifications"
)

const cookieName = "lastseen_session"

type ctxKey struct{}

func csrfFrom(r *http.Request) string {
	if sess, ok := r.Context().Value(ctxKey{}).(auth.Session); ok {
		return sess.CSRF
	}
	return ""
}

func (s *Server) setCookie(w http.ResponseWriter, value string, expires time.Time) {
	c := &http.Cookie{Name: cookieName, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: !s.cfg.InsecureCookies}
	if value == "" {
		c.MaxAge = -1
	} else {
		c.Expires = expires
	}
	http.SetCookie(w, c)
}

// requireAuth admits only requests with a live session and, for state-changing methods,
// a valid CSRF token (X-CSRF-Token header or csrf_token form field).
func (s *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		c, err := r.Cookie(cookieName)
		if err != nil {
			s.unauthenticated(w, r)
			return
		}
		sess, err := s.sessions.Lookup(r.Context(), c.Value, time.Now())
		if err != nil {
			s.unauthenticated(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
			got := r.Header.Get("X-CSRF-Token")
			if got == "" {
				got = r.PostFormValue("csrf_token")
			}
			if !auth.CSRFValid(sess.CSRF, got) {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
	})
}

func (s *Server) unauthenticated(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// originCheck rejects cross-site state-changing requests (defence in depth on top of
// SameSite=Lax and CSRF tokens; it also protects the unauthenticated login form).
func (s *Server) originCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			for _, h := range []string{"Origin", "Referer"} {
				if v := r.Header.Get(h); v != "" {
					u, err := url.Parse(v)
					if err != nil || u.Host != r.Host {
						http.Error(w, "cross-origin request blocked", http.StatusForbidden)
						return
					}
					break
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the caller's address. Behind the local Caddy proxy the rightmost
// X-Forwarded-For entry (added by Caddy) is used; otherwise the socket address.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(last) != nil {
				return last
			}
		}
	}
	return host
}

func (s *Server) hasSession(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	_, err = s.sessions.Lookup(r.Context(), c.Value, time.Now())
	return err == nil
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.hasSession(r) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.renderLogin(w, "", http.StatusOK)
}

func (s *Server) renderLogin(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = s.pages["login"].ExecuteTemplate(w, "login", map[string]any{"Error": msg})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<12)
	ip := clientIP(r)
	now := time.Now()
	if !s.limiter.Allow(ip, now) {
		s.log.Warn("login rate limited", "ip", ip)
		w.Header().Set("Retry-After", "900")
		s.renderLogin(w, "Too many failed attempts. Try again in 15 minutes.", http.StatusTooManyRequests)
		return
	}
	if !auth.Verify(s.cfg.AdminPasswordHash, r.PostFormValue("password")) {
		s.limiter.Fail(ip, now)
		s.log.Warn("login failed", "ip", ip)
		s.renderLogin(w, "Incorrect password.", http.StatusUnauthorized)
		return
	}
	s.limiter.Reset(ip)
	if name := cleanName(r.PostFormValue("name")); name != "" {
		_ = s.db.SetSetting(r.Context(), "admin_name", name) // display name only; not used for authentication
	}
	token, sess, err := s.sessions.Create(r.Context(), now)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.setCookie(w, token, sess.ExpiresAt)
	s.log.Info("admin login", "ip", ip)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// cleanName trims a display name, drops control characters and limits it to 40 characters.
func cleanName(in string) string {
	var b []rune
	for _, c := range strings.TrimSpace(in) {
		if unicode.IsControl(c) {
			continue
		}
		b = append(b, c)
		if len(b) == 40 {
			break
		}
	}
	return strings.TrimSpace(string(b))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		_ = s.sessions.Delete(r.Context(), c.Value)
	}
	s.setCookie(w, "", time.Time{})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- notifications & settings pages ----

func (s *Server) notificationsPage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Notifications(r.Context(), 100)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "notifications", s.page(r, "Notifications", "notifications", map[string]any{
		"Rows": rows, "SMTP": s.smtpSummary(),
	}))
}

type smtpSummary struct {
	Configured     bool
	Host, From, To string
	User           string // masked
}

func (s *Server) smtpSummary() smtpSummary {
	c := s.cfg.SMTP
	return smtpSummary{Configured: c.Configured(), Host: fmt.Sprintf("%s:%d", c.Host, c.Port), From: c.From, To: c.To, User: maskUser(c.User)}
}

// maskUser hides most of the SMTP username. The password is never rendered anywhere.
func maskUser(u string) string {
	if u == "" {
		return ""
	}
	if i := strings.Index(u, "@"); i > 1 {
		return u[:1] + "•••" + u[i:]
	}
	if len(u) > 2 {
		return u[:1] + "•••" + u[len(u)-1:]
	}
	return "•••"
}

func (s *Server) sendTestEmail(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err := s.sender.Send(ctx, "[Last Seen] Test email", "This is a test email from Last Seen.\n\nIf you can read this, outage notifications will reach you.\n")
	if err != nil {
		s.log.Warn("test email failed", "err", err)
		fmt.Fprintf(w, `<p class="text-sm text-bad">Failed: %s</p>`, html.EscapeString(err.Error()))
		return
	}
	fmt.Fprint(w, `<p class="text-sm text-ok">Test email sent. Check your inbox.</p>`)
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	title, _ := s.db.GetSetting(ctx, "site_title", "Status")
	enabled, _ := s.db.GetSetting(ctx, "notifications_enabled", "1")
	alive, beat := s.db.WorkerAlive(ctx, time.Now())
	hb := "Never seen"
	if !beat.IsZero() {
		hb = notifications.HumanDuration(time.Since(beat)) + " ago"
	}
	s.render(w, "settings", s.page(r, "Settings", "settings", map[string]any{
		"SiteTitle": title, "NotifyEnabled": enabled == "1", "Saved": r.URL.Query().Get("saved") == "1",
		"Errors": map[string]string{}, "SMTP": s.smtpSummary(), "BaseURL": s.cfg.BaseURL,
		"Interval": s.cfg.CheckInterval.String(), "WorkerAlive": alive, "Heartbeat": hb,
	}))
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	title := strings.TrimSpace(r.PostFormValue("site_title"))
	if n := len([]rune(title)); n < 1 || n > 60 {
		s.settingsError(w, r, title, "Title must be 1–60 characters.")
		return
	}
	notify := "0"
	if r.PostFormValue("notifications_enabled") != "" {
		notify = "1"
	}
	ctx := r.Context()
	if err := s.db.SetSetting(ctx, "site_title", title); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.db.SetSetting(ctx, "notifications_enabled", notify); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/admin/settings?saved=1", http.StatusSeeOther)
}

func (s *Server) settingsError(w http.ResponseWriter, r *http.Request, title, msg string) {
	enabled := r.PostFormValue("notifications_enabled") != ""
	alive, beat := s.db.WorkerAlive(r.Context(), time.Now())
	hb := "Never seen"
	if !beat.IsZero() {
		hb = notifications.HumanDuration(time.Since(beat)) + " ago"
	}
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, "settings", s.page(r, "Settings", "settings", map[string]any{
		"SiteTitle": title, "NotifyEnabled": enabled, "Errors": map[string]string{"site_title": msg},
		"SMTP": s.smtpSummary(), "BaseURL": s.cfg.BaseURL, "Interval": s.cfg.CheckInterval.String(), "WorkerAlive": alive, "Heartbeat": hb,
	}))
}

// ---- incidents ----

type incidentView struct {
	database.IncidentRow
	Open bool
}

func (s *Server) incidentsPage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Incidents(r.Context(), false, 0, 200)
	if err != nil {
		s.fail(w, err)
		return
	}
	views := make([]incidentView, len(rows))
	for i, x := range rows {
		views[i] = incidentView{IncidentRow: x, Open: !x.ResolvedAt.Valid}
	}
	s.render(w, "incidents", s.page(r, "Incidents", "incidents", map[string]any{"Incidents": views}))
}

func (s *Server) incidentID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	var id int64
	if _, err := fmt.Sscan(r.PathValue("id"), &id); err != nil || id <= 0 {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

func (s *Server) incidentNote(w http.ResponseWriter, r *http.Request) {
	id, ok := s.incidentID(w, r)
	if !ok {
		return
	}
	note := strings.TrimSpace(r.PostFormValue("note"))
	if len([]rune(note)) > 500 {
		http.Error(w, "note must be at most 500 characters", http.StatusUnprocessableEntity)
		return
	}
	if err := s.db.SetIncidentNote(r.Context(), id, note); err != nil {
		if err == database.ErrNotFound {
			http.NotFound(w, r)
			return
		}
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/admin/incidents", http.StatusSeeOther)
}

func (s *Server) incidentResolve(w http.ResponseWriter, r *http.Request) {
	id, ok := s.incidentID(w, r)
	if !ok {
		return
	}
	if err := s.db.ResolveIncident(r.Context(), id, time.Now()); err != nil {
		if err == database.ErrNotFound {
			http.NotFound(w, r)
			return
		}
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/admin/incidents", http.StatusSeeOther)
}
