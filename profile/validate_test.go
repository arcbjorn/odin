package profile

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateChecksJobsAndSkillsOffline(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant", true, true)
	dir := filepath.Join(root, "profiles", "default")
	jobsDir := filepath.Join(dir, "jobs")
	if err := os.MkdirAll(jobsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `[[job]]
name = "Brief"
schedule = "0 7 * * *"
prompt = "brief.md"
skills = ["database-guide"]
enabled = true
`
	if err := os.WriteFile(filepath.Join(jobsDir, "jobs.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobsDir, "brief.md"), []byte("Write a brief."), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Validate(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	if report.Jobs != 1 || report.Skills != 1 || report.Timezone != "America/New_York" {
		t.Fatalf("report = %+v", report)
	}

	if err := os.Remove(filepath.Join(dir, "skills", "database-guide.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(root, "default"); err == nil || !strings.Contains(err.Error(), "database-guide") {
		t.Fatalf("expected missing skill error, got %v", err)
	}
}

// A profile directory the service user cannot write makes SQLite refuse to
// open its own WAL database even read-only, reporting "attempt to write a
// readonly database". That error points at the database and the real fault is
// a chmod — it cost a live deploy several wrong turns. Validation must name
// the directory.
func TestValidateNamesAnUnwritableProfileDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := writeProfile(t, "default", minimalConfig, "# General assistant", true, true)
	dir := filepath.Join(root, "profiles", "default")

	// Put the database in WAL mode: a rollback-journal database opens fine
	// from an unwritable directory, so the mode is what makes this fail.
	db, err := sql.Open("sqlite", filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO records (value) VALUES ('x')"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err = Validate(root, "default")
	if err == nil {
		t.Fatal("validation should fail when the profile directory is unwritable")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("error should name the unwritable directory, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("error should say the directory is not writable, got: %v", err)
	}
}

// An ordinary validation must not carry the permission hint, or the advice
// becomes noise that points at the wrong thing.
func TestValidateOnAHealthyProfileHasNoPermissionHint(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant", true, true)

	report, err := Validate(root, "default")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if report.Profile.Name != "default" {
		t.Fatalf("profile = %q", report.Profile.Name)
	}
	// And the probe left nothing behind.
	entries, err := os.ReadDir(filepath.Join(root, "profiles", "default"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".odin-write-probe") {
			t.Fatalf("write probe was not cleaned up: %s", e.Name())
		}
	}
}
