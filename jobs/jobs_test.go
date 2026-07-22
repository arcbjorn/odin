package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeJobs(t *testing.T, manifest string, prompts map[string]string) string {
	t.Helper()
	profileDir := t.TempDir()
	dir := filepath.Join(profileDir, "jobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, "jobs.toml"), []byte(manifest), 0o600); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
	for name, body := range prompts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write prompt %s: %v", name, err)
		}
	}
	return profileDir
}

const twoJobs = `
[[job]]
name = "Daily report"
schedule = "0 7 * * *"
prompt = "daily-report.md"
skills = ["database-guide"]
enabled = true

[[job]]
name = "Hourly check"
schedule = "30 22 * * *"
prompt = "hourly-check.md"
enabled = false
`

func TestLoadJobs(t *testing.T) {
	dir := writeJobs(t, twoJobs, map[string]string{
		"daily-report.md": "You are a general assistant. Ground every line in a row you read.",
		"hourly-check.md":   "Check whether today's review exists.",
	})

	set, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(set.Jobs))
	}

	// Sorted by name for stable output.
	if set.Jobs[0].Name != "Daily report" {
		t.Fatalf("jobs not sorted: %v", set.Names())
	}
	if !strings.Contains(set.Jobs[0].Prompt, "Ground every line") {
		t.Fatalf("prompt not loaded: %q", set.Jobs[0].Prompt)
	}
	if !set.Jobs[0].Enabled || set.Jobs[1].Enabled {
		t.Fatal("enabled flags not applied")
	}
	if len(set.Jobs[0].Skills) != 1 || set.Jobs[0].Skills[0] != "database-guide" {
		t.Fatalf("skills = %v", set.Jobs[0].Skills)
	}
}

// A job someone bothered to declare should run unless switched off.
func TestEnabledDefaultsTrue(t *testing.T) {
	manifest := `
[[job]]
name = "Daily report"
schedule = "0 7 * * *"
prompt = "daily-report.md"
`
	dir := writeJobs(t, manifest, map[string]string{"daily-report.md": "brief"})

	set, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !set.Jobs[0].Enabled {
		t.Fatal("enabled should default to true")
	}
}

// Omitting prompt derives the filename from the job name.
func TestPromptFilenameDerivedFromName(t *testing.T) {
	manifest := `
[[job]]
name = "Weekly rollup"
schedule = "0 18 * * 0"
`
	dir := writeJobs(t, manifest, map[string]string{"weekly-rollup.md": "debrief"})

	set, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if set.Jobs[0].Prompt != "debrief" {
		t.Fatalf("prompt = %q", set.Jobs[0].Prompt)
	}
}

// A missing prompt file must fail at load, not at 07:00 with nobody watching.
func TestMissingPromptIsFatal(t *testing.T) {
	dir := writeJobs(t, twoJobs, map[string]string{"daily-report.md": "brief"})

	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected a missing prompt file to fail the load")
	}
	if !strings.Contains(err.Error(), "hourly-check.md") {
		t.Fatalf("error should name the missing file, got: %v", err)
	}
}

// An empty prompt would send the model nothing and produce a confusing reply.
func TestEmptyPromptIsFatal(t *testing.T) {
	dir := writeJobs(t, twoJobs, map[string]string{
		"daily-report.md": "brief",
		"hourly-check.md":   "   \n\n",
	})
	if _, err := Load(dir); err == nil {
		t.Fatal("expected an empty prompt to fail the load")
	}
}

// A bad cron expression must be caught at load, not silently never fire.
func TestInvalidScheduleIsFatal(t *testing.T) {
	manifest := `
[[job]]
name = "Broken"
schedule = "99 7 * * *"
prompt = "broken.md"
`
	dir := writeJobs(t, manifest, map[string]string{"broken.md": "x"})

	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected an invalid schedule to fail the load")
	}
	if !strings.Contains(err.Error(), "Broken") {
		t.Fatalf("error should name the job, got: %v", err)
	}
}

func TestDuplicateJobNameIsFatal(t *testing.T) {
	manifest := `
[[job]]
name = "Daily report"
schedule = "0 7 * * *"
prompt = "a.md"

[[job]]
name = "Daily report"
schedule = "0 8 * * *"
prompt = "b.md"
`
	dir := writeJobs(t, manifest, map[string]string{"a.md": "x", "b.md": "y"})
	if _, err := Load(dir); err == nil {
		t.Fatal("expected duplicate job names to be refused")
	}
}

func TestMissingManifestReportsNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir)
	if !os.IsNotExist(err) {
		t.Fatalf("expected os.ErrNotExist so callers can treat no-jobs as valid, got %v", err)
	}
}

func TestFindIsCaseInsensitive(t *testing.T) {
	dir := writeJobs(t, twoJobs, map[string]string{
		"daily-report.md": "brief",
		"hourly-check.md":   "guard",
	})
	set, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := set.Find("daily report"); !ok {
		t.Fatal("Find should be case-insensitive")
	}
	if _, ok := set.Find("nonexistent"); ok {
		t.Fatal("Find matched a job that does not exist")
	}
}

func TestManifestSyntaxErrors(t *testing.T) {
	cases := map[string]string{
		"unknown table":     "[[cronjob]]\nname = \"x\"\n",
		"key outside block": "name = \"x\"\n",
		"unknown key":       "[[job]]\nname = \"x\"\nschedule = \"0 7 * * *\"\nfrequency = \"daily\"\n",
		"unquoted value":    "[[job]]\nname = x\n",
		"no schedule":       "[[job]]\nname = \"x\"\n",
	}
	for label, manifest := range cases {
		dir := writeJobs(t, manifest, map[string]string{"x.md": "x"})
		if _, err := Load(dir); err == nil {
			t.Errorf("%s: expected a parse error", label)
		}
	}
}
