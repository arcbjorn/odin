package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/odin/profile"
	"github.com/arcbjorn/odin/sched"
	"github.com/arcbjorn/odin/tools"
)

func TestDiagnosticProviderSelectionIgnoresOtherCredentials(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "profiles", "default")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := `
toolsets = ["file"]
timezone = "UTC"

[[providers]]
kind = "openai"
name = "first"
model = "first-model"
base_url = "https://first.test/v1"
api_key_env = "MISSING_FIRST_KEY"

[[providers]]
kind = "openai"
name = "second"
model = "second-model"
base_url = "https://second.test/v1"
api_key_env = "SECOND_KEY"
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("# Test"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SECOND_KEY", "second-key")

	common := &commonFlags{root: root, profile: "default"}
	providers, cleanup, err := common.loadDiagnosticProviders("second")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(providers) != 1 || providers[0].Name() != "second/second-model" {
		t.Fatalf("providers = %v", providers)
	}
}

func TestSchedulerDoesNotRequireDatabaseToolset(t *testing.T) {
	dir := t.TempDir()
	jobsDir := filepath.Join(dir, "jobs")
	if err := os.MkdirAll(jobsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobsDir, "jobs.toml"), []byte(`[[job]]
name = "Ping"
schedule = "0 7 * * *"
prompt = "ping.md"
enabled = true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobsDir, "ping.md"), []byte("Report."), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := &profile.Runtime{
		Profile:  &profile.Profile{Dir: dir},
		Location: time.UTC,
	}
	scheduler, err := buildScheduler(rt, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if scheduler == nil {
		t.Fatal("scheduler was not built without a database toolset")
	}
}

func TestBuildJobPromptLoadsDeclaredSkills(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "database-guide.md"), []byte("# Guide\n\nUse real rows."), 0o600); err != nil {
		t.Fatal(err)
	}
	skills, err := tools.NewSkills(dir)
	if err != nil {
		t.Fatal(err)
	}

	prompt, err := buildJobPrompt(&profile.Runtime{Skills: skills}, sched.Job{
		Name: "Brief", Prompt: "Write the brief.", Skills: []string{"database-guide"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Use real rows.") || !strings.Contains(prompt, "Write the brief.") {
		t.Fatalf("prompt did not contain skill and job:\n%s", prompt)
	}
}

func TestBuildJobPromptRejectsUnavailableSkillToolset(t *testing.T) {
	_, err := buildJobPrompt(&profile.Runtime{}, sched.Job{
		Name: "Brief", Prompt: "Write the brief.", Skills: []string{"database-guide"},
	})
	if err == nil || !strings.Contains(err.Error(), "toolset is disabled") {
		t.Fatalf("expected disabled toolset error, got %v", err)
	}
}
