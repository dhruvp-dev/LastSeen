// Command lastseen is the Last Seen binary: `serve` runs the web app, `worker` the monitoring worker.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dhruvp/lastseen/internal/auth"
	"github.com/dhruvp/lastseen/internal/config"
	"github.com/dhruvp/lastseen/internal/database"
	"github.com/dhruvp/lastseen/internal/monitor"
	"github.com/dhruvp/lastseen/internal/notifications"
	"github.com/dhruvp/lastseen/internal/scheduler"
	"github.com/dhruvp/lastseen/internal/web"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: lastseen serve|worker|migrate|backup [dir]|hash-password")
		os.Exit(2)
	}
	if os.Args[1] == "hash-password" {
		hashPassword()
		return
	}
	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "serve":
		if err := auth.ValidateHash(cfg.AdminPasswordHash); err != nil {
			log.Error("config", "err", err)
			os.Exit(1)
		}
		if cfg.InsecureCookies {
			log.Warn("LASTSEEN_DEV_INSECURE_COOKIES=1: session cookies are not marked Secure (development only)")
		}
		db := mustOpen(cfg, log)
		defer db.Close()
		srv, err := web.New(cfg, db, log, notifications.NewSMTPSender(cfg.SMTP))
		if err != nil {
			log.Error("web init", "err", err)
			os.Exit(1)
		}
		if err := srv.ListenAndServe(ctx); err != nil {
			log.Error("serve", "err", err)
			os.Exit(1)
		}
	case "worker":
		db := mustOpen(cfg, log)
		defer db.Close()
		guard := monitor.Guard(monitor.DefaultGuard)
		if cfg.AllowPrivate {
			log.Warn("LASTSEEN_DEV_ALLOW_PRIVATE=1: SSRF guard disabled (development only)")
			guard = monitor.AllowAll
		}
		if !cfg.SMTP.Configured() {
			log.Warn("SMTP not configured: down/recovered emails will queue and retry until LASTSEEN_SMTP_* is set")
		}
		go notifications.NewProcessor(db, notifications.NewSMTPSender(cfg.SMTP), log, cfg.BaseURL).Run(ctx)
		sch := scheduler.New(db, monitor.NewChecker(guard), log, cfg.CheckInterval)
		if err := sch.Run(ctx); err != nil {
			log.Error("worker", "err", err)
			os.Exit(1)
		}
	case "backup":
		db := mustOpen(cfg, log)
		defer db.Close()
		dir := filepath.Join(filepath.Dir(cfg.DBPath), "backups")
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		path, err := db.Backup(ctx, dir, cfg.BackupKeep, time.Now())
		if err != nil {
			log.Error("backup", "err", err)
			os.Exit(1)
		}
		log.Info("backup written", "path", path, "keep", cfg.BackupKeep)
	case "migrate":
		mustOpen(cfg, log).Close()
		log.Info("migrations applied")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func mustOpen(cfg *config.Config, log *slog.Logger) *database.DB {
	db, err := database.Open(cfg.DBPath)
	if err != nil {
		log.Error("open database", "err", err)
		os.Exit(1)
	}
	return db
}

// hashPassword reads a password (hidden when stdin is a terminal) and prints its bcrypt hash.
func hashPassword() {
	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		interactive = true
	}
	read := func(prompt string) string {
		if interactive {
			fmt.Fprint(os.Stderr, prompt)
			stty("-echo")
			defer func() { stty("echo"); fmt.Fprintln(os.Stderr) }()
		}
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimRight(line, "\r\n")
	}
	pw := read("New admin password (min 12 chars): ")
	if interactive && read("Repeat password: ") != pw {
		fmt.Fprintln(os.Stderr, "passwords do not match")
		os.Exit(1)
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(h)
}

func stty(arg string) {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin
	_ = cmd.Run()
}
