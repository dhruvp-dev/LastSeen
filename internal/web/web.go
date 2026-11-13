// Package web contains HTTP handlers and embedded templates.
package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dhruvp/lastseen/internal/auth"
	"github.com/dhruvp/lastseen/internal/config"
	"github.com/dhruvp/lastseen/internal/database"
	"github.com/dhruvp/lastseen/internal/monitor"
	"github.com/dhruvp/lastseen/internal/notifications"
)

//go:embed templates/*.html
var tplFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	cfg       *config.Config
	db        *database.DB
	log       *slog.Logger
	sender    notifications.Sender
	sessions  *auth.Store
	limiter   *auth.RateLimiter
	pages     map[string]*template.Template
	icons     *monitor.Checker // fetches site icons; same SSRF guard as the checks
	iconLocks sync.Map         // monitor ID -> *sync.Mutex, one icon fetch at a time per monitor
}

// layoutPages render inside layout.html; standalone pages define their own document.
var layoutPages = []string{"overview", "monitors", "monitor_form", "monitor_detail", "incidents", "notifications", "settings"}
var standalonePages = []string{"public", "login"}

func New(cfg *config.Config, db *database.DB, log *slog.Logger, sender notifications.Sender) (*Server, error) {
	guard := monitor.Guard(monitor.DefaultGuard)
	if cfg.AllowPrivate {
		guard = monitor.AllowAll
	}
	s := &Server{cfg: cfg, db: db, log: log, sender: sender, sessions: &auth.Store{DB: db},
		limiter: auth.NewRateLimiter(5, 15*time.Minute), pages: map[string]*template.Template{}, icons: monitor.NewChecker(guard)}
	for _, p := range layoutPages {
		t, err := template.New(p).Funcs(funcs).ParseFS(tplFS, "templates/layout.html", "templates/"+p+".html", "templates/icons.html")
		if err != nil {
			return nil, err
		}
		s.pages[p] = t
	}
	for _, p := range standalonePages {
		t, err := template.New(p).Funcs(funcs).ParseFS(tplFS, "templates/"+p+".html", "templates/icons.html")
		if err != nil {
			return nil, err
		}
		s.pages[p] = t
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /icon/{id}", s.publicFavicon)
	mux.HandleFunc("GET /{$}", s.public)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)

	adm := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireAuth(h)) }
	adm("POST /logout", s.logout)
	adm("GET /admin", s.overview)
	adm("GET /admin/charts", s.overviewCharts)
	adm("GET /admin/monitors", s.listMonitors)
	adm("GET /admin/monitors/new", s.newMonitor)
	adm("POST /admin/monitors", s.createMonitor)
	adm("GET /admin/monitors/{id}", s.monitorDetail)
	adm("GET /admin/monitors/{id}/checks", s.monitorChecks)
	adm("GET /admin/monitors/{id}/chart", s.monitorChart)
	adm("GET /admin/monitors/{id}/favicon", s.monitorFavicon)
	adm("GET /admin/monitors/{id}/edit", s.editMonitor)
	adm("POST /admin/monitors/{id}", s.updateMonitor)
	adm("POST /admin/monitors/{id}/toggle", s.toggleMonitor)
	adm("DELETE /admin/monitors/{id}", s.deleteMonitor)
	adm("GET /admin/incidents", s.incidentsPage)
	adm("POST /admin/incidents/{id}/note", s.incidentNote)
	adm("POST /admin/incidents/{id}/resolve", s.incidentResolve)
	adm("GET /admin/notifications", s.notificationsPage)
	adm("POST /admin/notifications/test", s.sendTestEmail)
	adm("GET /admin/settings", s.settingsPage)
	adm("POST /admin/settings", s.settingsSave)
	return s.recoverer(s.originCheck(mux))
}

