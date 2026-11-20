// Package config loads application configuration from LASTSEEN_* environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type SMTP struct {
	Host, User, Pass, From, To string
	Port                       int
}

// Configured reports whether enough SMTP settings exist to send mail.
func (s SMTP) Configured() bool { return s.Host != "" && s.From != "" && s.To != "" }

type Config struct {
	Addr              string
	DBPath            string
	BaseURL           string
	AdminPasswordHash string
	BackupKeep        int           // number of daily snapshots to retain
	CheckInterval     time.Duration // dev override via LASTSEEN_CHECK_INTERVAL
	AllowPrivate      bool          // dev only: disables SSRF guard (never set in production)
	InsecureCookies   bool          // dev only: drop the Secure flag so http://localhost works
	SMTP              SMTP
}

func Load() (*Config, error) {
	loadDotEnv(".env")

	c := &Config{
		Addr:              resolveAddr(),
		DBPath:            resolveDBPath(),
		BaseURL:           get("LASTSEEN_BASE_URL", "https://status.dhruvp.tech"),
		AdminPasswordHash: os.Getenv("LASTSEEN_ADMIN_PASSWORD_HASH"),
		SMTP: SMTP{
			Host: os.Getenv("LASTSEEN_SMTP_HOST"),
			User: os.Getenv("LASTSEEN_SMTP_USER"),
			Pass: os.Getenv("LASTSEEN_SMTP_PASS"),
			From: os.Getenv("LASTSEEN_SMTP_FROM"),
			To:   os.Getenv("LASTSEEN_SMTP_TO"),
		},
	}
	var err error
	if c.SMTP.Port, err = intEnv("LASTSEEN_SMTP_PORT", 587); err != nil {
		return nil, err
	}
	if c.BackupKeep, err = intEnv("LASTSEEN_BACKUP_KEEP", 14); err != nil {
		return nil, err
	}
	if c.CheckInterval, err = durEnv("LASTSEEN_CHECK_INTERVAL", 20*time.Minute); err != nil {
		return nil, err
	}
	c.AllowPrivate = os.Getenv("LASTSEEN_DEV_ALLOW_PRIVATE") == "1"
	c.InsecureCookies = os.Getenv("LASTSEEN_DEV_INSECURE_COOKIES") == "1"
	return c, nil
}

func resolveAddr() string {
	if addr := os.Getenv("LASTSEEN_ADDR"); addr != "" {
		return addr
	}
	// On Heroku and similar container environments, PORT is set dynamically.
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return "127.0.0.1:8080"
}

func resolveDBPath() string {
	tursoURL := getFirst("LASTSEEN_TURSO_URL", "TURSO_DATABASE_URL")
	token := getFirst("LASTSEEN_TURSO_AUTH_TOKEN", "TURSO_AUTH_TOKEN")

	dbPath := tursoURL
	if dbPath == "" {
		dbPath = get("LASTSEEN_DB", "./data/lastseen.db")
	}

	if isRemoteURL(dbPath) && token != "" && !strings.Contains(dbPath, "authToken=") {
		sep := "?"
		if strings.Contains(dbPath, "?") {
			sep = "&"
		}
		dbPath += sep + "authToken=" + token
	}
	return dbPath
}

func isRemoteURL(path string) bool {
	return strings.HasPrefix(path, "libsql://") ||
		strings.HasPrefix(path, "https://") ||
		strings.HasPrefix(path, "http://") ||
		strings.HasPrefix(path, "wss://") ||
		strings.HasPrefix(path, "ws://")
}

func getFirst(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func get(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func intEnv(k string, def int) (int, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 65535 {
		return 0, fmt.Errorf("%s: invalid value %q", k, v)
	}
	return n, nil
}

func durEnv(k string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: invalid duration %q", k, v)
	}
	return d, nil
}

func loadDotEnv(filename string) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}
