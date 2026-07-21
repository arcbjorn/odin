package profile

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
	"github.com/arcbjorn/odin/model"
)

const minimalConfig = `
toolsets = ["db", "skills"]

[agent]
max_turns = 20
effort = "high"

[[providers]]
kind = "openai"
name = "opencode-go"
model = "glm-5.2"
base_url = "https://opencode.ai/zen/go/v1"
api_key_env = "OPENCODE_GO_API_KEY"
`

// writeProfile lays out a profile directory on disk.
func writeProfile(t *testing.T, name, config, soul string, withDB, withSkills bool) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "profiles", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	if soul != "" {
		if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte(soul), 0o600); err != nil {
			t.Fatalf("write soul: %v", err)
		}
	}
	if withSkills {
		skills := filepath.Join(dir, "skills")
		if err := os.MkdirAll(skills, 0o700); err != nil {
			t.Fatalf("mkdir skills: %v", err)
		}
		// The body uses a distinctive marker so the "catalog must not leak
		// document bodies" assertion cannot pass or fail by coincidence.
		body := "# Database guide\n\nSENTINEL_SKILL_BODY_DO_NOT_INLINE"
		if err := os.WriteFile(filepath.Join(skills, "database-guide.md"), []byte(body), 0o600); err != nil {
			t.Fatalf("write skill: %v", err)
		}
	}
	if withDB {
		db, err := sql.Open("sqlite", filepath.Join(dir, "db.sqlite"))
		if err != nil {
			t.Fatalf("open database: %v", err)
		}
		defer db.Close()
		if _, err := db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT);
			INSERT INTO settings(key,value) VALUES ('timezone','America/New_York');`); err != nil {
			t.Fatalf("seed database: %v", err)
		}
	}
	return root
}

// Gotcha #9: a name that does not resolve must be a hard error, never a
// silent attach to a different or empty profile.
func TestMissingProfileFailsLoudly(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant", true, true)

	_, err := Load(root, "personl") // typo
	if err == nil {
		t.Fatal("a missing profile must not fall back to a default")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error should say the profile is missing, got: %v", err)
	}
	// The operator should not have to read source to find the right name.
	if !strings.Contains(err.Error(), "default") {
		t.Fatalf("error should list available profiles, got: %v", err)
	}
}

func TestProfileNameRejectsPaths(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant", true, true)
	for _, name := range []string{"../secrets", "default/../../etc", `..\win`, ""} {
		if _, err := Load(root, name); err == nil {
			t.Errorf("expected %q to be refused", name)
		}
	}
}

func TestLoadValidProfile(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant\n\nBe helpful.", true, true)

	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Name != "default" {
		t.Fatalf("name = %q", p.Name)
	}
	if !strings.Contains(p.Soul, "Be helpful.") {
		t.Fatalf("soul not loaded: %q", p.Soul)
	}
	if p.Config.MaxTurns != 20 || p.Config.Effort != "high" {
		t.Fatalf("agent config = %+v", p.Config)
	}
	if len(p.Config.Providers) != 1 || p.Config.Providers[0].Name != "opencode-go" {
		t.Fatalf("providers = %+v", p.Config.Providers)
	}
	if !p.HasToolset("db") || p.HasToolset("shell") {
		t.Fatalf("toolsets = %v", p.Config.Toolsets)
	}
}

// The persona is the agent. Without it, the same code would write to the same
// database as a generic assistant.
func TestMissingSoulIsFatal(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "", true, true)
	if _, err := Load(root, "default"); err == nil {
		t.Fatal("expected a missing SOUL.md to fail the load")
	}
}

func TestEmptySoulIsFatal(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "   \n\n", true, true)
	if _, err := Load(root, "default"); err == nil {
		t.Fatal("expected an empty SOUL.md to fail the load")
	}
}

// A typo in a toolset name must fail at load, not silently drop a capability
// that only goes missing at 07:00.
func TestUnknownToolsetIsFatal(t *testing.T) {
	cfg := strings.Replace(minimalConfig, `["db", "skills"]`, `["db", "trackr"]`, 1)
	root := writeProfile(t, "default", cfg, "# General assistant", true, true)

	_, err := Load(root, "default")
	if err == nil {
		t.Fatal("expected an unknown toolset to be rejected")
	}
	if !strings.Contains(err.Error(), "trackr") {
		t.Fatalf("error should name the bad toolset, got: %v", err)
	}
}

// Declaring the database toolset without a database is a startup error, not a
// 07:00 surprise inside an unattended cron run.
func TestDBToolsetRequiresDatabase(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant", false, true)
	_, err := Load(root, "default")
	if err == nil {
		t.Fatal("expected a missing db.sqlite to fail validation")
	}
	if !strings.Contains(err.Error(), "db") {
		t.Fatalf("error should name the database, got: %v", err)
	}
}

func TestSkillsToolsetRequiresDirectory(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant", true, false)
	if _, err := Load(root, "default"); err == nil {
		t.Fatal("expected a missing skills dir to fail validation")
	}
}

// A configured gateway must never accept arbitrary users.
func TestTelegramWithoutAllowlistIsRefused(t *testing.T) {
	cfg := minimalConfig + "\n[telegram]\ntoken_env = \"TELEGRAM_TOKEN\"\nallowed_users = []\n"
	root := writeProfile(t, "default", cfg, "# General assistant", true, true)

	_, err := Load(root, "default")
	if err == nil {
		t.Fatal("expected an empty allowlist to be refused")
	}
	if !strings.Contains(err.Error(), "allowed_users") {
		t.Fatalf("error should name allowed_users, got: %v", err)
	}
}

func TestTelegramAllowlistParses(t *testing.T) {
	cfg := minimalConfig + "\n[telegram]\ntoken_env = \"TELEGRAM_TOKEN\"\nallowed_users = [123456789]\n"
	root := writeProfile(t, "default", cfg, "# General assistant", true, true)

	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.Config.Telegram.AllowedUsers) != 1 || p.Config.Telegram.AllowedUsers[0] != 123456789 {
		t.Fatalf("allowed_users = %v", p.Config.Telegram.AllowedUsers)
	}
}

// A secret in config.toml ends up in git and in backups.
func TestInlineSecretIsRefused(t *testing.T) {
	cfg := strings.Replace(minimalConfig,
		`api_key_env = "OPENCODE_GO_API_KEY"`,
		`api_key = "sk-live-abc123"`, 1)
	root := writeProfile(t, "default", cfg, "# General assistant", true, true)

	_, err := Load(root, "default")
	if err == nil {
		t.Fatal("expected an inline api_key to be refused")
	}
	if !strings.Contains(err.Error(), "api_key_env") {
		t.Fatalf("error should point at api_key_env, got: %v", err)
	}
}

func TestProviderValidation(t *testing.T) {
	cases := map[string]string{
		"unknown kind":       strings.Replace(minimalConfig, `kind = "openai"`, `kind = "grpc"`, 1),
		"missing model":      strings.Replace(minimalConfig, "model = \"glm-5.2\"\n", "", 1),
		"missing base_url":   strings.Replace(minimalConfig, "base_url = \"https://opencode.ai/zen/go/v1\"\n", "", 1),
		"missing key source": strings.Replace(minimalConfig, "api_key_env = \"OPENCODE_GO_API_KEY\"\n", "", 1),
	}
	for name, cfg := range cases {
		root := writeProfile(t, "default", cfg, "# General assistant", true, true)
		if _, err := Load(root, "default"); err == nil {
			t.Errorf("%s: expected validation failure", name)
		}
	}
}

func TestDuplicateProviderNameIsRefused(t *testing.T) {
	cfg := minimalConfig + `
