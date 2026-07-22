package tools

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// backupTimeFormat is filesystem-safe and lexically sortable, so a plain
// filename sort orders backups oldest-to-newest.
const backupTimeFormat = "2006-01-02T150405Z"

// Backup writes a consistent snapshot of the database at dbPath into dir and
// prunes the directory to the newest keep backups.
//
// It uses SQLite's VACUUM INTO rather than copying the file. A live database
// runs in WAL mode, where recent writes live in the -wal sidecar, not the main
// file — so `cp db.sqlite` captures a stale, partial database. VACUUM INTO
// produces a single fully-checkpointed file safely while the agent is still
// writing, which is the whole point of an online backup.
//
// It opens its own read connection: it must not share the agent's single
// writer, and it never writes to the live database.
func Backup(dbPath, dir string, keep int) (string, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return "", fmt.Errorf("database %s: %w", dbPath, err)
	}
	if keep < 1 {
		keep = 1
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}

	// Read-only handle. busy_timeout lets VACUUM INTO wait briefly for the
	// agent's writer rather than failing on a transient lock.
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(10000)&mode=ro")
	if err != nil {
		return "", fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	dest := filepath.Join(dir, "db-"+time.Now().UTC().Format(backupTimeFormat)+".sqlite")

	// VACUUM INTO refuses to overwrite, so a same-second re-run must not clobber
	// — extremely unlikely with a UTC-second stamp, but fail loudly if it does
	// rather than silently skipping the backup.
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("backup %s already exists", dest)
	}

	// The destination path is interpolated into SQL, so reject a quote rather
	// than build a malformed or injectable statement. Backup dirs are operator-
	// controlled, but this keeps the guarantee local.
	if strings.Contains(dest, "'") {
		return "", fmt.Errorf("backup path must not contain a quote: %s", dest)
	}
	if _, err := db.Exec("VACUUM INTO '" + dest + "'"); err != nil {
		return "", fmt.Errorf("vacuum into %s: %w", dest, err)
	}

	if err := pruneBackups(dir, keep); err != nil {
		// The backup succeeded; a prune failure is worth reporting but must not
		// present the whole operation as failed.
		return dest, fmt.Errorf("backup written to %s, but prune failed: %w", dest, err)
	}
	return dest, nil
}

// ListBackups returns backup file paths in dir, newest first.
func ListBackups(dir string) ([]string, error) {
	names, err := backupNames(dir)
	if err != nil {
		return nil, err
	}
	// backupNames is oldest-first; reverse for newest-first.
	out := make([]string, 0, len(names))
	for i := len(names) - 1; i >= 0; i-- {
		out = append(out, filepath.Join(dir, names[i]))
	}
	return out, nil
}

// pruneBackups keeps the newest keep backups and removes the rest.
func pruneBackups(dir string, keep int) error {
	names, err := backupNames(dir)
	if err != nil {
		return err
	}
	if len(names) <= keep {
		return nil
	}
	// names is oldest-first; remove all but the last keep.
	for _, name := range names[:len(names)-keep] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// backupNames returns backup filenames sorted oldest-first. The timestamp
// format sorts lexically, so a name sort is a chronological sort.
func backupNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		// Match only our own backups so a stray file in the dir is never
		// pruned or listed as a backup.
		if !e.IsDir() && strings.HasPrefix(name, "db-") && strings.HasSuffix(name, ".sqlite") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// Restore replaces the database at dbPath with a backup file.
//
// The caller MUST stop the agent first: replacing a database under a running
// writer corrupts both. Restore verifies the source is a readable SQLite
// database before touching the target, and removes stale -wal/-shm sidecars so
// the restored file is not shadowed by leftover WAL from the old database.
func Restore(backupPath, dbPath string) error {
	if _, err := os.Stat(backupPath); err != nil {
		return fmt.Errorf("backup %s: %w", backupPath, err)
	}

	// Verify the backup opens and passes an integrity check before we overwrite
	// anything — restoring a corrupt file over a good database is the one
	// unrecoverable mistake here.
	src, err := sql.Open("sqlite", backupPath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open backup: %w", err)
	}
	var result string
	if err := src.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		src.Close()
		return fmt.Errorf("backup integrity check: %w", err)
	}
	src.Close()
	if result != "ok" {
		return fmt.Errorf("backup failed integrity check: %s", result)
	}

	// Copy to a temp file beside the target, then rename — an interrupted
	// restore leaves the original database intact rather than half-written.
	tmp := dbPath + ".restore-tmp"
	if err := copyFile(backupPath, tmp); err != nil {
		return fmt.Errorf("stage restore: %w", err)
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace database: %w", err)
	}

	// The restored file is a fresh checkpointed database; any -wal/-shm left
	// from the old one would shadow it with stale pages.
	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale sidecar %s: %w", sidecar, err)
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, in, 0o600)
}
