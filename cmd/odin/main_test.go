package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiagnosticProviderSelectionIgnoresOtherCredentials(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "profiles", "default")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := `
toolsets = ["file"]

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
