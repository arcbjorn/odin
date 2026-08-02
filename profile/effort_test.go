package profile

import (
	"strings"
	"testing"
)

const effortConfig = `
toolsets = ["skills"]
timezone = "UTC"

[agent]
effort = "high"

[[providers]]
kind = "openai"
name = "primary"
model = "gpt-5.6-terra"
base_url = "https://example.invalid/v1"
api_key_env = "PRIMARY_KEY"
effort = "low"

[[providers]]
kind = "openai"
name = "backup"
model = "glm-5.2"
base_url = "https://example.invalid/v1"
api_key_env = "BACKUP_KEY"
`

func TestProviderEffortParses(t *testing.T) {
	root := writeProfile(t, "default", effortConfig, "# Test agent", false, true)
	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Config.Effort != "high" {
		t.Fatalf("profile effort = %q", p.Config.Effort)
	}
	if got := p.Config.Providers[0].Effort; got != "low" {
		t.Fatalf("primary effort = %q, want the per-provider override", got)
	}
	// An absent per-provider level defers to the profile default rather than
	// silently meaning "none".
	if got := p.Config.Providers[1].Effort; got != "" {
		t.Fatalf("backup effort = %q, want empty", got)
	}
}

func TestProviderEffortRejectsUnknownLevel(t *testing.T) {
	config := strings.Replace(effortConfig, `effort = "low"`, `effort = "ultra"`, 1)
	root := writeProfile(t, "default", config, "# Test agent", false, true)

	_, err := Load(root, "default")
	if err == nil {
		t.Fatal("an unsupported effort level must fail at load, not at 07:00")
	}
	if !strings.Contains(err.Error(), "effort") {
		t.Fatalf("error should name the offending key: %v", err)
	}
}

// Asking for a level and suppressing the field are contradictory; a config
// that says both is a mistake worth reporting rather than silently resolving.
func TestProviderEffortAndDropEffortConflict(t *testing.T) {
	config := strings.Replace(effortConfig, `effort = "low"`, "effort = \"low\"\ndrop_effort = true", 1)
	root := writeProfile(t, "default", config, "# Test agent", false, true)

	_, err := Load(root, "default")
	if err == nil {
		t.Fatal("effort together with drop_effort must fail")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error should explain the conflict: %v", err)
	}
}
