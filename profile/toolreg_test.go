package profile

import (
	"strings"
	"testing"
)

func TestParseToolRegTables(t *testing.T) {
	cfg, err := parseConfig(`
toolsets = ["db", "toolreg"]
timezone = "UTC"

[[toolreg]]
name = "health"
url = "https://backend.example.com/v1/tools"
token_env = "HEALTH_SERVICE_TOKEN"

[[toolreg]]
name = "finance"
url = "https://money.example.com/v1/tools"
token_env = "FINANCE_SERVICE_TOKEN"
`)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.ToolRegs) != 2 {
		t.Fatalf("expected 2 registries, got %d", len(cfg.ToolRegs))
	}
	if cfg.ToolRegs[0].Name != "health" ||
		cfg.ToolRegs[0].URL != "https://backend.example.com/v1/tools" ||
		cfg.ToolRegs[0].TokenEnv != "HEALTH_SERVICE_TOKEN" {
		t.Errorf("first registry = %+v", cfg.ToolRegs[0])
	}
	if cfg.ToolRegs[1].Name != "finance" {
		t.Errorf("second registry = %+v", cfg.ToolRegs[1])
	}
}

func TestParseToolRegRejectsUnknownKey(t *testing.T) {
	_, err := parseConfig(`
[[toolreg]]
token = "never_in_config"
`)
	if err == nil || !strings.Contains(err.Error(), "[[toolreg]]") {
		t.Fatalf("expected an unknown-key error, got %v", err)
	}
}

func TestParseToolRegKeyOutsideBlockFails(t *testing.T) {
	// A stray key with no preceding [[toolreg]] table is a config mistake,
	// not an implicit first registry.
	_, err := parseConfig("[[providers]]\nname = \"p\"\n[[toolreg]]\nname = \"a\"\n")
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// The toolset fails at build, not at the first scheduled job: no tables, a
// missing token_env, an unset env var, or a duplicate name each refuse the
// profile outright.
func TestBuildToolRegsFailsFast(t *testing.T) {
	p := &Profile{Config: Config{}}
	if _, err := buildToolRegs(p); err == nil {
		t.Error("expected an error with no [[toolreg]] tables")
	}

	p.Config.ToolRegs = []ToolRegConfig{{Name: "health", URL: "https://api.example.com/v1/tools"}}
	if _, err := buildToolRegs(p); err == nil {
		t.Error("expected an error with no token_env")
	}

	p.Config.ToolRegs[0].TokenEnv = "TOOLREG_TEST_TOKEN_UNSET"
	if _, err := buildToolRegs(p); err == nil {
		t.Error("expected an error with the env var unset")
	}

	t.Setenv("TOOLREG_TEST_TOKEN_UNSET", "tok_x")
	regs, err := buildToolRegs(p)
	if err != nil {
		t.Fatalf("buildToolRegs: %v", err)
	}
	if len(regs) != 1 || len(regs[0].Tools()) != 2 {
		t.Errorf("expected 1 registry with 2 tools, got %d", len(regs))
	}

	p.Config.ToolRegs = append(p.Config.ToolRegs, ToolRegConfig{
		Name: "health", URL: "https://other.example.com/v1/tools",
		TokenEnv: "TOOLREG_TEST_TOKEN_UNSET",
	})
	if _, err := buildToolRegs(p); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected a duplicate-name error, got %v", err)
	}
}

func TestBuildToolRegsSupportsSeveralRegistries(t *testing.T) {
	t.Setenv("TOOLREG_TEST_TOKEN_A", "tok_a")
	t.Setenv("TOOLREG_TEST_TOKEN_B", "tok_b")
	p := &Profile{Config: Config{ToolRegs: []ToolRegConfig{
		{Name: "health", URL: "https://a.example.com/v1/tools", TokenEnv: "TOOLREG_TEST_TOKEN_A"},
		{Name: "finance", URL: "https://b.example.com/v1/tools", TokenEnv: "TOOLREG_TEST_TOKEN_B"},
	}}}
	regs, err := buildToolRegs(p)
	if err != nil {
		t.Fatalf("buildToolRegs: %v", err)
	}
	names := []string{}
	for _, reg := range regs {
		for _, tool := range reg.Tools() {
			names = append(names, tool.Def.Name)
		}
	}
	want := []string{"health_tools", "health_tool", "finance_tools", "finance_tool"}
	if len(names) != len(want) {
		t.Fatalf("tool names = %v", names)
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("tool %d = %q, want %q", i, names[i], n)
		}
	}
}
