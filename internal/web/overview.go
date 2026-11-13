package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dhruvp/lastseen/internal/database"
)

// page adds the fields every admin page needs.
func (s *Server) page(r *http.Request, title, nav string, extra map[string]any) map[string]any {
	extra["Title"] = title
	extra["Nav"] = nav
	extra["CSRF"] = csrfFrom(r)
	extra["Now"] = "Updated " + time.Now().UTC().Format("15:04 UTC")
	if name, err := s.db.GetSetting(r.Context(), "admin_name", ""); err == nil && name != "" {
		extra["AdminName"] = name
		extra["AdminInitial"] = strings.ToUpper(string([]rune(name)[:1]))
	}
	if n, err := s.db.OpenIncidentCount(r.Context()); err == nil {
		extra["OpenIncidents"] = n
	}
	return extra
}

type kpi struct {
	Title, Icon, Tone, Label, Value, FootL, FootR, Delta, Tip string
	Good                                                      bool
}

type serviceView struct {
	*database.ServiceRow
	DotClass, StatusClass, MarkClass, StatusLabel, UptimeText, ResponseText string
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	cur, err := s.db.CheckWindow(ctx, now.Add(-24*time.Hour).UnixMilli(), now.UnixMilli()+1)
	if err != nil {
		s.fail(w, err)
		return
	}
	prev, err := s.db.CheckWindow(ctx, now.Add(-48*time.Hour).UnixMilli(), now.Add(-24*time.Hour).UnixMilli())
	if err != nil {
		s.fail(w, err)
		return
	}
	rows, err := s.db.ServiceRows(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}

	active := 0
	for _, m := range rows {
		if m.Enabled {
			active++
		}
	}

	down, pending := 0, 0
	var downNames []string
	for _, m := range rows {
		if !m.Enabled {
			continue
		}
		switch m.State {
		case "down":
			down++
			downNames = append(downNames, m.Name)
		case "up":
		default:
			pending++
		}
	}
	hero := map[string]any{"Tone": "ok", "Headline": "All systems operational", "Sub": fmt.Sprintf("%d of %d monitors running", active, len(rows))}
	switch {
	case len(rows) == 0:
		hero["Tone"], hero["Headline"], hero["Sub"] = "none", "No monitors yet", "Add your first site to start watching it."
	case active == 0:
		hero["Tone"], hero["Headline"], hero["Sub"] = "none", "Nothing is being checked", fmt.Sprintf("All %d monitors are paused", len(rows))
	case down == 1:
		hero["Tone"], hero["Headline"], hero["Sub"] = "bad", downNames[0]+" is down", "1 service needs attention"
	case down > 1:
		hero["Tone"], hero["Headline"], hero["Sub"] = "bad", fmt.Sprintf("%d services are down", down), strings.Join(downNames, ", ")
	case pending > 0:
		hero["Tone"], hero["Headline"], hero["Sub"] = "warn", "Waiting on first checks", fmt.Sprintf("%d monitor(s) pending", pending)
	}

	up := kpi{Title: "Uptime", Icon: "check", Tone: "tile-green", Label: "Checks that passed", Tip: "Share of checks that passed in the last 24 hours. The change compares it with the 24 hours before.", Value: "—", FootL: "No checks yet", FootR: "last 24h"}
	if v, ok := cur.UptimePct(); ok {
		up.Value, up.FootL, up.FootR = fmt.Sprintf("%.2f%%", v), fmt.Sprintf("%d checks", cur.Total), "vs previous 24h"
		if pv, ok := prev.UptimePct(); ok {
			d := v - pv
			up.Delta, up.Good = fmt.Sprintf("%+.2f pts", d), d >= 0
			up.FootR = "vs previous"
		}
	}
	rt := kpi{Title: "Response time", Icon: "clock", Tone: "tile-orange", Label: "Average of successful checks", Tip: "Average response time of successful checks in the last 24 hours. Lower is faster.", Value: "—", FootL: "No checks yet", FootR: "last 24h"}
	if cur.AvgMS.Valid {
		rt.Value, rt.FootL, rt.FootR = fmt.Sprintf("%.0f ms", cur.AvgMS.Float64), "Last 24 hours", ""
		if prev.AvgMS.Valid && prev.AvgMS.Float64 > 0 {
			d := (cur.AvgMS.Float64 - prev.AvgMS.Float64) * 100 / prev.AvgMS.Float64
			rt.Delta, rt.Good = fmt.Sprintf("%+.0f%%", d), d <= 0 // faster is good
			rt.FootR = "vs previous"
		}
	}
	paused := len(rows) - active
	monFoot := "all running"
	if paused > 0 {
		monFoot = fmt.Sprintf("%d paused", paused)
	}
	checks := kpi{Title: "Checks run", Icon: "send", Tone: "tile-purple", Label: "All attempts", Tip: "Every check attempt in the last 24 hours, including retries. Failures are counted underneath.", Value: fmt.Sprint(cur.Total), FootL: fmt.Sprintf("%d failed", cur.Total-cur.Up), FootR: "last 24h"}
	kpis := []kpi{
		up,
		{Title: "Monitors", Icon: "globe", Tone: "tile-blue", Label: "Being checked", Tip: "Sites Last Seen is checking right now. Paused monitors count toward the total but are not checked.", Value: fmt.Sprint(active), FootL: fmt.Sprintf("of %d total", len(rows)), FootR: monFoot},
		rt,
		checks,
	}

	statusFilter := r.URL.Query().Get("status")
	var views []serviceView
	for i := range rows {
		m := rows[i]
		v := serviceView{ServiceRow: &m, UptimeText: "—", ResponseText: "—"}
		switch {
		case !m.Enabled:
			v.DotClass, v.StatusClass, v.MarkClass, v.StatusLabel = "bg-mute/40", "st-paused", "mk-paused", "Paused"
		case m.State == "up":
			v.DotClass, v.StatusClass, v.MarkClass, v.StatusLabel = "bg-ok", "st-up", "mk-up", "Up"
		case m.State == "down":
			v.DotClass, v.StatusClass, v.MarkClass, v.StatusLabel = "bg-bad", "st-down", "mk-down", "Down"
		default:
			v.DotClass, v.StatusClass, v.MarkClass, v.StatusLabel = "bg-warn", "st-pending", "mk-pending", "Pending"
		}
		if p, ok := m.Uptime24h.UptimePct(); ok {
			v.UptimeText = fmt.Sprintf("%.2f%%", p)
		}
		if m.LastResponseMS.Valid {
			v.ResponseText = fmt.Sprintf("%d ms", m.LastResponseMS.Int64)
		}
		if statusFilter != "" && !strings.EqualFold(v.StatusLabel, statusFilter) {
			continue
		}
		views = append(views, v)
	}
	charts, err := s.overviewChartsData(ctx, "7d")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "overview", s.page(r, "Dashboard", "overview", map[string]any{"KPIs": kpis, "Services": views, "Status": statusFilter, "StatusOptions": []string{"Up", "Down", "Pending", "Paused"}, "ServiceCount": len(views), "Charts": charts, "Hero": hero}))
}