// csp forbids inline script/style and third-party origins; all assets are self-hosted.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", csp)
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic", "path", r.URL.Path, "err", v)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("render", "page", page, "err", err)
	}
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	if alive, _ := s.db.WorkerAlive(ctx, time.Now()); !alive {
		http.Error(w, "worker heartbeat stale", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok\n"))
}

func (s *Server) listMonitors(w http.ResponseWriter, r *http.Request) {
	ms, err := s.db.ListMonitors(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "monitors", s.page(r, "Monitors", "monitors", map[string]any{"Monitors": ms}))
}

// ---- forms ----

type monitorForm struct {
	Name, Slug, URL, Method, ExpectedStat string
	TimeoutS, MaxRetries, RetryDelayS     string
	Enabled, IsPublic                     bool
}

func defaultForm() monitorForm {
	return monitorForm{Method: "GET", ExpectedStat: "200-299", TimeoutS: "10", MaxRetries: "2", RetryDelayS: "30", Enabled: true, IsPublic: true}
}

func (s *Server) formPage(w http.ResponseWriter, r *http.Request, id int64, f monitorForm, errs map[string]string, code int) {
	title := "Add monitor"
	if id != 0 {
		title = "Edit monitor"
	}
	data := s.page(r, title, "monitors", map[string]any{"ID": id, "Form": f, "Errors": errs})
	if isHX(r) { // opened as a modal: return just the dialog
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(code)
		if err := s.pages["monitor_form"].ExecuteTemplate(w, "monitor_modal", data); err != nil {
			s.log.Error("render modal", "err", err)
		}
		return
	}
	w.WriteHeader(code)
	s.render(w, "monitor_form", data)
}

// isHX reports whether the request came from htmx (a partial update rather than a page load).
func isHX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Boosted") != "true"
}

// done finishes a successful form post: htmx clients get a client-side redirect, others a normal one.
func done(w http.ResponseWriter, r *http.Request, to string) {
	if isHX(r) {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) newMonitor(w http.ResponseWriter, r *http.Request) {
	s.formPage(w, r, 0, defaultForm(), nil, http.StatusOK)
}

func (s *Server) editMonitor(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMonitor(w, r)
	if !ok {
		return
	}
	s.formPage(w, r, m.ID, monitorForm{Name: m.Name, Slug: m.Slug, URL: m.URL, Method: m.Method, ExpectedStat: m.ExpectedStat,
		TimeoutS: strconv.Itoa(m.TimeoutMS / 1000), MaxRetries: strconv.Itoa(m.MaxRetries),
		RetryDelayS: strconv.Itoa(m.RetryDelayS), Enabled: m.Enabled, IsPublic: m.IsPublic}, nil, http.StatusOK)
}

var slugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// parseForm validates user input and returns a Monitor ready to store.
func (s *Server) parseForm(r *http.Request, id int64) (*database.Monitor, monitorForm, map[string]string) {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<16)
	_ = r.ParseForm()
	f := monitorForm{
		Name: strings.TrimSpace(r.PostFormValue("name")), Slug: strings.TrimSpace(r.PostFormValue("slug")),
		URL: strings.TrimSpace(r.PostFormValue("url")), Method: r.PostFormValue("method"),
		ExpectedStat: monitor.NormalizeStatusExpr(r.PostFormValue("expected_status")), TimeoutS: strings.TrimSpace(r.PostFormValue("timeout_s")),
		MaxRetries: strings.TrimSpace(r.PostFormValue("max_retries")), RetryDelayS: strings.TrimSpace(r.PostFormValue("retry_delay_s")),
		Enabled: r.PostFormValue("enabled") != "", IsPublic: r.PostFormValue("is_public") != "",
	}
	errs := map[string]string{}
	if n := len([]rune(f.Name)); n < 1 || n > 80 {
		errs["name"] = "Name must be 1–80 characters."
	}
	if f.Method != "GET" && f.Method != "HEAD" {
		f.Method = "GET"
	}
	if f.Slug == "" {
		f.Slug = slugify(f.Name)
	}
	if !slugRe.MatchString(f.Slug) || len(f.Slug) > 60 {
		errs["slug"] = "Use lowercase letters, digits and single hyphens (max 60)."
	} else if taken, err := s.db.SlugExists(r.Context(), f.Slug, id); err != nil {
		errs["slug"] = "Could not check slug."
	} else if taken {
		errs["slug"] = "This slug is already in use."
	}
	timeout, err := strconv.Atoi(f.TimeoutS)
	if err != nil || timeout < 1 || timeout > 60 {
		errs["timeout_s"] = "Timeout must be 1–60 seconds."
	}
	retries, err := strconv.Atoi(f.MaxRetries)
	if err != nil || retries < 0 || retries > 5 {
		errs["max_retries"] = "Retries must be 0–5."
	}
	delay, err := strconv.Atoi(f.RetryDelayS)
	if err != nil || delay < 10 || delay > 1800 {
		errs["retry_delay_s"] = "Wait must be 10–1800 seconds (max 30 minutes)."
	}
	if _, err := monitor.ParseExpectedStatus(f.ExpectedStat); err != nil {
		errs["expected_status"] = err.Error()
	}
	norm, err := monitor.ValidateURL(r.Context(), f.URL, nil, s.cfg.AllowPrivate)
	if err != nil {
		errs["url"] = err.Error()
	}
	if len(errs) > 0 {
		return nil, f, errs
	}
	return &database.Monitor{ID: id, Name: f.Name, Slug: f.Slug, URL: norm, Method: f.Method, ExpectedStat: f.ExpectedStat,
		TimeoutMS: timeout * 1000, RetryDelayS: delay, MaxRetries: retries, Enabled: f.Enabled, IsPublic: f.IsPublic}, f, nil
}

