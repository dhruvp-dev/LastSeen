package monitor

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func tinyPNG() []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	return b.Bytes()
}

func fetch(t *testing.T, guard Guard, target string) ([]byte, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return NewChecker(guard).FetchIcon(ctx, target)
}

func TestFetchIconFollowsLinkTag(t *testing.T) {
	icon := tinyPNG()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><link rel="shortcut icon" href="/assets/fav.png?v=1&amp;x=2"></head><body>hi</body></html>`))
	})
	mux.HandleFunc("/assets/fav.png", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "v=1&x=2" { // the &amp; entity must be decoded
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain") // a lying server: we trust the bytes, not the header
		_, _ = w.Write(icon)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	data, ct, err := fetch(t, AllowAll, srv.URL)
	if err != nil || ct != "image/png" || !bytes.Equal(data, icon) {
		t.Fatalf("got ct=%q len=%d err=%v", ct, len(data), err)
	}
}

func TestFetchIconFallsBackToFaviconIco(t *testing.T) {
	icon := tinyPNG()
	mux := http.NewServeMux()
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(icon) })
	srv := httptest.NewServer(mux) // "/" is a 404, so there is no page to parse
	defer srv.Close()
	if _, ct, err := fetch(t, AllowAll, srv.URL); err != nil || ct != "image/png" {
		t.Fatalf("ct=%q err=%v", ct, err)
	}
}

func TestFetchIconAcceptsSVG(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"/>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if _, ct, err := fetch(t, AllowAll, srv.URL); err != nil || ct != "image/svg+xml" {
		t.Fatalf("ct=%q err=%v", ct, err)
	}
}

func TestFetchIconRejectsNonImages(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter){
		"soft 404 html": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`<!doctype html><html><body><svg></svg></body></html>`))
		},
		"plain text": func(w http.ResponseWriter) { _, _ = w.Write([]byte("not an image")) },
		"too large":  func(w http.ResponseWriter) { _, _ = w.Write(append(tinyPNG(), make([]byte, maxIconBytes)...)) },
		"empty":      func(w http.ResponseWriter) {},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { body(w) }))
			defer srv.Close()
			if _, _, err := fetch(t, AllowAll, srv.URL); !errors.Is(err, ErrNoIcon) {
				t.Fatalf("want ErrNoIcon, got %v", err)
			}
		})
	}
}

// The icon fetcher must honour the SSRF guard: a loopback server gets no request at all.
func TestFetchIconHonoursSSRFGuard(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(tinyPNG())
	}))
	defer srv.Close() // httptest listens on 127.0.0.1
	if _, _, err := fetch(t, DefaultGuard, srv.URL); !errors.Is(err, ErrNoIcon) {
		t.Fatalf("want ErrNoIcon, got %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("guard let %d request(s) through to a loopback address", n)
	}
}

// A page that points its icon at another (internal) host must not make us fetch it.
func TestFetchIconBlocksIconLinkToInternalHost(t *testing.T) {
	var internal atomic.Int32
	internalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internal.Add(1)
		_, _ = w.Write(tinyPNG())
	}))
	defer internalSrv.Close()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<link rel="icon" href="` + internalSrv.URL + `/secret.png">`))
	}))
	defer page.Close()
	pageURL, _ := url.Parse(page.URL)
	// Both servers are on loopback, so tell them apart by port: only the page may be dialled.
	onlyPage := Guard(func(ap netip.AddrPort) error {
		if strconv.Itoa(int(ap.Port())) == pageURL.Port() {
			return nil
		}
		return ErrBlockedAddress
	})
	if _, _, err := fetch(t, onlyPage, page.URL); !errors.Is(err, ErrNoIcon) {
		t.Fatalf("want ErrNoIcon, got %v", err)
	}
	if n := internal.Load(); n != 0 {
		t.Fatalf("fetched %d time(s) from a blocked host", n)
	}
}

func TestIconCandidatesOrderAndFiltering(t *testing.T) {
	doc := `<link rel="apple-touch-icon" href="/apple.png">
		<link rel="mask-icon" href="/mask.svg">
		<link rel="stylesheet" href="/x.css">
		<link href="/norel.png">
		<link rel='icon' href='/small.png'>
		<link rel="icon" href="javascript:alert(1)">
		<link rel="icon" href="//cdn.example.net/cdn.png">`
	base, _ := url.Parse("https://example.com/app/")
	got := iconCandidates([]byte(doc), base)
	want := []string{"https://example.com/small.png", "https://cdn.example.net/cdn.png", "https://example.com/apple.png"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestSniffImageIgnoresClaimsAndMarkup(t *testing.T) {
	if got := sniffImage(tinyPNG()); got != "image/png" {
		t.Errorf("png: %q", got)
	}
	for name, b := range map[string][]byte{
		"html with svg inside": []byte("<html><body><svg/></body></html>"),
		"script":               []byte("<script>alert(1)</script>"),
		"nil":                  nil,
	} {
		if got := sniffImage(b); got != "" {
			t.Errorf("%s accepted as %q", name, got)
		}
	}
}

func TestSanitizeSVGStripsDarkMedia(t *testing.T) {
	input := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16">
  <style>
    path.bg { fill: #0e1f3b; }
    @media (prefers-color-scheme: dark) {
      path.bg { fill: #ffffff; }
    }
  </style>
  <path class="bg" d="M1,1 H13 V9 H4 V15 H1 Z" />
</svg>`)
	got := string(SanitizeSVG(input))
	if strings.Contains(got, "prefers-color-scheme") || strings.Contains(got, "#ffffff") {
		t.Fatalf("dark media query was not stripped: %s", got)
	}
	if !strings.Contains(got, "#0e1f3b") {
		t.Fatalf("canonical color was lost: %s", got)
	}
}
