package profile

import (
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
