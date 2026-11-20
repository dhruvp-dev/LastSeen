package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const backupPrefix = "lastseen-"

// Backup writes a consistent snapshot of the live database to dir using VACUUM INTO (safe while
// the web server and worker are running), then keeps only the newest `keep` snapshots.
// It returns the path of the new snapshot.
func (db *DB) Backup(ctx context.Context, dir string, keep int, now time.Time) (string, error) {
	if db.remote {
		return "", fmt.Errorf("snapshot not supported for remote Turso/libSQL database: backups are managed via Turso")
	}
	if keep < 1 {
		return "", fmt.Errorf("keep must be at least 1")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, backupPrefix+now.UTC().Format("20060102-150405")+".db")
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("backup %s already exists", dest)
	}
	tmp := dest + ".partial"
	_ = os.Remove(tmp)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("snapshot: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil { // never leave a half-written file with a final name
		return "", err
	}
	return dest, pruneBackups(dir, keep)
}

func pruneBackups(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, backupPrefix) && strings.HasSuffix(n, ".db") {
			names = append(names, n)
		}
	}
	sort.Strings(names) // timestamped names sort chronologically
	for len(names) > keep {
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}
