// Package monitor performs HTTP health checks and validates monitor definitions.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// blockedPrefixes lists ranges a monitor may never target (SSRF protection).
var blockedPrefixes = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
)

var blockedHosts = map[string]bool{
	"localhost": true, "metadata.google.internal": true, "metadata": true, "instance-data": true,
}

var nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

// IsBlockedIP reports whether ip is in a range monitors must not reach.
func IsBlockedIP(ip netip.Addr) bool {
	ip = ip.Unmap().WithZone("")
	if !ip.IsValid() {
		return true
	}
	// NAT64 (DNS64 networks): judge the IPv4 address embedded in the low 32 bits, not the wrapper.
	if nat64Prefix.Contains(ip) {
		b := ip.As16()
		return IsBlockedIP(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ValidateURL checks scheme, host and (via resolver) the addresses a monitor URL points to.
// It returns the normalised URL. resolver may be nil to use the system resolver.
func ValidateURL(ctx context.Context, raw string, resolver *net.Resolver, allowPrivate bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) > 2048 {
		return "", errors.New("URL is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", errors.New("enter a valid URL, e.g. https://example.com")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("only http and https URLs are allowed")
	}
	if u.User != nil {
		return "", errors.New("URLs with credentials are not allowed")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("URL has no host")
	}
	if allowPrivate {
		return u.String(), nil
	}
	if blockedHosts[host] || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return "", errors.New("this host is not allowed")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if IsBlockedIP(ip) {
			return "", errors.New("private, loopback and reserved addresses are not allowed")
		}
		return u.String(), nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := resolver.LookupNetIP(rctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return "", errors.New("host does not resolve")
	}
	for _, a := range addrs {
		if IsBlockedIP(a) {
			return "", errors.New("host resolves to a private or reserved address")
		}
	}
	return u.String(), nil
}

// StatusMatcher matches HTTP status codes against an expression such as "200-299,301".
type StatusMatcher struct{ ranges [][2]int }

func ParseExpectedStatus(expr string) (*StatusMatcher, error) {
	expr = strings.ReplaceAll(strings.TrimSpace(expr), " ", "")
	if expr == "" {
		return nil, errors.New("expected status is required")
	}
	m := &StatusMatcher{}
	for _, part := range strings.Split(expr, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("invalid status %q", part)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				return nil, fmt.Errorf("invalid status range %q", part)
			}
		}
		if a < 100 || b > 599 || a > b {
			return nil, fmt.Errorf("status %q must be within 100-599", part)
		}
		m.ranges = append(m.ranges, [2]int{a, b})
	}
	return m, nil
}

func (m *StatusMatcher) Match(code int) bool {
	for _, r := range m.ranges {
		if code >= r[0] && code <= r[1] {
			return true
		}
	}
	return false
}

// NormalizeStatusExpr returns a canonical form of a validated expression.
func NormalizeStatusExpr(expr string) string {
	return strings.ReplaceAll(strings.TrimSpace(expr), " ", "")
}
