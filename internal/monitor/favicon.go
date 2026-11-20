package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const (
	maxIconBytes = 256 << 10
	maxPageBytes = 128 << 10
	maxIconTries = 4
)

// ErrNoIcon means the site declared no icon we could fetch and validate.
var ErrNoIcon = errors.New("no usable icon found")

var (
	linkTagRe   = regexp.MustCompile(`(?is)<link\b[^>]*>`)
	attrRe      = regexp.MustCompile(`(?is)([a-z][a-z0-9:-]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
	darkMediaRe = regexp.MustCompile(`(?is)@media\s*\(\s*prefers-color-scheme\s*:\s*dark\s*\)\s*\{[^{}]*(?:\{[^{}]*\}[^{}]*)*\}`)
)

// SanitizeSVG strips dark mode media queries from SVG icons so they don't turn white/invisible
// when embedded on light surfaces.
func SanitizeSVG(data []byte) []byte {
	return darkMediaRe.ReplaceAll(data, nil)
}

// iconCandidates lists the icon URLs an HTML document declares, best first: rel="icon" (including
// "shortcut icon") before apple-touch-icon. Only absolute http(s) URLs are returned.
func iconCandidates(doc []byte, base *url.URL) []string {
	type cand struct {
		u    string
		rank int
	}
	var cs []cand
	for _, tag := range linkTagRe.FindAll(doc, -1) {
		attrs := map[string]string{}
		for _, m := range attrRe.FindAllStringSubmatch(string(tag), -1) {
			v := m[2]
			if v == "" {
				v = m[3]
			}
			if v == "" {
				v = m[4]
			}
			attrs[strings.ToLower(m[1])] = html.UnescapeString(v)
		}
		href := strings.TrimSpace(attrs["href"])
		if href == "" {
			continue
		}
		rank := -1
		for _, r := range strings.Fields(strings.ToLower(attrs["rel"])) {
			switch {
			case r == "icon":
				rank = 0
			case strings.HasPrefix(r, "apple-touch-icon") && rank != 0:
				rank = 1
			}
		}
		if rank < 0 {
			continue
		}
		ref, err := base.Parse(href)
		if err != nil || (ref.Scheme != "http" && ref.Scheme != "https") {
			continue
		}
		cs = append(cs, cand{ref.String(), rank})
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].rank < cs[j].rank })
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.u
	}
	return out
}

// sniffImage decides what the bytes really are, ignoring whatever the remote server claimed.
// It returns "" for anything that is not a small raster icon or an SVG.
func sniffImage(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	trimmed := bytes.TrimLeft(b, "\xef\xbb\xbf \t\r\n")
	head := bytes.ToLower(trimmed[:min(len(trimmed), 1024)])
	switch {
	case bytes.Contains(head, []byte("<html")), bytes.Contains(head, []byte("<!doctype html")):
		return ""
	case bytes.HasPrefix(head, []byte("<svg")), bytes.HasPrefix(head, []byte("<?xml")) && bytes.Contains(head, []byte("<svg")):
		return "image/svg+xml"
	}
	switch ct := http.DetectContentType(b); ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/x-icon":
		return ct
	}
	return ""
}

// get fetches one URL through the guarded client and returns at most max bytes of body.
// With truncate set an oversized body is cut; otherwise it is an error.
func (c *Checker) get(ctx context.Context, rawURL string, max int64, truncate bool) ([]byte, *url.URL, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(b)) > max {
		if !truncate {
			return nil, nil, errors.New("response too large")
		}
		b = b[:max]
	}
	return b, resp.Request.URL, nil
}

// FetchIcon finds and downloads a site's icon: the icons declared in the page's <link> tags first,
// then /favicon.ico. Every request goes through the checker's guarded client, so the SSRF protection
// (dial-time address checks, redirect cap) applies exactly as it does to checks. The returned
// content type comes from sniffing the bytes, never from the remote server.
func (c *Checker) FetchIcon(ctx context.Context, pageURL string) ([]byte, string, error) {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, "", err
	}
	var cands []string
	if body, final, err := c.get(ctx, pageURL, maxPageBytes, true); err == nil {
		cands = iconCandidates(body, final)
		base = final
	}
	cands = append(cands, base.ResolveReference(&url.URL{Path: "/favicon.ico"}).String())
	seen := map[string]bool{}
	tries := 0
	for _, u := range cands {
		if seen[u] {
			continue
		}
		seen[u] = true
		if tries++; tries > maxIconTries {
			break
		}
		data, _, err := c.get(ctx, u, maxIconBytes, false)
		if err != nil {
			continue
		}
		if ct := sniffImage(data); ct != "" {
			if ct == "image/svg+xml" {
				data = SanitizeSVG(data)
			}
			return data, ct, nil
		}
	}
	return nil, "", ErrNoIcon
}