[[providers]]
kind = "openai"
name = "opencode-go"
model = "glm-5.2"
base_url = "https://opencode.ai/zen/go/v1"
api_key_env = "OPENCODE_GO_API_KEY"
`
	root := writeProfile(t, "default", cfg, "# General assistant", true, true)
	if _, err := Load(root, "default"); err == nil {
		t.Fatal("expected duplicate provider names to be refused")
	}
}

func TestOAuthProviderRequiresEndpoints(t *testing.T) {
	cfg := `
toolsets = ["db"]

[[providers]]
kind = "openai"
name = "codex"
model = "gpt-5.5"
base_url = "https://chatgpt.com/backend-api/codex"
oauth = true
`
	root := writeProfile(t, "default", cfg, "# General assistant", true, false)
	_, err := Load(root, "default")
	if err == nil {
		t.Fatal("expected oauth without client_id/token_url to be refused")
	}
	if !strings.Contains(err.Error(), "token_url") {
		t.Fatalf("error should name the missing endpoints, got: %v", err)
	}
}

func TestSubscriptionProviderParses(t *testing.T) {
	cfg := `
toolsets = ["db"]

[[providers]]
kind = "openai"
name = "codex"
model = "gpt-5.5"
base_url = "https://chatgpt.com/backend-api/codex"
subscription = "codex"
api_mode = "responses"
`
	root := writeProfile(t, "default", cfg, "# General assistant", true, false)
	p, err := Load(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	provider := p.Config.Providers[0]
	if provider.Subscription != "codex" || provider.APIMode != "responses" {
		t.Fatalf("provider = %+v", provider)
	}
}

func TestSubscriptionKindMismatchIsRefused(t *testing.T) {
	cfg := strings.Replace(minimalConfig,
		`api_key_env = "OPENCODE_GO_API_KEY"`, `subscription = "claude"`, 1)
	root := writeProfile(t, "default", cfg, "# General assistant", true, true)
	if _, err := Load(root, "default"); err == nil || !strings.Contains(err.Error(), "requires kind") {
		t.Fatalf("expected kind mismatch, got %v", err)
	}
}

func TestSubscriptionAPIModeMismatchIsRefused(t *testing.T) {
	cfg := `
