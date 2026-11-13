// Package auth implements single-admin authentication: bcrypt password check,
// server-side sessions (only a hash of the token is stored), CSRF tokens and login rate limiting.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/dhruvp/lastseen/internal/database"
)

const (
	SessionTTL  = 30 * 24 * time.Hour
	MinPassword = 12
	bcryptCost  = 12
)

// HashPassword returns a bcrypt hash suitable for LASTSEEN_ADMIN_PASSWORD_HASH.
func HashPassword(pw string) (string, error) {
	if len(pw) < MinPassword {
		return "", errors.New("password must be at least 12 characters")
	}
	if len(pw) > 72 { // bcrypt silently truncates beyond 72 bytes
		return "", errors.New("password must be at most 72 bytes")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(h), err
}

// ValidateHash checks that s looks like a bcrypt hash.
func ValidateHash(s string) error {
	if _, err := bcrypt.Cost([]byte(s)); err != nil {
		return errors.New("LASTSEEN_ADMIN_PASSWORD_HASH is not a valid bcrypt hash (generate one with `lastseen hash-password`)")
	}
	return nil
}

// Verify reports whether password matches hash. An empty or invalid hash never matches (fail closed).
func Verify(hash, password string) bool {
	if hash == "" || len(password) > 72 {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

type Session struct {
	CSRF      string
	ExpiresAt time.Time
}

type Store struct{ DB *database.DB }

func randToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// Create starts a session and returns the cookie token (never stored in clear).
func (s *Store) Create(ctx context.Context, now time.Time) (token string, sess Session, err error) {
	if token, err = randToken(); err != nil {
		return
	}
	csrf, err := randToken()
	if err != nil {
		return
	}
	sess = Session{CSRF: csrf, ExpiresAt: now.Add(SessionTTL)}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO sessions(token_hash, csrf_token, created_at, expires_at) VALUES (?,?,?,?)`,
		hashToken(token), csrf, now.UnixMilli(), sess.ExpiresAt.UnixMilli())
	return
}

var ErrNoSession = errors.New("no valid session")

// Lookup returns the live session for a cookie token.
func (s *Store) Lookup(ctx context.Context, token string, now time.Time) (Session, error) {
	if token == "" || len(token) > 128 {
		return Session{}, ErrNoSession
	}
	var sess Session
	var exp int64
	err := s.DB.QueryRowContext(ctx, `SELECT csrf_token, expires_at FROM sessions WHERE token_hash=?`, hashToken(token)).Scan(&sess.CSRF, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, err
	}
	if exp <= now.UnixMilli() {
		_, _ = s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, hashToken(token))
		return Session{}, ErrNoSession
	}
	sess.ExpiresAt = time.UnixMilli(exp)
	return sess, nil
}

func (s *Store) Delete(ctx context.Context, token string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, hashToken(token))
	return err
}

// CSRFValid compares tokens in constant time.
func CSRFValid(want, got string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// RateLimiter allows a limited number of failed logins per key within a window (in memory).
type RateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
}

func NewRateLimiter(max int, window time.Duration) *RateLimiter {
	return &RateLimiter{max: max, window: window, fails: map[string][]time.Time{}}
}

func (r *RateLimiter) prune(key string, now time.Time) []time.Time {
	keep := r.fails[key][:0]
	for _, t := range r.fails[key] {
		if now.Sub(t) < r.window {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(r.fails, key)
		return nil
	}
	r.fails[key] = keep
	return keep
}

// Allow reports whether key may attempt a login now.
func (r *RateLimiter) Allow(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.prune(key, now)) < r.max
}

func (r *RateLimiter) Fail(key string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune(key, now)
	if len(r.fails) > 10000 { // bound memory under a distributed attack
		r.fails = map[string][]time.Time{}
	}
	r.fails[key] = append(r.fails[key], now)
}

func (r *RateLimiter) Reset(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.fails, key)
}
