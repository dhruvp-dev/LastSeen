package web

import (
	"context"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dhruvp/lastseen/internal/database"
	"github.com/dhruvp/lastseen/internal/notifications"
)

type chartRange struct {
	Key, Label string
	Span, Size time.Duration
}

var chartRanges = []chartRange{
	{"24h", "Last 24 hours", 24 * time.Hour, time.Hour},
	{"7d", "Last 7 days", 7 * 24 * time.Hour, 6 * time.Hour},
	{"30d", "Last 30 days", 30 * 24 * time.Hour, 24 * time.Hour},
	{"90d", "Last 90 days", 90 * 24 * time.Hour, 24 * time.Hour},
}

// parseRange maps user input onto the whitelist; anything unknown becomes 7d.
func parseRange(s string) chartRange {
	for _, r := range chartRanges {
		if r.Key == s {
			return r
		}
	}
	return chartRanges[1]
}

type bucket struct {
	Start     int64 // unix ms, aligned to the bucket size (UTC)
	Total, Up int
	Avg, P95  float64
	HasMS     bool
}

func (b bucket) uptime() (float64, bool) {
	if b.Total == 0 {
		return 0, false
	}
	return float64(b.Up) * 100 / float64(b.Total), true
}

// bucketize groups points into n consecutive buckets of `size` ending at the bucket containing now.
func bucketize(pts []database.SeriesPoint, now time.Time, size time.Duration, n int) []bucket {
	sz := size.Milliseconds()
	last := now.UnixMilli() / sz * sz
	first := last - int64(n-1)*sz
	out := make([]bucket, n)
	times := make([][]float64, n)
	for i := range out {
		out[i].Start = first + int64(i)*sz
	}
	for _, p := range pts {
		if p.At < first || p.At >= last+sz {
			continue
		}
		i := int((p.At - first) / sz)
		out[i].Total++
		if p.OK {
			out[i].Up++
			if p.MS.Valid {
				times[i] = append(times[i], float64(p.MS.Int64))
			}
		}
	}
	for i, t := range times {
		if len(t) == 0 {
			continue
		}
		sort.Float64s(t)
		sum := 0.0
		for _, v := range t {
			sum += v
		}
		out[i].HasMS = true
		out[i].Avg = sum / float64(len(t))
		out[i].P95 = t[int(math.Ceil(0.95*float64(len(t))))-1] // nearest rank
	}
	return out
}

// niceMax rounds v up to a 1/2/5 × 10^k value so axis labels are tidy.
func niceMax(v float64) float64 {
	if v <= 0 {
		return 10
	}
	exp := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 5, 10} {
		if v <= m*exp {
			return m * exp
		}
	}
	return 10 * exp
}

// uptimeFloor picks the lower bound of the uptime axis so small dips stay visible.
func uptimeFloor(min float64) float64 {
	// Each floor splits the axis into four tidy steps: 0.1, 0.5, 1, 2, 5, 10, 25 points.
	for _, f := range []float64{99.6, 98, 96, 92, 80, 60} {
		if min >= f {
			return f
		}
	}
	return 0
}

// dims is a chart's logical size. Fonts are fixed in SVG units, so each chart uses a viewBox close to
// its on-screen width to keep text near 12px. Classes set a floor (the wrapper scrolls on phones)
// and a ceiling (text never balloons on wide screens). They are literal strings so Tailwind sees them.
type dims struct {
	W, H  int
	Class string
}

var (
	overviewDims = dims{720, 250, "min-w-[560px] max-w-[800px]"}
	detailDims   = dims{1100, 280, "min-w-[800px] max-w-[1500px]"}
)

const (
	cLeft, cRight, cTop, cBot = 50, 10, 12, 28
	colBrand                  = "#292929"
	colRule                   = "#e4e4e7"
	colMute                   = "#71717a"
)

const emptyChart = `<p class="text-sm text-mute py-16 text-center">No data yet for this range.</p>`

type frame struct {
	b     *strings.Builder
	d     dims
	n     int
	plotW float64
	plotH float64
	slot  float64
}

func newFrame(n int, d dims) *frame {
	f := &frame{b: &strings.Builder{}, d: d, n: n}
	f.plotW = float64(d.W - cLeft - cRight)
	f.plotH = float64(d.H - cTop - cBot)
	f.slot = f.plotW / float64(n)
	fmt.Fprintf(f.b, `<svg viewBox="0 0 %d %d" class="w-full h-auto %s" role="img" preserveAspectRatio="xMidYMid meet">`, d.W, d.H, d.Class)
	return f
}

func (f *frame) x(i int) float64 { return cLeft + (float64(i)+0.5)*f.slot }

func (f *frame) y(v, lo, hi float64) float64 {
	if hi == lo {
		return cTop + f.plotH
	}
	return cTop + f.plotH*(1-(v-lo)/(hi-lo))
}

