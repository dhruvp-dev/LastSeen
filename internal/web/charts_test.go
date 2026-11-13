package web

import (
	"database/sql"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dhruvp/lastseen/internal/database"
)

func pt(at time.Time, ok bool, ms int64) database.SeriesPoint {
	p := database.SeriesPoint{At: at.UnixMilli(), OK: ok}
	if ms > 0 {
		p.MS = sql.NullInt64{Int64: ms, Valid: true}
	}
	return p
}

func TestBucketize(t *testing.T) {
	now := time.Date(2026, 6, 10, 15, 30, 0, 0, time.UTC)
	hr := func(h, m int) time.Time { return time.Date(2026, 6, 10, h, m, 0, 0, time.UTC) }
	var pts []database.SeriesPoint
	// 15:00 bucket: 20 successes 1..20 ms plus one failure.
	for i := 1; i <= 20; i++ {
		pts = append(pts, pt(hr(15, 5), true, int64(i)))
	}
	pts = append(pts, pt(hr(15, 20), false, 0))
	// 13:00 bucket: only a failure. 14:00 bucket: empty. 12:59 belongs to the 12:00 bucket.
	pts = append(pts, pt(hr(13, 10), false, 0), pt(hr(12, 59), true, 100))
	// Outside the window (before the first bucket and after now's bucket) must be ignored.
	pts = append(pts, pt(hr(15, 5).Add(-30*time.Hour), true, 999), pt(hr(16, 1), true, 999))

	bs := bucketize(pts, now, time.Hour, 6) // 10:00 .. 15:00
	if len(bs) != 6 {
		t.Fatalf("len=%d", len(bs))
	}
	if got := time.UnixMilli(bs[0].Start).UTC().Hour(); got != 10 {
		t.Errorf("first bucket hour=%d want 10", got)
	}
	last := bs[5]
	if last.Total != 21 || last.Up != 20 {
		t.Errorf("15:00 bucket total=%d up=%d", last.Total, last.Up)
	}
	if math.Abs(last.Avg-10.5) > 1e-9 {
		t.Errorf("avg=%v want 10.5", last.Avg)
	}
	if last.P95 != 19 { // nearest rank: ceil(0.95*20)=19th value
		t.Errorf("p95=%v want 19", last.P95)
	}
	if bs[3].Total != 1 || bs[3].Up != 0 || bs[3].HasMS {
		t.Errorf("13:00 bucket: %+v", bs[3])
	}
	if bs[4].Total != 0 {
		t.Errorf("14:00 bucket should be empty: %+v", bs[4])
	}
	if bs[2].Total != 1 || !bs[2].HasMS || bs[2].Avg != 100 { // 12:59 -> 12:00 bucket
		t.Errorf("12:00 bucket: %+v", bs[2])
	}
	for _, b := range bs {
		if b.Total > 0 && b.Total == 999 {
			t.Error("out-of-window point counted")
		}
	}
}

func TestNiceMaxAndUptimeFloor(t *testing.T) {
	cases := map[float64]float64{0: 10, 0.5: 0.5, 7: 10, 142: 200, 200: 200, 201: 500, 999: 1000, 1001: 2000, 4500: 5000}
	for in, want := range cases {
		if got := niceMax(in); got != want {
			t.Errorf("niceMax(%v)=%v want %v", in, got, want)
		}
	}
	for in, want := range map[float64]float64{100: 99.6, 99.7: 99.6, 99.2: 98, 98: 98, 97: 96, 93: 92, 85: 80, 61: 60, 10: 0} {
		if got := uptimeFloor(in); got != want {
			t.Errorf("uptimeFloor(%v)=%v want %v", in, got, want)
		}
	}
}

func TestParseRangeWhitelist(t *testing.T) {
	for in, want := range map[string]string{"24h": "24h", "7d": "7d", "30d": "30d", "90d": "90d", "": "7d", "1y": "7d", "7d'><script>": "7d"} {
		if got := parseRange(in).Key; got != want {
			t.Errorf("parseRange(%q)=%s want %s", in, got, want)
		}
	}
}

func sampleBuckets() []bucket {
	now := time.Date(2026, 6, 10, 15, 0, 0, 0, time.UTC)
	var pts []database.SeriesPoint
	for h := 0; h < 24; h++ {
		at := now.Add(-time.Duration(23-h) * time.Hour)
		switch {
		case h == 5: // gap: no data
		case h == 10:
			pts = append(pts, pt(at, false, 0), pt(at.Add(time.Minute), true, 300))
		default:
			pts = append(pts, pt(at, true, int64(100+h*3)))
		}
	}
	return bucketize(pts, now, time.Hour, 24)
}

func TestResponseSVG(t *testing.T) {
	svg := string(responseSVG(sampleBuckets(), time.Hour, "g-test", overviewDims))
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Fatal("not a well-formed svg wrapper")
	}
	for _, bad := range []string{"NaN", "Inf", "<script", "onload"} {
		if strings.Contains(svg, bad) {
			t.Errorf("svg contains %q", bad)
		}
	}
	if n := strings.Count(svg, "<title>"); n != 24 {
		t.Errorf("tooltips=%d want 24", n)
	}
	if !strings.Contains(svg, "avg ") || !strings.Contains(svg, "p95 ") || !strings.Contains(svg, "no checks") {
		t.Error("tooltip content missing")
	}
	if strings.Count(svg, `fill="#dc2626"`) != 1 {
		t.Error("expected exactly one failure marker")
	}
	if strings.Count(svg, `stroke-linejoin="round"`) != 2 { // the data gap splits the line into two segments
		t.Errorf("gap should split the line into 2 segments, got %d", strings.Count(svg, `stroke-linejoin="round"`))
	}
	if !strings.Contains(svg, `url(#g-test)`) {
		t.Error("gradient reference missing")
	}
}

