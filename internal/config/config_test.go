package config

import (
	"os"
	"testing"
)

func TestResolveAddr(t *testing.T) {
	// Clean env
	os.Unsetenv("LASTSEEN_ADDR")
	os.Unsetenv("PORT")

	if got := resolveAddr(); got != "127.0.0.1:8080" {
		t.Errorf("default addr: got %q, want %q", got, "127.0.0.1:8080")
	}

	// Heroku dynamic PORT
	t.Setenv("PORT", "5000")
	if got := resolveAddr(); got != ":5000" {
		t.Errorf("heroku port: got %q, want %q", got, ":5000")
	}

	// Explicit LASTSEEN_ADDR overrides PORT
	t.Setenv("LASTSEEN_ADDR", "0.0.0.0:3000")
	if got := resolveAddr(); got != "0.0.0.0:3000" {
		t.Errorf("explicit addr: got %q, want %q", got, "0.0.0.0:3000")
	}
}

func TestResolveDBPath(t *testing.T) {
	os.Unsetenv("LASTSEEN_DB")
	os.Unsetenv("LASTSEEN_TURSO_URL")
	os.Unsetenv("TURSO_DATABASE_URL")
	os.Unsetenv("LASTSEEN_TURSO_AUTH_TOKEN")
	os.Unsetenv("TURSO_AUTH_TOKEN")

	// Default local db
	if got := resolveDBPath(); got != "./data/lastseen.db" {
		t.Errorf("default db: got %q, want %q", got, "./data/lastseen.db")
	}

	// LASTSEEN_TURSO_URL with token
	t.Setenv("LASTSEEN_TURSO_URL", "libsql://my-db.turso.io")
	t.Setenv("LASTSEEN_TURSO_AUTH_TOKEN", "secret-token")
	if got := resolveDBPath(); got != "libsql://my-db.turso.io?authToken=secret-token" {
		t.Errorf("turso url + token: got %q", got)
	}

	// TURSO_DATABASE_URL and TURSO_AUTH_TOKEN fallback
	os.Unsetenv("LASTSEEN_TURSO_URL")
	os.Unsetenv("LASTSEEN_TURSO_AUTH_TOKEN")
	t.Setenv("TURSO_DATABASE_URL", "libsql://other-db.turso.io?param=1")
	t.Setenv("TURSO_AUTH_TOKEN", "other-token")
	if got := resolveDBPath(); got != "libsql://other-db.turso.io?param=1&authToken=other-token" {
		t.Errorf("turso url with existing query param: got %q", got)
	}

	// Token already embedded in URL
	t.Setenv("TURSO_DATABASE_URL", "libsql://already.turso.io?authToken=embedded")
	if got := resolveDBPath(); got != "libsql://already.turso.io?authToken=embedded" {
		t.Errorf("already embedded token: got %q", got)
	}
}

func TestLoadDotEnv(t *testing.T) {
	tmp := t.TempDir() + "/.env"
	content := `
# Comment line
TEST_KEY_1=value1
TEST_KEY_2='single quoted'
TEST_KEY_3="double quoted"
`
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("TEST_KEY_1")
	os.Unsetenv("TEST_KEY_2")
	os.Unsetenv("TEST_KEY_3")

	loadDotEnv(tmp)

	if os.Getenv("TEST_KEY_1") != "value1" {
		t.Errorf("key 1: got %q", os.Getenv("TEST_KEY_1"))
	}
	if os.Getenv("TEST_KEY_2") != "single quoted" {
		t.Errorf("key 2: got %q", os.Getenv("TEST_KEY_2"))
	}
	if os.Getenv("TEST_KEY_3") != "double quoted" {
		t.Errorf("key 3: got %q", os.Getenv("TEST_KEY_3"))
	}
}
