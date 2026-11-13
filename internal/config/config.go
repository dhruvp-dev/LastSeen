// Package config loads application configuration from LASTSEEN_* environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
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
	c := &Config{
		Addr:              get("LASTSEEN_ADDR", "127.0.0.1:8080"),
		DBPath:            get("LASTSEEN_DB", "./data/lastseen.db"),
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
