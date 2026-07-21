package profile

import (
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// The whole point of the scaffold: `odin init` must produce something
// `odin status` accepts immediately. A scaffold needing undocumented edits
// before it works is a README pretending to be a command.
func TestScaffoldLoadsWithoutEdits(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENAI_API_KEY", "test-key")

	dir, err := Scaffold(ScaffoldOptions{Root: root, Name: "default", Timezone: "UTC"})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if !strings.HasSuffix(dir, filepath.Join("profiles", "default")) {
		t.Fatalf("unexpected dir %s", dir)
	}

	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("a fresh scaffold must load: %v", err)
	}

	rt, err := Build(p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("a fresh scaffold must build: %v", err)
	}
	defer rt.Close()

	if rt.Track.Location().String() != "UTC" {
		t.Fatalf("timezone = %s", rt.Track.Location())
	}
	for _, name := range []string{"query", "exec", "read_file"} {
		if _, ok := rt.Tools.Lookup(name); !ok {
			t.Errorf("%s not registered", name)
		}
	}
}

// Wrong zone means every late-night session is misfiled, so it is asked for
// rather than defaulted.
func TestScaffoldRequiresValidTimezone(t *testing.T) {
	root := t.TempDir()

	if _, err := Scaffold(ScaffoldOptions{Root: root, Name: "a"}); err == nil {
		t.Error("expected a missing timezone to be refused")
	}
	if _, err := Scaffold(ScaffoldOptions{Root: root, Name: "b", Timezone: "Mars/Olympus"}); err == nil {
		t.Error("expected an unknown timezone to be refused")
	}
}

// The directory holds a tracker database and OAuth tokens; clobbering either
// is unrecoverable.
func TestScaffoldNeverOverwrites(t *testing.T) {
	root := t.TempDir()

	if _, err := Scaffold(ScaffoldOptions{Root: root, Name: "default", Timezone: "UTC"}); err != nil {
		t.Fatalf("first scaffold: %v", err)
	}

	marker := filepath.Join(root, "profiles", "default", "SOUL.md")
	if err := os.WriteFile(marker, []byte("# My real persona"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := Scaffold(ScaffoldOptions{Root: root, Name: "default", Timezone: "UTC"}); err == nil {
		t.Fatal("expected a second scaffold to refuse")
	}

	body, err := os.ReadFile(marker)
	if err != nil || !strings.Contains(string(body), "My real persona") {
		t.Fatal("an existing profile was overwritten")
	}
}

func TestScaffoldRejectsPathNames(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"../escape", "a/b", `..\win`, ""} {
		if _, err := Scaffold(ScaffoldOptions{Root: root, Name: name, Timezone: "UTC"}); err == nil {
			t.Errorf("expected %q to be refused", name)
		}
	}
}

// The tracker must carry its timezone: NewSQLite refuses to start without one
// rather than defaulting to UTC.
func TestScaffoldSeedsTimezone(t *testing.T) {
	root := t.TempDir()
	dir, err := Scaffold(ScaffoldOptions{Root: root, Name: "default", Timezone: "Asia/Tokyo"})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, "tracker.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var tz string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key='timezone'`).Scan(&tz); err != nil {
		t.Fatalf("read timezone: %v", err)
	}
	if tz != "Asia/Tokyo" {
		t.Fatalf("timezone = %q", tz)
	}
}

// WAL lets the agent and inspection tools read concurrently.
func TestScaffoldEnablesWAL(t *testing.T) {
	root := t.TempDir()
	dir, err := Scaffold(ScaffoldOptions{Root: root, Name: "default", Timezone: "UTC"})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, "tracker.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

func TestScaffoldAppliesCustomSchema(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OPENAI_API_KEY", "test-key")
	schema := filepath.Join(t.TempDir(), "schema.sql")
	body := `CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT);
INSERT INTO settings(key, value) VALUES ('timezone', 'Asia/Tokyo');
CREATE TABLE records (id INTEGER PRIMARY KEY, value TEXT);`
	if err := os.WriteFile(schema, []byte(body), 0o600); err != nil {
		t.Fatalf("write schema: %v", err)
	}

	dir, err := Scaffold(ScaffoldOptions{
		Root: root, Name: "default", Timezone: "America/New_York", TrackerSchema: schema,
	})
	if err != nil {
		t.Fatalf("Scaffold with real schema: %v", err)
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, "tracker.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables != 2 {
		t.Fatalf("expected two custom tables, got %d", tables)
	}

	// The scaffold's timezone must win over whatever the schema seeds, since
	// the operator just stated it explicitly.
	var tz string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key='timezone'`).Scan(&tz); err != nil {
		t.Fatalf("read timezone: %v", err)
	}
	if tz != "America/New_York" {
		t.Fatalf("timezone = %q; the requested zone should win", tz)
	}

	// And it must still be a loadable profile.
	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := Build(p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rt.Close()
}

func TestScaffoldReportsBadSchemaPath(t *testing.T) {
	root := t.TempDir()
	_, err := Scaffold(ScaffoldOptions{
		Root: root, Name: "default", Timezone: "UTC",
		TrackerSchema: filepath.Join(t.TempDir(), "nope.sql"),
	})
	if err == nil {
		t.Fatal("expected a missing schema file to fail")
	}
	if !strings.Contains(err.Error(), "nope.sql") {
		t.Fatalf("error should name the file, got: %v", err)
	}
}

// Credentials and live state must not be committable by accident.
func TestScaffoldGitignoresSecrets(t *testing.T) {
	root := t.TempDir()
	dir, err := Scaffold(ScaffoldOptions{Root: root, Name: "default", Timezone: "UTC"})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read gitignore: %v", err)
	}
	for _, want := range []string{"auth/", "tracker.db", "*.env", "state/"} {
		if !strings.Contains(string(body), want) {
			t.Errorf(".gitignore missing %q", want)
		}
	}
}

// Secrets belong in the environment; config.toml is meant to be committed.
func TestScaffoldConfigHasNoSecrets(t *testing.T) {
	root := t.TempDir()
	dir, err := Scaffold(ScaffoldOptions{Root: root, Name: "default", Timezone: "UTC"})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "api_key_env") {
		t.Error("config should name the key's env var")
	}
	// The parser rejects these keys outright; the template must not model them.
	for _, forbidden := range []string{"api_key =", "token =", "secret ="} {
		if strings.Contains(text, forbidden) {
			t.Errorf("config template contains %q", forbidden)
		}
	}
}

// Auth and state hold refresh tokens and run history.
func TestScaffoldDirectoriesArePrivate(t *testing.T) {
	root := t.TempDir()
	dir, err := Scaffold(ScaffoldOptions{Root: root, Name: "default", Timezone: "UTC"})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	for _, sub := range []string{"auth", "state", "notes"} {
		info, err := os.Stat(filepath.Join(dir, sub))
		if err != nil {
			t.Errorf("stat %s: %v", sub, err)
			continue
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s has mode %o, want 700", sub, perm)
		}
	}
}

func TestScaffoldDoesNotInstallTemplates(t *testing.T) {
	root := t.TempDir()
	dir, err := Scaffold(ScaffoldOptions{Root: root, Name: "default", Timezone: "UTC"})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	for _, sub := range []string{"jobs", "skills"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			t.Fatalf("read %s: %v", sub, err)
		}
		if len(entries) != 0 {
			t.Errorf("%s should start empty, got %d template files", sub, len(entries))
		}
	}
}