toolsets = ["db"]

[[providers]]
kind = "openai"
name = "codex"
model = "gpt-5.5"
subscription = "codex"
api_mode = "chat_completions"
`
	root := writeProfile(t, "default", cfg, "# General assistant", true, false)
	if _, err := Load(root, "default"); err == nil || !strings.Contains(err.Error(), "requires api_mode") {
		t.Fatalf("expected api mode mismatch, got %v", err)
	}
}

func TestSubscriptionBaseURLDefaults(t *testing.T) {
	tests := map[string]string{
		"codex":   "https://chatgpt.com/backend-api/codex",
		"claude":  "https://api.anthropic.com/v1",
		"xai":     "https://api.x.ai/v1",
		"minimax": "https://api.minimax.io/anthropic",
		"qwen":    "https://coding-intl.dashscope.aliyuncs.com/v1",
		"kimi":    "https://api.kimi.com/coding/v1",
	}
	for subscription, want := range tests {
		if got := providerBaseURL(ProviderConfig{Subscription: subscription}); got != want {
			t.Errorf("%s base URL = %q, want %q", subscription, got, want)
		}
	}
}

func TestQwenCodingPlanUsesNativeAPIKey(t *testing.T) {
	cfg := `
toolsets = ["db"]

[[providers]]
kind = "openai"
name = "qwen"
model = "qwen3-coder-plus"
subscription = "qwen"
api_key_env = "QWEN_PLAN_KEY"
`
	root := writeProfile(t, "default", cfg, "# General assistant", true, false)
	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	t.Setenv("QWEN_PLAN_KEY", "sk-sp-test-plan-key")
	source, err := tokenSource(p, p.Config.Providers[0])
	if err != nil {
		t.Fatalf("tokenSource: %v", err)
	}
	token, err := source.Token(context.Background())
	if err != nil || token != "sk-sp-test-plan-key" {
		t.Fatalf("token=%q err=%v", token, err)
	}

	t.Setenv("QWEN_PLAN_KEY", "sk-payg-key")
	if _, err := tokenSource(p, p.Config.Providers[0]); err == nil || !strings.Contains(err.Error(), "sk-sp-") {
		t.Fatalf("expected non-plan key to be rejected, got %v", err)
	}
}

func TestQwenCodingPlanRequiresKeyEnv(t *testing.T) {
	cfg := `
toolsets = ["db"]