func (f *frame) grid(lo, hi float64, label func(float64) string) {
	for i := 0; i <= 4; i++ {
		v := lo + (hi-lo)*float64(i)/4
		y := f.y(v, lo, hi)
		fmt.Fprintf(f.b, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" stroke="%s" stroke-dasharray="3 3"/>`, cLeft, f.d.W-cRight, y, y, colRule)
		fmt.Fprintf(f.b, `<text x="%d" y="%.1f" text-anchor="end" font-size="12" fill="%s">%s</text>`, cLeft-8, y+4, colMute, label(v))
	}
}

func (f *frame) xlabels(bs []bucket, size time.Duration) {
	layout := "15:04"
	if size >= 6*time.Hour {
		layout = "2 Jan"
	}
	step := int(math.Ceil(float64(len(bs)) / 6))
	for i := 0; i < len(bs); i += step {
		fmt.Fprintf(f.b, `<text x="%.1f" y="%d" text-anchor="middle" font-size="12" fill="%s">%s</text>`,
			f.x(i), f.d.H-8, colMute, time.UnixMilli(bs[i].Start).UTC().Format(layout))
	}
}

func (f *frame) close() template.HTML {
	f.b.WriteString(`</svg>`)
	return template.HTML(f.b.String())
}

func tipTime(b bucket, size time.Duration) string {
	if size < 24*time.Hour {
		return time.UnixMilli(b.Start).UTC().Format("2 Jan 15:04") + " UTC"
	}
	return time.UnixMilli(b.Start).UTC().Format("2 Jan 2006")
}

// responseSVG draws average response time as an area chart. Buckets without data break the line;
// buckets with failed checks get a red marker on the baseline.
func responseSVG(bs []bucket, size time.Duration, id string, d dims) template.HTML {
	maxV, any := 0.0, false
	for _, b := range bs {
		if b.HasMS {
			any = true
			maxV = math.Max(maxV, b.Avg)
		}
		if b.Total-b.Up > 0 {
			any = true
		}
	}
	if !any {
		return emptyChart
	}
	hi := niceMax(maxV)
	f := newFrame(len(bs), d)
	fmt.Fprintf(f.b, `<defs><linearGradient id="%s" x1="0" y1="0" x2="0" y2="1"><stop offset="0%%" stop-color="%s" stop-opacity="0.22"/><stop offset="100%%" stop-color="%s" stop-opacity="0"/></linearGradient></defs>`, id, colBrand, colBrand)
	f.grid(0, hi, func(v float64) string { return fmt.Sprintf("%.0f", v) })
	base := f.y(0, 0, hi)

	flush := func(seg []int) {
		if len(seg) == 0 {
			return
		}
		if len(seg) == 1 {
			fmt.Fprintf(f.b, `<circle cx="%.1f" cy="%.1f" r="3" fill="%s"/>`, f.x(seg[0]), f.y(bs[seg[0]].Avg, 0, hi), colBrand)
			return
		}
		var line, area strings.Builder
		for k, i := range seg {
			cmd := "L"
			if k == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&line, "%s%.1f %.1f ", cmd, f.x(i), f.y(bs[i].Avg, 0, hi))
		}
		fmt.Fprintf(&area, "%sL%.1f %.1f L%.1f %.1f Z", line.String(), f.x(seg[len(seg)-1]), base, f.x(seg[0]), base)
		fmt.Fprintf(f.b, `<path d="%s" fill="url(#%s)"/><path d="%s" fill="none" stroke="%s" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>`,
			area.String(), id, line.String(), colBrand)
	}
	var seg []int
	for i, b := range bs {
		if b.HasMS {
			seg = append(seg, i)
		} else {
			flush(seg)
			seg = nil
		}
	}
	flush(seg)

	for i, b := range bs {
		if failed := b.Total - b.Up; failed > 0 {
			fmt.Fprintf(f.b, `<rect x="%.1f" y="%.1f" width="%.1f" height="5" rx="1" fill="#dc2626"/>`, f.x(i)-math.Max(f.slot*0.3, 1.5), base-5, math.Max(f.slot*0.6, 3))
		}
	}
	f.xlabels(bs, size)
	for i, b := range bs { // hover targets with native tooltips
		tip := tipTime(b, size) + " · no checks"
		if b.Total > 0 {
			tip = fmt.Sprintf("%s · %d/%d checks ok", tipTime(b, size), b.Up, b.Total)
			if b.HasMS {
				tip += fmt.Sprintf(" · avg %.0f ms · p95 %.0f ms", b.Avg, b.P95)
			}
		}
		fmt.Fprintf(f.b, `<rect x="%.1f" y="%d" width="%.1f" height="%.0f" fill="transparent"><title>%s</title></rect>`, f.x(i)-f.slot/2, cTop, f.slot, f.plotH, tip)
	}
	return f.close()
}

