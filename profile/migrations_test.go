package profile

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrationsApplyOnceInOrder(t *testing.T) {
	dir := t.TempDir()
	writeMigration(t, dir, "001-create-items.sql", `CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT);`)
	writeMigration(t, dir, "002-seed-item.sql", `INSERT INTO items(name) VALUES ('first');`)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if count, err := ApplyMigrations(context.Background(), db, dir); err != nil || count != 2 {
		t.Fatalf("first apply count=%d err=%v", count, err)
	}
	if count, err := ApplyMigrations(context.Background(), db, dir); err != nil || count != 0 {
		t.Fatalf("second apply count=%d err=%v", count, err)
	}
	var items int
	if err := db.QueryRow(`SELECT count(*) FROM items`).Scan(&items); err != nil || items != 1 {
		t.Fatalf("items=%d err=%v", items, err)
	}
}

func TestMigrationFailureRollsBackAndChecksumIsImmutable(t *testing.T) {
	dir := t.TempDir()
	path := writeMigration(t, dir, "001-create-items.sql", `CREATE TABLE items (id INTEGER PRIMARY KEY);`)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := ApplyMigrations(context.Background(), db, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`CREATE TABLE changed (id INTEGER);`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyMigrations(context.Background(), db, dir); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("expected checksum error, got %v", err)
	}

	if err := os.WriteFile(path, []byte(`CREATE TABLE items (id INTEGER PRIMARY KEY);`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeMigration(t, dir, "002-broken.sql", `CREATE TABLE partial (id INTEGER); invalid sql;`)
	if _, err := ApplyMigrations(context.Background(), db, dir); err == nil {
		t.Fatal("expected broken migration to fail")
	}
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='partial'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("failed migration was not rolled back")
	}
}

func writeMigration(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