[[providers]]
kind = "openai"
name = "qwen"
model = "qwen3-coder-plus"
subscription = "qwen"
`
	root := writeProfile(t, "default", cfg, "# General assistant", true, false)
	if _, err := Load(root, "default"); err == nil || !strings.Contains(err.Error(), "api_key_env") {
		t.Fatalf("expected missing Qwen key env to fail, got %v", err)
	}
}

func TestKimiCodePlanUsesNativeAPIKey(t *testing.T) {
	cfg := `
toolsets = ["db"]

[[providers]]
kind = "openai"
name = "kimi"
model = "k3"
subscription = "kimi"
api_key_env = "KIMI_PLAN_KEY"
`
	root := writeProfile(t, "default", cfg, "# General assistant", true, false)
	p, err := Load(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIMI_PLAN_KEY", "sk-kimi-test-plan-key")
	source, err := tokenSource(p, p.Config.Providers[0])
	if err != nil {
		t.Fatal(err)
	}
	token, err := source.Token(context.Background())
	if err != nil || token != "sk-kimi-test-plan-key" {
		t.Fatalf("token=%q err=%v", token, err)
	}

	t.Setenv("KIMI_PLAN_KEY", "moonshot-payg-key")
	if _, err := tokenSource(p, p.Config.Providers[0]); err == nil || !strings.Contains(err.Error(), "sk-kimi-") {
		t.Fatalf("expected non-plan key to be rejected, got %v", err)
	}
}

func TestKimiCodePlanRequiresKeyEnv(t *testing.T) {
	cfg := `
toolsets = ["db"]

[[providers]]
kind = "openai"
name = "kimi"
model = "k3"
subscription = "kimi"
`
	root := writeProfile(t, "default", cfg, "# General assistant", true, false)
	if _, err := Load(root, "default"); err == nil || !strings.Contains(err.Error(), "api_key_env") {
		t.Fatalf("expected missing Kimi key env to fail, got %v", err)
	}
}

func TestOpenCodeAPIModeRouting(t *testing.T) {
	tests := map[string]string{
		"glm-5.2":      "chat_completions",
		"minimax-m2.7": "anthropic_messages",
		"qwen3.7-max":  "anthropic_messages",
	}
	for modelName, want := range tests {
		got := providerAPIMode(ProviderConfig{Name: "opencode-go", Kind: "openai", Model: modelName})
		if got != want {
			t.Errorf("%s mode = %s, want %s", modelName, got, want)
		}
	}
	zen := map[string]string{
		"gpt-5.6-terra": "responses", "claude-opus-4-8": "anthropic_messages", "gemini-3-pro": "chat_completions",
	}
	for modelName, want := range zen {
		got := providerAPIMode(ProviderConfig{Name: "opencode-zen", Kind: "openai", Model: modelName})
		if got != want {
			t.Errorf("%s mode = %s, want %s", modelName, got, want)
		}
	}
}

func TestBuildNamedProviderIgnoresOtherCredentials(t *testing.T) {
	cfg := `
toolsets = ["db"]

[[providers]]
kind = "openai"
name = "first"
model = "model-1"
base_url = "https://first.test/v1"
api_key_env = "FIRST_KEY"

