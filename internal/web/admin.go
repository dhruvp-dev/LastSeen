package web

import (
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/dhruvp/lastseen/internal/database"
	"github.com/dhruvp/lastseen/internal/notifications"
)

var funcs = template.FuncMap{
	"ts": func(ms int64) string { return time.UnixMilli(ms).UTC().Format("2 Jan, 15:04:05") },
	"tsn": func(n sql.NullInt64) string {
		if !n.Valid {
			return "—"
		}
		return time.UnixMilli(n.Int64).UTC().Format("2 Jan, 15:04:05")
	},
	"dur": func(start int64, end sql.NullInt64) string {
		e := time.Now()
		if end.Valid {
			e = time.UnixMilli(end.Int64)
		}
		return notifications.HumanDuration(e.Sub(time.UnixMilli(start)))
	},
	"str": func(n sql.NullString) string { return n.String },
	"num": func(n sql.NullInt64) string {
		if !n.Valid {
			return "—"
		}
		return strconv.FormatInt(n.Int64, 10)
	},
}

const checksPerPage = 25

func (s *Server) monitorDetail(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMonitor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	now := time.Now()
	tiles := []kpi{}
	for _, win := range []struct {
		label string
		d     time.Duration
	}{{"Uptime (24h)", 24 * time.Hour}, {"Uptime (30d)", 30 * 24 * time.Hour}, {"Uptime (90d)", 90 * 24 * time.Hour}} {
		agg, err := s.db.MonitorWindow(ctx, m.ID, now.Add(-win.d).UnixMilli(), now.UnixMilli()+1)
		if err != nil {
			s.fail(w, err)
			return
		}
		v, sub := "—", "No checks yet"
		if p, ok := agg.UptimePct(); ok {
			v, sub = formatPct(p), fmt.Sprintf("%d checks", agg.Total)
		}
		tiles = append(tiles, kpi{Title: win.label, Value: v, FootL: sub})
	}
	day, err := s.db.MonitorWindow(ctx, m.ID, now.Add(-24*time.Hour).UnixMilli(), now.UnixMilli()+1)
	if err != nil {
		s.fail(w, err)
		return
	}
	avg := kpi{Title: "Avg response (24h)", Value: "—", FootL: "No successful checks"}
	if day.AvgMS.Valid {
		avg.Value, avg.FootL = fmt.Sprintf("%.0f ms", day.AvgMS.Float64), "successful checks"
	}
	tiles = append(tiles, avg)

	checks, err := s.db.ChecksPage(ctx, m.ID, 0, checksPerPage)
	if err != nil {
		s.fail(w, err)
		return
	}
	incs, err := s.db.IncidentsForMonitor(ctx, m.ID, 20)
	if err != nil {
		s.fail(w, err)
		return
	}
	chart, err := s.monitorChartData(ctx, m, "24h")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "monitor_detail", s.page(r, m.Name, "monitors", map[string]any{
		"Monitor": m, "Tiles": tiles, "Checks": checks, "NextBefore": nextBefore(checks), "Incidents": incs, "MChart": chart,
	}))
}

func nextBefore(c []database.CheckRow) int64 {
	if len(c) < checksPerPage {
		return 0
	}
	return c[len(c)-1].ID
}

// monitorChecks serves further pages of check history as an HTMX fragment.
func (s *Server) monitorChecks(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMonitor(w, r)
	if !ok {
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	checks, err := s.db.ChecksPage(r.Context(), m.ID, before, checksPerPage)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.pages["monitor_detail"].ExecuteTemplate(w, "check_rows", map[string]any{
		"Monitor": m, "Checks": checks, "NextBefore": nextBefore(checks),
	})
}
