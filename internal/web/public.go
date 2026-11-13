package web

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/dhruvp/lastseen/internal/database"
	"github.com/dhruvp/lastseen/internal/notifications"
)

type publicService struct {
	Name, StateLabel, StateClass, StateIcon, Uptime30, Uptime90, Response string
	Strip                                                                 template.HTML
}

type publicIncident struct {
	Monitor, Started, Duration, Note string
}

const longStamp = "2 Jan 2006, 15:04"

func (s *Server) public(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	rows, err := s.db.ServiceRows(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	from := now.AddDate(0, 0, -89)
	fromDay := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	stats, err := s.db.DailyStatsAll(ctx, fromDay.UnixMilli())
	if err != nil {
		s.fail(w, err)
		return
	}
	alive, _ := s.db.WorkerAlive(ctx, now)

	days := make([]string, 90)
	for i := range days {
		days[i] = database.DayString(now.AddDate(0, 0, -(89 - i)))
	}
	day30 := database.DayString(now.AddDate(0, 0, -29))

	var services []publicService
	var lastChecked int64
	down, total := 0, 0
	for _, m := range rows {
		if !m.Enabled || !m.IsPublic {
			continue
		}
		total++
		ps := publicService{Name: m.Name, Uptime30: "—", Uptime90: "—"}
		switch m.State {
		case "up":
			ps.StateLabel, ps.StateClass = "Operational", "text-pub-ok"
			ps.StateIcon = "check"
		case "down":
			ps.StateLabel, ps.StateClass = "Outage", "text-pub-bad"
			ps.StateIcon = "alert"
			down++
		default:
			ps.StateLabel, ps.StateClass = "Awaiting data", "text-pub-subtle"
			ps.StateIcon = "clock"
		}
		if m.LastResponseMS.Valid {
			ps.Response = fmt.Sprintf("%d ms", m.LastResponseMS.Int64)
		}
		mine := stats[m.ID]
		if p, ok := database.SumDays(mine, day30).UptimePct(); ok {
			ps.Uptime30 = formatPct(p)
		}
		if p, ok := database.SumDays(mine, days[0]).UptimePct(); ok {
			ps.Uptime90 = formatPct(p)
		}
		ps.Strip = stripSVG(days, mine)
		if m.LastCheckedAt.Valid && m.LastCheckedAt.Int64 > lastChecked {
			lastChecked = m.LastCheckedAt.Int64
		}
		services = append(services, ps)
	}

	incs, err := s.db.Incidents(ctx, true, now.AddDate(0, 0, -90).UnixMilli(), 60)
	if err != nil {
		s.fail(w, err)
		return
	}
	var active, resolved []publicIncident
	for _, i := range incs {
		start := time.UnixMilli(i.StartedAt)
		pi := publicIncident{Monitor: i.MonitorName, Started: start.UTC().Format(longStamp) + " UTC", Note: i.Note.String}
		if i.ResolvedAt.Valid {
			pi.Duration = notifications.HumanDuration(time.UnixMilli(i.ResolvedAt.Int64).Sub(start))
			if len(resolved) < 10 {
				resolved = append(resolved, pi)
			}
		} else {
			pi.Duration = notifications.HumanDuration(now.Sub(start))
			active = append(active, pi)
		}
	}

	title, _ := s.db.GetSetting(ctx, "site_title", "Status")
	data := map[string]any{"SiteTitle": title, "Services": services, "Active": active, "Resolved": resolved}
	data["Overall"], data["Detail"], data["OverallDot"] = overall(alive, total, down)
	if lastChecked > 0 {
		data["LastChecked"] = time.UnixMilli(lastChecked).UTC().Format(longStamp + " UTC")
	}
	w.Header().Set("Cache-Control", "public, max-age=30")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages["public"].ExecuteTemplate(w, "public", data); err != nil {
		s.log.Error("render public", "err", err)
	}
}

// overall summarises health in one sentence. A stale worker heartbeat overrides green:
// we must not claim "operational" when nothing is being checked.
func overall(workerAlive bool, total, down int) (headline, detail, dotClass string) {
	switch {
	case !workerAlive:
		return "Monitoring paused", "Checks are not currently running, so the information below may be out of date.", "bg-pub-warn"
	case total == 0:
		return "No services configured", "", "bg-pub-subtle"
	case down == 0:
		return "All systems operational", "", "bg-pub-ok pulse"
	case down == total:
		return "Major outage", "All monitored services are currently unavailable.", "bg-pub-bad pulse"
	case down == 1:
		return "1 service is down", "", "bg-pub-bad pulse"
	}
	return fmt.Sprintf("%d services are down", down), "", "bg-pub-bad pulse"
}

func formatPct(p float64) string {
	if p >= 100 {
		return "100%"
	}
	if p > 99.99 { // never round a real outage up to a perfect score
		p = 99.99
	}
	return fmt.Sprintf("%.2f%%", p)
}

// stripSVG draws one bar per day (oldest first). The string only contains numbers and
// our own colour constants plus formatted dates, so it is safe to mark as trusted HTML.
func stripSVG(days []string, stats []database.DayStat) template.HTML {
	byDay := make(map[string]database.DayStat, len(stats))
	for _, d := range stats {
		byDay[d.Day] = d
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d 30" preserveAspectRatio="none" class="w-full h-8 strip-svg" role="img" aria-label="Uptime for the last %d days">`, len(days)*10, len(days))
	for i, day := range days {
		color := "d-none"
		pctText := "No data"
		status := "none"
		var detail string

		t, err := time.Parse("2006-01-02", day)
		dateLabel := day
		if err == nil {
			dateLabel = t.Format("2 Jan 2006")
		}
		if i == len(days)-1 {
			dateLabel += " · Today"
		}

		if d, ok := byDay[day]; ok {
			if p, ok := d.UptimePct(); ok {
				pct := formatPct(p)
				pctText = pct + " uptime"
				switch {
				case p >= 99:
					color = "d-ok"
					status = "ok"
				case p >= 90:
					color = "d-warn"
					status = "warn"
				default:
					color = "d-bad"
					status = "bad"
				}
				if failed := d.Total - d.Up; failed > 0 {
					detail = fmt.Sprintf("%d failed", failed)
					if d.AvgMS.Valid && d.AvgMS.Float64 > 0 {
						detail += fmt.Sprintf(" · %.0f ms", d.AvgMS.Float64)
					}
				} else if d.AvgMS.Valid && d.AvgMS.Float64 > 0 {
					detail = fmt.Sprintf("%.0f ms avg", d.AvgMS.Float64)
				}
			}
		}

		ariaLabel := fmt.Sprintf("%s · %s", dateLabel, pctText)
		if detail != "" {
			ariaLabel += " · " + detail
		}

		fmt.Fprintf(&b, `<rect x="%d" y="0" width="8" height="30" rx="1.5" class="strip-bar %s" data-date="%s" data-pct="%s" data-status="%s" data-detail="%s" aria-label="%s" tabindex="0"></rect>`,
			i*10, color, dateLabel, pctText, status, detail, ariaLabel)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
