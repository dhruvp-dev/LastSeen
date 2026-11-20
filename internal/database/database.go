// Package database owns SQLite access: connection setup, migrations and queries.
package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/tursodatabase/libsql-client-go/libsql"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type DB struct {
	*sql.DB
	remote bool
}

// IsRemote reports whether the DB connects to a remote endpoint (e.g. Turso libSQL).
func (db *DB) IsRemote() bool {
	return db.remote
}

func isRemote(path string) bool {
	return strings.HasPrefix(path, "libsql://") ||
		strings.HasPrefix(path, "https://") ||
		strings.HasPrefix(path, "http://") ||
		strings.HasPrefix(path, "wss://") ||
		strings.HasPrefix(path, "ws://")
}

// Open opens the SQLite file (or remote Turso libSQL URL) with appropriate connection settings and migrations.
func Open(path string) (*DB, error) {
	var (
		sdb    *sql.DB
		err    error
		remote = isRemote(path)
	)

	if remote {
		sdb, err = sql.Open("libsql", path)
		if err != nil {
			return nil, err
		}
		sdb.SetMaxOpenConns(10)
		sdb.SetConnMaxLifetime(time.Hour)
	} else {
		if path != ":memory:" {
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return nil, err
			}
		}
		dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
			"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
		sdb, err = sql.Open("sqlite", dsn)
		if err != nil {
			return nil, err
		}
		// SQLite serialises writers; a small pool avoids lock contention on a 1 vCPU box.
		sdb.SetMaxOpenConns(4)
		sdb.SetConnMaxLifetime(0)
	}

	if err := sdb.Ping(); err != nil {
		sdb.Close()
		return nil, err
	}
	db := &DB{DB: sdb, remote: remote}
	if err := db.Migrate(context.Background()); err != nil {
		sdb.Close()
		return nil, err
	}
	return db, nil
}

// Migrate applies unapplied migrations in order, each in its own transaction.
func (db *DB) Migrate(ctx context.Context) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if err := db.applyMigration(ctx, name, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

func (db *DB) applyMigration(ctx context.Context, name, body string) error {
	tx, err := db.BeginTx(ctx, nil) // immediate lock: concurrent processes serialise here
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	for _, stmt := range splitSQLStatements(body) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("execute statement %q: %w", stmt, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?,?)`, name, Now()); err != nil {
		return err
	}
	return tx.Commit()
}

// splitSQLStatements splits a SQL script into individual statements by semicolon,
// taking into account comments (-- ...) and string literals ('...').
func splitSQLStatements(script string) []string {
	var statements []string
	var current strings.Builder
	inString := false
	inComment := false

	chars := []rune(script)
	for i := 0; i < len(chars); i++ {
		c := chars[i]
		if inComment {
			if c == '\n' {
				inComment = false
			}
			continue
		}

		if inString {
			current.WriteRune(c)
			if c == '\'' {
				// Handle escaped quote: ''
				if i+1 < len(chars) && chars[i+1] == '\'' {
					current.WriteRune('\'')
					i++
				} else {
					inString = false
				}
			}
			continue
		}

		if c == '-' && i+1 < len(chars) && chars[i+1] == '-' {
			inComment = true
			i++
			continue
		}

		if c == '\'' {
			inString = true
			current.WriteRune(c)
			continue
		}

		if c == ';' {
			stmt := strings.TrimSpace(current.String())
			if stmt != "" {
				statements = append(statements, stmt)
			}
			current.Reset()
			continue
		}

		current.WriteRune(c)
	}

	if stmt := strings.TrimSpace(current.String()); stmt != "" {
		statements = append(statements, stmt)
	}

	return statements
}

// Now returns the current time as unix milliseconds.
func Now() int64 { return time.Now().UnixMilli() }

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
