package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/dhruvp/lastseen/internal/database"
	"github.com/dhruvp/lastseen/internal/monitor"
)

const (
	iconTTL      = 7 * 24 * time.Hour // a found icon is trusted for a week
	iconMissTTL  = time.Hour          // "no icon" (or site down) is retried after an hour
	iconFetchMax = 8 * time.Second
)

// fallbackIcon is shown when a site has no usable icon. A neutral globe, so the layout never jumps.
const fallbackIcon = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="#a3a3a3" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M12 2a14.5 14.5 0 0 0 0 20 14.5 14.5 0 0 0 0-20"/><path d="M2 12h20"/></svg>`

// monitorFavicon serves a monitor's site icon to the signed-in admin.
func (s *Server) monitorFavicon(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMonitor(w, r)
	if !ok {
		return
	}
	s.writeIcon(w, r, m, "private")
}

// publicFavicon serves the icon for the public status page. It answers only for monitors the public page
// itself lists (enabled and public); anything else is a plain 404, so private monitors are not revealed.
func (s *Server) publicFavicon(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m, err := s.db.GetMonitor(r.Context(), id)
	if errors.Is(err, database.ErrNotFound) || (err == nil && (!m.Enabled || !m.IsPublic)) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeIcon(w, r, m, "public")
}

// writeIcon sends the monitor's icon, or the neutral fallback when it has none. scope is the Cache-Control
// audience: "private" for the admin UI, "public" for the status page.
func (s *Server) writeIcon(w http.ResponseWriter, r *http.Request, m *database.Monitor, scope string) {
	ic, err := s.iconFor(r.Context(), m)
	if err != nil {
		s.fail(w, err)
		return
	}
	if ic == nil || len(ic.Data) == 0 {
		serveIcon(w, scope, "image/svg+xml", []byte(fallbackIcon), 5*time.Minute)
		return
	}
	serveIcon(w, scope, ic.ContentType, ic.Data, time.Hour)
}

// iconFor returns a monitor's cached icon, refreshing it when stale. The public endpoint is reachable by
// anyone, so outbound fetches must not scale with traffic: callers for the same monitor queue on one lock,
// and whoever gets it re-checks the cache first, so a burst of requests causes a single fetch. Combined with
// the TTLs that is at most one fetch per monitor per hour (none found) or per week (found).
func (s *Server) iconFor(ctx context.Context, m *database.Monitor) (*database.Icon, error) {
	ic, err := s.db.GetIcon(ctx, m.ID)
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		return nil, err
	}
	if ic != nil && iconFresh(ic, m.URL, time.Now()) {
		return ic, nil
	}
	mu := s.iconLock(m.ID)
	mu.Lock()
	defer mu.Unlock()
	if again, err := s.db.GetIcon(ctx, m.ID); err == nil {
		if iconFresh(again, m.URL, time.Now()) {
			return again, nil
		}
		ic = again
	}
	return s.refreshIcon(ctx, m, ic), nil
}

func (s *Server) iconLock(id int64) *sync.Mutex {
	v, _ := s.iconLocks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func iconFresh(ic *database.Icon, url string, now time.Time) bool {
	if ic.ForURL != url {
		return false
	}
	ttl := iconMissTTL
	if len(ic.Data) > 0 {
		ttl = iconTTL
	}
	return now.Sub(time.UnixMilli(ic.FetchedAt)) < ttl
}

// refreshIcon fetches and stores a new icon. If the fetch fails but an icon for the same URL is already
// cached, the old one keeps being served (a site that is down should not lose its icon).
func (s *Server) refreshIcon(ctx context.Context, m *database.Monitor, old *database.Icon) *database.Icon {
	// A visitor closing the tab must not abort the fetch and cache a false "no icon".
	ctx = context.WithoutCancel(ctx)
	fctx, cancel := context.WithTimeout(ctx, iconFetchMax)
	defer cancel()
	now := time.Now().UnixMilli()
	data, ct, err := s.icons.FetchIcon(fctx, m.URL)
	if err != nil {
		s.log.Debug("favicon fetch failed", "monitor", m.ID, "err", err)
		if old != nil && old.ForURL == m.URL && len(old.Data) > 0 {
			old.FetchedAt = now // back off for an hour before trying again
			_ = s.db.SaveIcon(ctx, m.ID, *old)
			return old
		}
		miss := database.Icon{ForURL: m.URL, FetchedAt: now}
		_ = s.db.SaveIcon(ctx, m.ID, miss)
		return &miss
	}
	ic := database.Icon{ForURL: m.URL, ContentType: ct, Data: data, FetchedAt: now}
	_ = s.db.SaveIcon(ctx, m.ID, ic)
	return &ic
}

// serveIcon writes image bytes with headers that make even an attacker-supplied SVG inert: nosniff, and
// a CSP that forbids scripts and sandboxes the document if the URL is ever opened directly.
func serveIcon(w http.ResponseWriter, scope, contentType string, data []byte, maxAge time.Duration) {
	if contentType == "image/svg+xml" {
		data = monitor.SanitizeSVG(data)
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("Cache-Control", scope+", max-age="+strconv.Itoa(int(maxAge.Seconds())))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	_, _ = w.Write(data)
}