// uptimeSVG draws uptime per bucket as bars: blue when healthy, amber/red when degraded.
func uptimeSVG(bs []bucket, size time.Duration, d dims) template.HTML {
	minV, any := 100.0, false
	for _, b := range bs {
		if p, ok := b.uptime(); ok {
			any = true
			minV = math.Min(minV, p)
		}
	}
	if !any {
		return emptyChart
	}
	lo := uptimeFloor(minV)
	f := newFrame(len(bs), d)
	f.grid(lo, 100, func(v float64) string { return fmt.Sprintf("%.4g%%", v) })
	base := f.y(lo, lo, 100)
	bw := math.Max(f.slot*0.62, 2)
	for i, b := range bs {
		p, ok := b.uptime()
		tip := tipTime(b, size) + " · no checks"
		if ok {
			tip = fmt.Sprintf("%s · %s uptime · %d/%d checks ok", tipTime(b, size), formatPct(p), b.Up, b.Total)
			h := math.Max(base-f.y(p, lo, 100), 2)
			color := colBrand
			switch {
			case p < 90:
				color = "#dc2626"
			case p < 99:
				color = "#d97706"
			}
			fmt.Fprintf(f.b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" fill="%s" fill-opacity="0.85"/>`, f.x(i)-bw/2, base-h, bw, h, color)
		}
		fmt.Fprintf(f.b, `<rect x="%.1f" y="%d" width="%.1f" height="%.0f" fill="transparent"><title>%s</title></rect>`, f.x(i)-f.slot/2, cTop, f.slot, f.plotH, tip)
	}
	f.xlabels(bs, size)
	return f.close()
}

// ---- data + handlers ----

func (s *Server) chartData(ctx context.Context, monitorID int64, rng chartRange) ([]bucket, error) {
	now := time.Now()
	n := int(rng.Span / rng.Size)
	from := now.Add(-rng.Span - rng.Size) // one bucket of slack for alignment
	pts, err := s.db.Series(ctx, monitorID, from.UnixMilli())
	if err != nil {
		return nil, err
	}
	return bucketize(pts, now, rng.Size, n), nil
}

func (s *Server) overviewChartsData(ctx context.Context, key string) (map[string]any, error) {
	rng := parseRange(key)
	bs, err := s.chartData(ctx, 0, rng)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	data := map[string]any{"Range": rng.Key, "Ranges": chartRanges,
		"Response": responseSVG(bs, rng.Size, "g-resp", detailDims), "Uptime": uptimeSVG(bs, rng.Size, overviewDims),
		"Headline": "—", "HeadSub": "No checks in this period"}
	cur, err := s.db.CheckWindow(ctx, now.Add(-rng.Span).UnixMilli(), now.UnixMilli()+1)
	if err != nil {
		return nil, err
	}
	prev, err := s.db.CheckWindow(ctx, now.Add(-2*rng.Span).UnixMilli(), now.Add(-rng.Span).UnixMilli())
	if err != nil {
		return nil, err
	}
	if cur.AvgMS.Valid {
		data["Headline"] = fmt.Sprintf("%.0f ms", cur.AvgMS.Float64)
		data["HeadSub"] = "average response, " + strings.ToLower(rng.Label)
		if prev.AvgMS.Valid && prev.AvgMS.Float64 > 0 {
			d := (cur.AvgMS.Float64 - prev.AvgMS.Float64) * 100 / prev.AvgMS.Float64
			data["Delta"], data["Good"] = fmt.Sprintf("%+.0f%%", d), d <= 0
			data["HeadSub"] = "vs previous period"
		}
	}
	type incView struct {
		Name, Started, Duration string
		Open                    bool
	}
	var incs []incView
	open := 0
	rows, err := s.db.Incidents(ctx, false, now.AddDate(0, 0, -90).UnixMilli(), 6)
	if err != nil {
		return nil, err
	}
	for _, i := range rows {
		end := now
		if i.ResolvedAt.Valid {
			end = time.UnixMilli(i.ResolvedAt.Int64)
		} else {
			open++
		}
		start := time.UnixMilli(i.StartedAt)
		incs = append(incs, incView{Name: i.MonitorName, Started: start.UTC().Format("2 Jan, 15:04"),
			Duration: notifications.HumanDuration(end.Sub(start)), Open: !i.ResolvedAt.Valid})
	}
	data["Incidents"], data["OpenCount"] = incs, open
	return data, nil
}

func (s *Server) monitorChartData(ctx context.Context, m *database.Monitor, key string) (map[string]any, error) {
	rng := parseRange(key)
	bs, err := s.chartData(ctx, m.ID, rng)
	if err != nil {
		return nil, err
	}
	return map[string]any{"Monitor": m, "Range": rng.Key, "Ranges": chartRanges, "SVG": responseSVG(bs, rng.Size, "g-mon", detailDims)}, nil
}

func (s *Server) overviewCharts(w http.ResponseWriter, r *http.Request) {
	data, err := s.overviewChartsData(r.Context(), r.URL.Query().Get("range"))
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.pages["overview"].ExecuteTemplate(w, "charts", data)
}

func (s *Server) monitorChart(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMonitor(w, r)
	if !ok {
		return
	}
	data, err := s.monitorChartData(r.Context(), m, r.URL.Query().Get("range"))
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.pages["monitor_detail"].ExecuteTemplate(w, "monitor_chart", data)
}