[[providers]]
kind = "openai"
name = "second"
model = "model-2"
base_url = "https://second.test/v1"
api_key_env = "SECOND_KEY"
`
	root := writeProfile(t, "default", cfg, "# General assistant", true, false)
	p, err := Load(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SECOND_KEY", "second-key")
	provider, err := BuildNamedProvider(p, "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "second/model-2" {
		t.Fatalf("provider = %s", provider.Name())
	}
}

func TestXAIPlanUsesStandardAPI(t *testing.T) {
	pc := ProviderConfig{Kind: "openai", Name: "grok", Model: "grok-build", Subscription: "xai"}
	if got := providerAPIMode(pc); got != "chat_completions" {
		t.Fatalf("api mode = %q", got)
	}
	// A SuperGrok subscription token authenticates against the standard xAI
	// API, not the version-gated Grok Build CLI proxy.
	if got := providerBaseURL(pc); got != "https://api.x.ai/v1" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestProviderUsageKind(t *testing.T) {
	tests := []struct {
		name string
		pc   ProviderConfig
		url  string
		want model.AccountUsageKind
	}{
		{name: "grok", pc: ProviderConfig{Subscription: "xai"}, want: model.AccountUsageGrok},
		{name: "kimi", pc: ProviderConfig{Subscription: "kimi"}, want: model.AccountUsageKimi},
		{name: "opencode go", url: "https://opencode.ai/zen/go/v1", want: model.AccountUsageOpenCodeGo},
		{name: "opencode go trailing slash", url: "https://opencode.ai/zen/go/v1/", want: model.AccountUsageOpenCodeGo},
		{name: "lookalike host", url: "https://opencode.ai.example/zen/go/v1"},
		{name: "lookalike path", url: "https://opencode.ai/zen/go/v1-proxy"},
		{name: "custom openai", url: "https://example.test/v1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := providerUsageKind(test.pc, test.url); got != test.want {
				t.Fatalf("usage kind = %q, want %q", got, test.want)
			}
		})
	}
}

func TestEffortValidation(t *testing.T) {
	cfg := strings.Replace(minimalConfig, `effort = "high"`, `effort = "xhigh"`, 1)
	root := writeProfile(t, "default", cfg, "# General assistant", true, true)
	if _, err := Load(root, "default"); err == nil {
		t.Fatal("expected an unsupported effort value to be refused")
	}
}

// Build must register exactly the allowlisted toolsets.
func TestBuildRegistersOnlyAllowlistedTools(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant", true, true)
	t.Setenv("OPENCODE_GO_API_KEY", "test-key")

	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := Build(p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer rt.Close()

	got := strings.Join(rt.Tools.Names(), ",")
	want := "exec,query,read_skill"
	if got != want {
		t.Fatalf("registered tools = %q, want %q", got, want)
	}
	// file was not allowlisted, so it must not exist for this profile.
	for _, name := range []string{"read_file", "write_file", "list_files"} {
		if _, ok := rt.Tools.Lookup(name); ok {
			t.Errorf("%s registered despite not being allowlisted", name)
		}
	}
}

// The system prompt is assembled once and must stay byte-identical across
// turns, or the prompt cache misses and the whole prefix is re-billed.
func TestSystemPromptIsStable(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant\n\nBe direct.", true, true)
	t.Setenv("OPENCODE_GO_API_KEY", "test-key")

	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := Build(p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer rt.Close()

	first := rt.System()
	for i := 0; i < 5; i++ {
		if got := rt.System(); got != first {
			t.Fatalf("system prompt changed on call %d", i)
		}
	}
	if !strings.Contains(first, "Be direct.") {
		t.Fatal("system prompt missing the soul")
	}
	if !strings.Contains(first, "database-guide") {
		t.Fatal("system prompt missing the skill catalog")
	}
	// The catalog is a summary, never the documents themselves. Loading every
	// skill body into the prefix would cost thousands of tokens per turn.
	if strings.Contains(first, "SENTINEL_SKILL_BODY_DO_NOT_INLINE") {
		t.Fatal("system prompt leaked skill document bodies")
	}
}

// Missing credentials must fail at startup, not mid-run.
func TestBuildFailsWhenKeyEnvMissing(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant", true, true)
	t.Setenv("OPENCODE_GO_API_KEY", "")

	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = Build(p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("expected Build to fail when the key env var is unset")
	}
	if !strings.Contains(err.Error(), "OPENCODE_GO_API_KEY") {
		t.Fatalf("error should name the missing variable, got: %v", err)
	}
}

func TestUnimplementedToolsetIsRefused(t *testing.T) {
	cfg := strings.Replace(minimalConfig, `["db", "skills"]`, `["db", "shell"]`, 1)
	root := writeProfile(t, "default", cfg, "# General assistant", true, true)
	t.Setenv("OPENCODE_GO_API_KEY", "test-key")

	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Build(p, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("expected the shell toolset to be refused until implemented")
	}
}

func TestAuthPathIsProfileScoped(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant", true, true)
	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := p.AuthPath("xai")
	if !strings.HasSuffix(got, filepath.Join("profiles", "default", "auth", "xai.json")) {
		t.Fatalf("auth path not profile-scoped: %s", got)
	}
	pooled := p.AccountAuthPath("xai", "backup")
	if !strings.HasSuffix(pooled, filepath.Join("profiles", "default", "auth", "xai", "backup.json")) {
		t.Fatalf("pooled auth path not profile-scoped: %s", pooled)
	}
}

func TestSubscriptionAccountPoolParses(t *testing.T) {
	cfg := `
