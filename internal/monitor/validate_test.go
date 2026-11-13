package monitor

import (
	"context"
	"net/netip"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "172.16.0.1": true, "172.31.255.255": true, "172.32.0.1": false,
		"192.168.1.1": true, "169.254.169.254": true, "100.64.0.1": true, "0.0.0.0": true,
		"::1": true, "fe80::1": true, "fc00::1": true, "::ffff:127.0.0.1": true, "::ffff:169.254.169.254": true,
		"224.0.0.1": true, "255.255.255.255": true,
		"8.8.8.8": false, "1.1.1.1": false, "2606:4700:4700::1111": false, "::ffff:8.8.8.8": false,
	}
	for s, want := range cases {
		if got := IsBlockedIP(netip.MustParseAddr(s)); got != want {
			t.Errorf("IsBlockedIP(%s)=%v want %v", s, got, want)
		}
	}
}

func TestValidateURL(t *testing.T) {
	ctx := context.Background()
	bad := []string{
		"", "ftp://example.com", "file:///etc/passwd", "javascript:alert(1)", "https://", "//example.com",
		"http://user:pw@example.com", "http://127.0.0.1", "http://localhost:8080", "http://169.254.169.254/latest/meta-data",
		"http://[::1]/", "http://metadata.google.internal/", "http://10.0.0.5", "http://foo.localhost", "http://[::ffff:127.0.0.1]/",
	}
	for _, u := range bad {
		if _, err := ValidateURL(ctx, u, nil, false); err == nil {
			t.Errorf("expected %q to be rejected", u)
		}
	}
	if _, err := ValidateURL(ctx, "https://8.8.8.8/health", nil, false); err != nil {
		t.Errorf("public IP rejected: %v", err)
	}
	if _, err := ValidateURL(ctx, "http://127.0.0.1:9", nil, true); err != nil {
		t.Errorf("allowPrivate should bypass: %v", err)
	}
}

func TestExpectedStatus(t *testing.T) {
	m, err := ParseExpectedStatus("200-299, 301,404")
	if err != nil {
		t.Fatal(err)
	}
	for code, want := range map[int]bool{200: true, 299: true, 300: false, 301: true, 404: true, 500: false} {
		if m.Match(code) != want {
			t.Errorf("Match(%d)=%v want %v", code, !want, want)
		}
	}
	for _, bad := range []string{"", "abc", "99", "600", "300-200", "200-", "-5", "200,,300"} {
		if _, err := ParseExpectedStatus(bad); err == nil {
			t.Errorf("expected %q to be invalid", bad)
		}
	}
}