func (s *Server) createMonitor(w http.ResponseWriter, r *http.Request) {
	m, f, errs := s.parseForm(r, 0)
	if m == nil {
		s.formPage(w, r, 0, f, errs, http.StatusUnprocessableEntity)
		return
	}
	if err := s.db.CreateMonitor(r.Context(), m); err != nil {
		if errors.Is(err, database.ErrSlugTaken) {
			s.formPage(w, r, 0, f, map[string]string{"slug": "This slug is already in use."}, http.StatusUnprocessableEntity)
			return
		}
		s.fail(w, err)
		return
	}
	done(w, r, "/admin/monitors")
}

func (s *Server) updateMonitor(w http.ResponseWriter, r *http.Request) {
	old, ok := s.loadMonitor(w, r)
	if !ok {
		return
	}
	m, f, errs := s.parseForm(r, old.ID)
	if m == nil {
		s.formPage(w, r, old.ID, f, errs, http.StatusUnprocessableEntity)
		return
	}
	if err := s.db.UpdateMonitor(r.Context(), m); err != nil {
		if errors.Is(err, database.ErrSlugTaken) {
			s.formPage(w, r, old.ID, f, map[string]string{"slug": "This slug is already in use."}, http.StatusUnprocessableEntity)
			return
		}
		s.fail(w, err)
		return
	}
	done(w, r, "/admin/monitors")
}

func (s *Server) toggleMonitor(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMonitor(w, r)
	if !ok {
		return
	}
	if err := s.db.SetMonitorEnabled(r.Context(), m.ID, !m.Enabled); err != nil {
		s.fail(w, err)
		return
	}
	m, _ = s.db.GetMonitor(r.Context(), m.ID)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.pages["monitors"].ExecuteTemplate(w, "row", m)
}

func (s *Server) deleteMonitor(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMonitor(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteMonitor(r.Context(), m.ID); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusOK) // empty body: htmx removes the row
}

func (s *Server) loadMonitor(w http.ResponseWriter, r *http.Request) (*database.Monitor, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	m, err := s.db.GetMonitor(r.Context(), id)
	if errors.Is(err, database.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return m, true
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// ListenAndServe runs the HTTP server until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr: s.cfg.Addr, Handler: s.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	s.log.Info("web server listening", "addr", s.cfg.Addr)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