toolsets = ["db"]

[[providers]]
kind = "openai"
name = "codex"
model = "gpt-5.5"
subscription = "codex"
accounts = ["primary", "backup-2"]
`
	root := writeProfile(t, "default", cfg, "# General assistant", true, false)
	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := p.Config.Providers[0].Accounts; len(got) != 2 || got[0] != "primary" || got[1] != "backup-2" {
		t.Fatalf("accounts = %#v", got)
	}
	if source, err := tokenSource(p, p.Config.Providers[0]); err != nil {
		t.Fatalf("tokenSource: %v", err)
	} else if _, ok := source.(*model.TokenPool); !ok {
		t.Fatalf("token source = %T, want *model.TokenPool", source)
	}
}

func TestAccountPoolNamesAreValidated(t *testing.T) {
	base := `
toolsets = ["db"]

[[providers]]
kind = "openai"
name = "codex"
model = "gpt-5.5"
subscription = "codex"
accounts = %s
`
	for _, accounts := range []string{`["only"]`, `["same", "same"]`, `["safe", "../escape"]`} {
		root := writeProfile(t, "default", fmt.Sprintf(base, accounts), "# General assistant", true, false)
		if _, err := Load(root, "default"); err == nil {
			t.Fatalf("accounts %s: expected validation error", accounts)
		}
	}
}

// Two profiles must share nothing: separate database, notes, skills, and auth.
func TestProfilesAreIsolated(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant", true, true)

	maintDir := filepath.Join(root, "profiles", "maint")
	if err := os.MkdirAll(maintDir, 0o700); err != nil {
		t.Fatalf("mkdir maint: %v", err)
	}
	maintCfg := `
toolsets = ["file"]

[[providers]]
kind = "openai"
name = "opencode-go"
model = "glm-5.2"
base_url = "https://opencode.ai/zen/go/v1"
api_key_env = "OPENCODE_GO_API_KEY"
`
	if err := os.WriteFile(filepath.Join(maintDir, "config.toml"), []byte(maintCfg), 0o600); err != nil {
		t.Fatalf("write maint config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(maintDir, "SOUL.md"), []byte("# Infra assistant"), 0o600); err != nil {
		t.Fatalf("write maint soul: %v", err)
	}

	defaultProfile, err := Load(root, "default")
	if err != nil {
		t.Fatalf("load default: %v", err)
	}
	maint, err := Load(root, "maint")
	if err != nil {
		t.Fatalf("load maint: %v", err)
	}

	if defaultProfile.DBPath == maint.DBPath || defaultProfile.AuthDir == maint.AuthDir || defaultProfile.NotesDir == maint.NotesDir {
		t.Fatal("profiles must not share paths")
	}
	if maint.HasToolset("db") {
		t.Fatal("maint must not have database access")
	}

	names, err := List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 2 || names[0] != "default" || names[1] != "maint" {
		t.Fatalf("List = %v", names)
	}
}

// maint is read-only by design: it reports, it does not mutate.
func TestMaintFileAccessIsReadOnly(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "profiles", "maint")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := `
toolsets = ["file"]

[[providers]]
kind = "openai"
name = "opencode-go"
model = "glm-5.2"
base_url = "https://opencode.ai/zen/go/v1"
api_key_env = "OPENCODE_GO_API_KEY"
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("# Infra assistant"), 0o600); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	t.Setenv("OPENCODE_GO_API_KEY", "test-key")

	p, err := Load(root, "maint")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := Build(p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer rt.Close()

	if _, ok := rt.Tools.Lookup("write_file"); ok {
		t.Fatal("maint must not be given write_file")
	}
	if _, ok := rt.Tools.Lookup("read_file"); !ok {
		t.Fatal("maint should have read_file")
	}
}
