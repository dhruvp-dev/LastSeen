package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned when a connection targets a forbidden address.
var ErrBlockedAddress = errors.New("blocked address")

const (
	maxRedirects = 5
	maxBodyBytes = 64 << 10
	userAgent    = "LastSeen/1.0 (+uptime monitor)"
)

// Target describes one check.
type Target struct {
	URL      string
	Method   string
	Timeout  time.Duration
	Expected *StatusMatcher
}

// Result is the outcome of a single check attempt.
type Result struct {
	OK         bool
	StatusCode int
	ResponseMS int64
	Error      string
}

// Guard decides whether a resolved address may be connected to.
type Guard func(netip.AddrPort) error

// DefaultGuard blocks loopback, private, link-local, metadata and other reserved ranges.
func DefaultGuard(ap netip.AddrPort) error {
	if IsBlockedIP(ap.Addr()) {
		return ErrBlockedAddress
	}
	return nil
}

// AllowAll disables the guard (development only).
func AllowAll(netip.AddrPort) error { return nil }

type Checker struct{ client *http.Client }

// NewChecker builds a checker whose every outbound connection, including redirects,
// is validated against guard at dial time (after DNS resolution), defeating DNS rebinding.
func NewChecker(guard Guard) *Checker {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return ErrBlockedAddress
			}
			return guard(ap)
		},
	}
	tr := &http.Transport{
		Proxy:                  nil, // never honour proxy env vars
		DialContext:            dialer.DialContext,
		TLSHandshakeTimeout:    10 * time.Second,
		DisableKeepAlives:      true,
		ResponseHeaderTimeout:  0, // overall deadline comes from the request context
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &Checker{client: &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect to non-http scheme")
			}
			return nil
		},
	}}
}

// Check performs one HTTP check. It never panics and always returns a Result.
func (c *Checker) Check(ctx context.Context, t Target) Result {
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	method := t.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, t.URL, nil)
	if err != nil {
		return Result{Error: "invalid request"}
	}
	req.Header.Set("User-Agent", userAgent)
	start := time.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		return Result{ResponseMS: time.Since(start).Milliseconds(), Error: describe(err, t.Timeout)}
	}
	defer resp.Body.Close()
	if method == http.MethodGet {
		_, _ = io.CopyN(io.Discard, resp.Body, maxBodyBytes) // bounded read, then discard
	}
	ms := time.Since(start).Milliseconds()
	res := Result{StatusCode: resp.StatusCode, ResponseMS: ms}
	if t.Expected != nil && t.Expected.Match(resp.StatusCode) {
		res.OK = true
	} else {
		res.Error = fmt.Sprintf("unexpected status %d", resp.StatusCode)
	}
	return res
}

// describe turns a transport error into a short, URL-free message.
func describe(err error, timeout time.Duration) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	switch {
	case errors.Is(err, ErrBlockedAddress):
		return "blocked address (private or reserved range)"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return fmt.Sprintf("timeout after %s", timeout.Round(time.Second))
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
