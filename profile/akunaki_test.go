package profile

import (
	"strings"
	"testing"
)

func TestParseAkunakiSection(t *testing.T) {
	cfg, err := parseConfig(`
toolsets = ["db", "akunaki"]
timezone = "UTC"

[akunaki]
base_url = "https://akunaki.example.com"
token_env = "AKUNAKI_SERVICE_TOKEN"
`)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Akunaki.BaseURL != "https://akunaki.example.com" {
		t.Errorf("BaseURL = %q", cfg.Akunaki.BaseURL)
	}
	if cfg.Akunaki.TokenEnv != "AKUNAKI_SERVICE_TOKEN" {
		t.Errorf("TokenEnv = %q", cfg.Akunaki.TokenEnv)
	}
}

func TestParseAkunakiRejectsUnknownKey(t *testing.T) {
	_, err := parseConfig(`
[akunaki]
token = "aksvc_never_in_config"
`)
	if err == nil || !strings.Contains(err.Error(), "[akunaki]") {
		t.Fatalf("expected an unknown-key error, got %v", err)
	}
}

// The toolset fails at build, not at the first scheduled job: no token env
// name, or a named env var that is unset, refuses the profile outright.
func TestBuildAkunakiFailsFast(t *testing.T) {
	p := &Profile{Config: Config{Akunaki: AkunakiConfig{
		BaseURL: "https://akunaki.example.com",
	}}}
	if _, err := buildAkunaki(p); err == nil {
		t.Error("expected an error with no token_env")
	}

	p.Config.Akunaki.TokenEnv = "AKUNAKI_TEST_TOKEN_UNSET"
	if _, err := buildAkunaki(p); err == nil {
		t.Error("expected an error with the env var unset")
	}

	t.Setenv("AKUNAKI_TEST_TOKEN_UNSET", "aksvc_x")
	a, err := buildAkunaki(p)
	if err != nil {
		t.Fatalf("buildAkunaki: %v", err)
	}
	if got := len(a.Tools()); got != 2 {
		t.Errorf("expected 2 tools, got %d", got)
	}
}
