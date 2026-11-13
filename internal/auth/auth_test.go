package auth

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dhruvp/lastseen/internal/database"
)

func TestPasswordHashing(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Error("short password accepted")
	}
	if _, err := HashPassword(strings.Repeat("a", 73)); err == nil {
		t.Error("73-byte password accepted")
	}
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if h == "correct horse battery" || !strings.HasPrefix(h, "$2") {
		t.Errorf("not a bcrypt hash: %q", h)
	}
	if err := ValidateHash(h); err != nil {
		t.Error(err)
	}
	if ValidateHash("plaintext") == nil || ValidateHash("") == nil {
		t.Error("invalid hash accepted")
	}
	if !Verify(h, "correct horse battery") {
		t.Error("correct password rejected")
	}
	for _, bad := range []string{"", "wrong", "correct horse batter", strings.Repeat("x", 200)} {
		if Verify(h, bad) {
			t.Errorf("accepted %q", bad)
		}
	}
	if Verify("", "anything") || Verify("garbage", "anything") {
		t.Error("empty/invalid hash must fail closed")
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Store{DB: db}
}

func TestSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now()
	tok, sess, err := s.Create(ctx, now)
	if err != nil || len(tok) < 40 || sess.CSRF == "" {
		t.Fatalf("create: %v tok=%q", err, tok)
	}
	// Only a hash is stored, never the cookie token.
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash=?`, tok).Scan(&n)
	if n != 0 {
		t.Error("raw token stored in database")
	}
	got, err := s.Lookup(ctx, tok, now.Add(time.Hour))
	if err != nil || got.CSRF != sess.CSRF {
		t.Fatalf("lookup: %v", err)
	}
	if _, err := s.Lookup(ctx, tok+"x", now); err == nil {
		t.Error("tampered token accepted")
	}
	if _, err := s.Lookup(ctx, "", now); err == nil {
		t.Error("empty token accepted")
	}
	// Expiry.
	if _, err := s.Lookup(ctx, tok, now.Add(SessionTTL+time.Minute)); err == nil {
		t.Error("expired session accepted")
	}
	// Delete (logout).
	tok2, _, _ := s.Create(ctx, now)
	s.Delete(ctx, tok2)
	if _, err := s.Lookup(ctx, tok2, now); err == nil {
		t.Error("deleted session still valid")
	}
}

func TestTokensAreUnique(t *testing.T) {
	s := newStore(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		tok, _, _ := s.Create(context.Background(), time.Now())
		if seen[tok] {
			t.Fatal("duplicate token")
		}
		seen[tok] = true
	}
}

func TestCSRFValid(t *testing.T) {
	if !CSRFValid("abc", "abc") || CSRFValid("abc", "abd") || CSRFValid("", "") || CSRFValid("abc", "") {
		t.Error("CSRFValid wrong")
	}
}

func TestRateLimiter(t *testing.T) {
	rl := NewRateLimiter(5, 15*time.Minute)
	now := time.Now()
	for i := 0; i < 5; i++ {
		if !rl.Allow("1.2.3.4", now) {
			t.Fatalf("blocked after only %d failures", i)
		}
		rl.Fail("1.2.3.4", now)
	}
	if rl.Allow("1.2.3.4", now.Add(time.Minute)) {
		t.Error("6th attempt should be blocked")
	}
	if !rl.Allow("5.6.7.8", now) {
		t.Error("other IP affected")
	}
	if !rl.Allow("1.2.3.4", now.Add(16*time.Minute)) {
		t.Error("should unblock after window")
	}
	rl.Fail("9.9.9.9", now)
	rl.Reset("9.9.9.9")
	for i := 0; i < 4; i++ {
		rl.Fail("9.9.9.9", now)
	}
	if !rl.Allow("9.9.9.9", now) {
		t.Error("reset did not clear failures")
	}
}
