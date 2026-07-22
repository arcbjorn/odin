package tools

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// seedDB writes a WAL-mode database with a known row so a backup can be checked
// for that row's presence.
func seedDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE notes(id INTEGER PRIMARY KEY, body TEXT);
		INSERT INTO notes(body) VALUES ('first'), ('second');`); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func countRows(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM notes`).Scan(&n); err != nil {
		t.Fatalf("count in %s: %v", path, err)
	}
	return n
}

func TestBackupProducesConsistentSnapshot(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sqlite")
	seedDB(t, dbPath)

	dest, err := Backup(dbPath, filepath.Join(dir, "backups"), 7)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !strings.HasSuffix(dest, ".sqlite") {
		t.Fatalf("unexpected backup name %s", dest)
	}
	// The backup must contain the seeded rows — proving VACUUM INTO captured
	// the data, not an empty file.
	if got := countRows(t, dest); got != 2 {
		t.Fatalf("backup has %d rows, want 2", got)
	}
}

// The critical WAL case: rows written but not yet checkpointed live in the -wal
// sidecar. A naive file copy would miss them; VACUUM INTO must capture them.
func TestBackupCapturesUncheckpointedWALWrites(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sqlite")

	// Hold a live WAL connection and write without checkpointing.
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1) // mirror the agent's single-writer model
	if _, err := db.Exec(`CREATE TABLE notes(id INTEGER PRIMARY KEY, body TEXT);`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO notes(body) VALUES ('live-write')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// A -wal sidecar should now hold uncheckpointed data. Back up while the
	// connection is still open.
	dest, err := Backup(dbPath, filepath.Join(dir, "backups"), 7)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	db.Close()

	if got := countRows(t, dest); got != 1 {
		t.Fatalf("backup missed the uncheckpointed write: %d rows, want 1", got)
	}
}

func TestBackupPrunesToKeep(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sqlite")
	seedDB(t, dbPath)
	backupDir := filepath.Join(dir, "backups")

	// Create several backups with distinct, ordered names by hand, then prune.
	for _, ts := range []string{"2026-07-01T000000Z", "2026-07-02T000000Z", "2026-07-03T000000Z", "2026-07-04T000000Z"} {
		os.MkdirAll(backupDir, 0o700)
		if err := copyFile(dbPath, filepath.Join(backupDir, "db-"+ts+".sqlite")); err != nil {
			t.Fatalf("seed backup: %v", err)
		}
	}

	if err := pruneBackups(backupDir, 2); err != nil {
		t.Fatalf("prune: %v", err)
	}
	remaining, err := ListBackups(backupDir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("expected 2 backups after prune, got %d", len(remaining))
	}
	// Newest kept, oldest removed.
	if !strings.Contains(remaining[0], "2026-07-04") || !strings.Contains(remaining[1], "2026-07-03") {
		t.Fatalf("prune kept the wrong backups: %v", remaining)
	}
}

func TestListBackupsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	for _, ts := range []string{"2026-07-01T000000Z", "2026-07-03T000000Z", "2026-07-02T000000Z"} {
		if err := os.WriteFile(filepath.Join(dir, "db-"+ts+".sqlite"), []byte("x"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// A non-backup file must be ignored.
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)

	got, err := ListBackups(dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 backups, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "2026-07-03") {
		t.Fatalf("not newest-first: %v", got)
	}
}

func TestBackupRefusesMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	if _, err := Backup(filepath.Join(dir, "nope.sqlite"), dir, 7); err == nil {
		t.Fatal("expected an error for a missing database")
	}
}

func TestRestoreReplacesDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sqlite")
	seedDB(t, dbPath)

	dest, err := Backup(dbPath, filepath.Join(dir, "backups"), 7)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Corrupt the live database (delete a row), then restore.
	db, _ := sql.Open("sqlite", dbPath)
	db.Exec(`DELETE FROM notes`)
	db.Close()
	if got := countRows(t, dbPath); got != 0 {
		t.Fatalf("setup: expected 0 rows after delete, got %d", got)
	}

	if err := Restore(dest, dbPath); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := countRows(t, dbPath); got != 2 {
		t.Fatalf("restore did not bring back the rows: %d, want 2", got)
	}
}

// Restoring a corrupt backup over a good database is the one unrecoverable
// mistake; Restore must verify integrity before touching the target.
func TestRestoreRejectsCorruptBackup(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sqlite")
	seedDB(t, dbPath)

	corrupt := filepath.Join(dir, "db-bad.sqlite")
	if err := os.WriteFile(corrupt, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := Restore(corrupt, dbPath); err == nil {
		t.Fatal("expected restore of a corrupt backup to fail")
	}
	// The original database must be untouched.
	if got := countRows(t, dbPath); got != 2 {
		t.Fatalf("a failed restore damaged the live database: %d rows", got)
	}
}

// A restored database must not be shadowed by stale WAL from the old one.
func TestRestoreRemovesStaleSidecars(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sqlite")
	seedDB(t, dbPath)

	dest, err := Backup(dbPath, filepath.Join(dir, "backups"), 7)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Leave a stale -wal sidecar in place.
	if err := os.WriteFile(dbPath+"-wal", []byte("stale wal pages"), 0o600); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}

	if err := Restore(dest, dbPath); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(dbPath + "-wal"); !os.IsNotExist(err) {
		t.Fatal("stale -wal sidecar was not removed")
	}
	if got := countRows(t, dbPath); got != 2 {
		t.Fatalf("restored db unreadable: %d rows", got)
	}
}