func TestUptimeSVG(t *testing.T) {
	svg := string(uptimeSVG(sampleBuckets(), time.Hour, overviewDims))
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Fatal("bad numbers in svg")
	}
	if !strings.Contains(svg, "50.00% uptime") && !strings.Contains(svg, "50% uptime") {
		t.Errorf("degraded bucket tooltip missing")
	}
	if !strings.Contains(svg, `fill="#dc2626"`) { // 50% uptime bar is red
		t.Error("degraded bar should be red")
	}
}

func TestChartDimsAppearInSVG(t *testing.T) {
	svg := string(responseSVG(sampleBuckets(), time.Hour, "x", detailDims))
	if !strings.Contains(svg, `viewBox="0 0 1100 280"`) || !strings.Contains(svg, "min-w-[800px]") {
		t.Error("detail dims not applied")
	}
	svg = string(uptimeSVG(sampleBuckets(), time.Hour, overviewDims))
	if !strings.Contains(svg, `viewBox="0 0 720 250"`) || strings.Contains(svg, "NaN") {
		t.Error("overview dims not applied")
	}
}

func TestEmptyCharts(t *testing.T) {
	empty := bucketize(nil, time.Now(), time.Hour, 24)
	if !strings.Contains(string(responseSVG(empty, time.Hour, "x", overviewDims)), "No data yet") || !strings.Contains(string(uptimeSVG(empty, time.Hour, overviewDims)), "No data yet") {
		t.Error("empty state missing")
	}
}

func TestSingleBucketDoesNotPanicOrDivideByZero(t *testing.T) {
	now := time.Now()
	bs := bucketize([]database.SeriesPoint{pt(now, true, 50)}, now, time.Hour, 1)
	if s := string(responseSVG(bs, time.Hour, "x", detailDims)); strings.Contains(s, "NaN") {
		t.Error("NaN with a single bucket")
	}
	if s := string(uptimeSVG(bs, time.Hour, detailDims)); strings.Contains(s, "NaN") {
		t.Error("NaN with a single bucket")
	}
}

func TestChartEndpointsAndOverviewIntegration(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	id := e.add(t, "Charted", true)
	for i := 0; i < 6; i++ {
		e.apply(t, id, i != 3, time.Now().Add(-time.Duration(i)*time.Hour))
	}
	for _, p := range []string{"/admin/charts?range=24h", "/admin/charts?range=30d", "/admin/charts?range=bogus", "/admin/monitors/1/chart?range=7d", "/admin/monitors/1/chart"} {
		rec := c.get(p)
		body := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(body, "<svg") {
			t.Errorf("%s: %d (svg present=%v)", p, rec.Code, strings.Contains(body, "<svg"))
		}
		if strings.Contains(body, "<html") {
			t.Errorf("%s: fragment must not include the page layout", p)
		}
	}
	if body := c.get("/admin/charts?range=24h").Body.String(); !strings.Contains(body, `aria-pressed="true" title="Last 24 hours"`) || strings.Count(body, `aria-pressed="true"`) != 1 {
		t.Error("selected range not reflected in the picker")
	}
	if rec := c.get("/admin/monitors/999/chart"); rec.Code != 404 {
		t.Errorf("chart for missing monitor: %d", rec.Code)
	}
	over := c.get("/admin").Body.String()
	if !strings.Contains(over, "Response time") || !strings.Contains(over, `id="charts"`) || strings.Count(over, "<svg") < 2 {
		t.Error("overview missing charts")
	}
	if rec := e.do(newGet("/admin/charts")); rec.Code != 303 {
		t.Errorf("chart endpoint must require login: %d", rec.Code)
	}
}

func TestPagesAreCSPClean(t *testing.T) {
	e := newEnv(t)
	c := e.login(t)
	id := e.add(t, "A", true)
	e.apply(t, id, false, time.Now())
	e.heartbeat(t, time.Now())
	inlineScript := func(s string) bool {
		for _, part := range strings.Split(s, "<script")[1:] {
			if !strings.Contains(part[:strings.Index(part, ">")], "src=") {
				return true
			}
		}
		return false
	}
	pages := map[string]string{}
	for _, p := range []string{"/admin", "/admin/monitors", "/admin/monitors/new", "/admin/monitors/1", "/admin/incidents", "/admin/notifications", "/admin/settings"} {
		pages[p] = c.get(p).Body.String()
	}
	_, pages["/"] = e.get("/")
	pages["/login"] = e.do(newGet("/login")).Body.String()
	for p, body := range pages {
		if inlineScript(body) {
			t.Errorf("%s: inline <script> would be blocked by the CSP", p)
		}
		for _, bad := range []string{" onclick=", " onsubmit=", " onchange=", " onload=", ` style="`, "javascript:"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s: contains %q (blocked by CSP)", p, bad)
			}
		}
	}
	rec := e.do(newGet("/"))
	got := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(got, want) {
			t.Errorf("CSP missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "unsafe-inline") || strings.Contains(got, "unsafe-eval") {
		t.Error("CSP must not allow unsafe-inline/eval")
	}
}
