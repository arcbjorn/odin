// Package profile loads a single agent's configuration.
//
// A profile is a directory:
//
//	<root>/profiles/<name>/
//	  config.toml      model chain, toolset allowlist, limits
//	  SOUL.md          primary system prompt
//	  context/         optional ordered system-prompt fragments
//	  skills/          markdown skill documents
//	  migrations/      profile-owned database migrations
//	  notes/           the model's scoped file area
//	  db.sqlite        profile domain database
//	  state/           runtime state, including timezone override
//	  auth/            OAuth tokens, 0600
//
// Everything is profile-scoped. There are no globals and no default profile;
// a name that does not resolve to a directory is a hard error.
package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Profile is a loaded, validated agent configuration.
type Profile struct {
	Name string
	Dir  string

	// Soul is the system prompt, read from SOUL.md.
	Soul string
	// System is the ordered, stable composition of every configured system
	// file. Soul remains available for callers that only need the primary file.
	System string

	Config Config

	// Resolved paths. Every one is inside Dir.
	SkillsDir     string
	NotesDir      string
	DBPath        string
	AuthDir       string
	StateDir      string
	MigrationsDir string
}

// Config is the parsed config.toml.
type Config struct {
	// SystemFiles are composed in order into the stable prompt. Empty defaults
	// to SOUL.md for backward compatibility.
	SystemFiles []string

	// Toolsets is the allowlist. A tool absent here is never registered, so
	// the model cannot call it. This is a profile boundary, not a prompt rule.
	Toolsets []string

	// Providers is the fallback chain, in order. providers[0] is primary.
	Providers []ProviderConfig

	// Timezone is the committed default. A machine-local runtime override may
	// temporarily replace it while travelling.
	Timezone string

	MaxTurns  int
	MaxTokens int
	Effort    string

	Telegram TelegramConfig
	Web      WebConfig
	Database DatabaseConfig
}

// DatabaseConfig defines generic write boundaries. Domain behavior belongs in
// profile skills and schema constraints, not in the runtime tool.
type DatabaseConfig struct {
	// MaxAffectedRows rolls a write back when it would change more rows than
	// this. Zero disables the limit.
	MaxAffectedRows int64
}

// WebConfig configures the web toolset. All fields are optional: the defaults
// use the public markdown reader and leave search disabled.
type WebConfig struct {
	// ReaderURL is the markdown-reader prefix. Swappable so a self-hosted
	// reader can replace the public one without a code change.
	ReaderURL string
	// ReaderKeyEnv names an env var holding an optional reader key, which
	// raises the rate limit. The key itself is never in config.
	ReaderKeyEnv string
	// SearchURL points at a self-hosted SearXNG instance. Empty means no
	// search tool is offered at all.
	SearchURL string
}

// ProviderConfig is one entry in the fallback chain.
type ProviderConfig struct {
	// Kind is "openai" (any OpenAI-compatible endpoint) or "anthropic".
	Kind    string
	Name    string
	Model   string
	BaseURL string

	// APIKeyEnv names the environment variable holding the key. The key
	// itself never appears in config.toml — systemd injects it via
	// EnvironmentFile from a 0600 file owned by the agent user.
	APIKeyEnv string

	// APIMode selects the provider wire protocol. Empty means infer it from
	// Kind, provider name, and model. The explicit forms are chat_completions,
	// responses, and anthropic_messages.
	APIMode string

	// Subscription selects a first-party subscription preset. Codex, Claude,
	// xAI, and MiniMax use OAuth. Qwen and Kimi use plan API keys from
	// APIKeyEnv; no provider CLI credential stores are read.
	Subscription string

	// Accounts enables a named credential pool. Each account is stored in its
	// own auth/<provider>/<account>.json file and tried in declaration order.
	Accounts []string

	// OAuth marks a provider whose token lives in auth/<name>.json and is
	// refreshed before expiry.
	OAuth    bool
	ClientID string
	TokenURL string
	// DeviceURL is the device-authorization endpoint. Device flow is used
	// because the server is headless: no browser, no inbound port.
	DeviceURL string
	Scope     string

	// DropEffort suppresses reasoning_effort for models that reject it with
	// HTTP 400 even though they do reason.
	DropEffort bool
}

// TelegramConfig configures the chat gateway.
type TelegramConfig struct {
	TokenEnv string
	// AllowedUsers is a strict allowlist of Telegram user IDs. Empty means
	// the gateway refuses to start.
	AllowedUsers []int64
	// ChatID is where scheduled jobs deliver. Defaults to the first allowed
	// user, which is correct for a single-user agent in a direct message.
	ChatID int64
}

// knownToolsets is the closed set of toolset names. A typo in config.toml
// must fail at load, not silently omit a capability at 07:00.
var knownToolsets = map[string]bool{
	"db":     true, // sqlite query + exec against db.sqlite
	"file":   true, // scoped read/write/list under notes/
	"skills": true, // read markdown skill documents
	"web":    true, // fetch + search
	"shell":  true, // ops shell; confine its service user and credentials
}

// Load reads and validates the profile named by name under root.
func Load(root, name string) (*Profile, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("profile name is required")
	}
	// A profile name is a directory name, never a path.
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return nil, fmt.Errorf("invalid profile name %q", name)
	}

	dir := filepath.Join(root, "profiles", name)

	// Gotcha #9, made impossible: no fallback, no default, no silent empty
	// profile. If the directory is not there, stop.
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("profile %q does not exist at %s%s", name, dir, availableHint(root))
		}
		return nil, fmt.Errorf("stat profile %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("profile path %s is not a directory", dir)
	}

	p := &Profile{
		Name:          name,
		Dir:           dir,
		SkillsDir:     filepath.Join(dir, "skills"),
		NotesDir:      filepath.Join(dir, "notes"),
		DBPath:        filepath.Join(dir, "db.sqlite"),
		AuthDir:       filepath.Join(dir, "auth"),
		StateDir:      filepath.Join(dir, "state"),
		MigrationsDir: filepath.Join(dir, "migrations"),
	}

	raw, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		return nil, fmt.Errorf("read config.toml for profile %q: %w", name, err)
	}
	cfg, err := parseConfig(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse config.toml for profile %q: %w", name, err)
	}
	p.Config = cfg
	if len(p.Config.SystemFiles) == 0 {
		p.Config.SystemFiles = []string{"SOUL.md"}
	}

	var systemParts []string
	for i, name := range p.Config.SystemFiles {
		path, err := profileFile(p.Dir, name)
		if err != nil {
			return nil, fmt.Errorf("system_files[%d]: %w", i, err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read system file %q for profile %q: %w", name, p.Name, err)
		}
		bodyText := strings.TrimSpace(string(body))
		if bodyText == "" {
			return nil, fmt.Errorf("system file %q for profile %q is empty", name, p.Name)
		}
		if name == "SOUL.md" {
			p.Soul = bodyText
		}
		systemParts = append(systemParts, bodyText)
	}
	if p.Soul == "" {
		return nil, fmt.Errorf("system_files must include SOUL.md")
	}
	p.System = strings.Join(systemParts, "\n\n")

	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("profile %q: %w", name, err)
	}
	return p, nil
}

func (p *Profile) validate() error {
	if p.Config.Timezone == "" {
		return fmt.Errorf("timezone is required")
	}
	if _, err := time.LoadLocation(p.Config.Timezone); err != nil {
		return fmt.Errorf("unknown timezone %q: %w", p.Config.Timezone, err)
	}
	if p.Config.Database.MaxAffectedRows < 0 {
		return fmt.Errorf("database max_affected_rows must not be negative")
	}
	systemSeen := make(map[string]bool, len(p.Config.SystemFiles))
	for _, name := range p.Config.SystemFiles {
		if systemSeen[name] {
			return fmt.Errorf("duplicate system file %q", name)
		}
		systemSeen[name] = true
	}
	if len(p.Config.Providers) == 0 {
		return fmt.Errorf("no providers configured")
	}
	seen := map[string]bool{}
	for i, pr := range p.Config.Providers {
		switch pr.Kind {
		case "openai", "anthropic":
		default:
			return fmt.Errorf("provider %d: unknown kind %q (want openai or anthropic)", i, pr.Kind)
		}
		if pr.Name == "" {
			return fmt.Errorf("provider %d: name is required", i)
		}
		if pr.APIKeyEnv != "" && !validEnvironmentName(pr.APIKeyEnv) {
			return fmt.Errorf("provider %q: invalid api_key_env %q", pr.Name, pr.APIKeyEnv)
		}
		if strings.ContainsAny(pr.Name, `/\`) || strings.Contains(pr.Name, "..") {
			return fmt.Errorf("provider %d: invalid name %q", i, pr.Name)
		}
		if seen[pr.Name] {
			return fmt.Errorf("duplicate provider name %q", pr.Name)
		}
		seen[pr.Name] = true
		if pr.Model == "" {
			return fmt.Errorf("provider %q: model is required", pr.Name)
		}
		if pr.Kind == "openai" && pr.BaseURL == "" && pr.Subscription == "" {
			return fmt.Errorf("provider %q: base_url is required for openai-compatible providers", pr.Name)
		}
		switch pr.APIMode {
		case "", "auto", "chat_completions", "responses", "anthropic_messages":
		default:
			return fmt.Errorf("provider %q: unknown api_mode %q", pr.Name, pr.APIMode)
		}
		switch pr.Subscription {
		case "", "codex", "claude", "xai", "minimax", "qwen", "kimi":
		default:
			return fmt.Errorf("provider %q: unknown subscription %q", pr.Name, pr.Subscription)
		}
		if pr.OAuth && pr.Subscription != "" {
			return fmt.Errorf("provider %q: oauth and subscription are mutually exclusive", pr.Name)
		}
		if len(pr.Accounts) > 0 {
			if pr.Subscription == "qwen" || pr.Subscription == "kimi" {
				return fmt.Errorf("provider %q: %s plan keys do not support OAuth account pools", pr.Name, pr.Subscription)
			}
			if !pr.OAuth && pr.Subscription == "" {
				return fmt.Errorf("provider %q: accounts require oauth or subscription credentials", pr.Name)
			}
			if len(pr.Accounts) < 2 {
				return fmt.Errorf("provider %q: accounts requires at least two names", pr.Name)
			}
			accountNames := make(map[string]bool, len(pr.Accounts))
			for _, account := range pr.Accounts {
				if !validPathName(account) {
					return fmt.Errorf("provider %q: invalid account name %q", pr.Name, account)
				}
				if accountNames[account] {
					return fmt.Errorf("provider %q: duplicate account name %q", pr.Name, account)
				}
				accountNames[account] = true
			}
		}
		if pr.OAuth {
			if pr.TokenURL == "" || pr.ClientID == "" {
				return fmt.Errorf("provider %q: oauth requires client_id and token_url", pr.Name)
			}
		} else if (pr.Subscription == "qwen" || pr.Subscription == "kimi") && pr.APIKeyEnv == "" {
			return fmt.Errorf("provider %q: %s plan requires api_key_env", pr.Name, pr.Subscription)
		} else if pr.Subscription == "" && pr.APIKeyEnv == "" {
			return fmt.Errorf("provider %q: api_key_env is required when oauth and subscription are unset", pr.Name)
		}
		if err := validateSubscriptionTransport(pr); err != nil {
			return err
		}
	}

	if len(p.Config.Toolsets) == 0 {
		return fmt.Errorf("no toolsets enabled; an agent with no tools cannot read its own database")
	}
	toolsetSeen := make(map[string]bool, len(p.Config.Toolsets))
	for _, ts := range p.Config.Toolsets {
		if !knownToolsets[ts] {
			return fmt.Errorf("unknown toolset %q (known: %s)", ts, knownToolsetNames())
		}
		if toolsetSeen[ts] {
			return fmt.Errorf("duplicate toolset %q", ts)
		}
		toolsetSeen[ts] = true
	}

	// The db toolset needs a database. Discovering this at 07:00, inside
	// a cron run with no human present, is exactly the silent failure mode
	// this package exists to prevent.
	if p.HasToolset("db") {
		if _, err := os.Stat(p.DBPath); err != nil {
			return fmt.Errorf("toolset \"database\" is enabled but %s is missing", p.DBPath)
		}
	}
	if p.HasToolset("skills") {
		if _, err := os.Stat(p.SkillsDir); err != nil {
			return fmt.Errorf("toolset \"skills\" is enabled but %s is missing", p.SkillsDir)
		}
	}

	if p.Config.Telegram.TokenEnv != "" {
		if !validEnvironmentName(p.Config.Telegram.TokenEnv) {
			return fmt.Errorf("telegram token_env %q is not a valid environment variable name", p.Config.Telegram.TokenEnv)
		}
		if len(p.Config.Telegram.AllowedUsers) == 0 {
			// Guest access is never enabled implicitly.
			return fmt.Errorf("telegram is configured but allowed_users is empty; refusing to run an open gateway")
		}
		if p.Config.Telegram.ChatID == 0 {
			// Direct messages use the user's own ID as the chat ID.
			p.Config.Telegram.ChatID = p.Config.Telegram.AllowedUsers[0]
		}
	}
	if p.Config.Web.ReaderKeyEnv != "" && !validEnvironmentName(p.Config.Web.ReaderKeyEnv) {
		return fmt.Errorf("web reader_key_env %q is not a valid environment variable name", p.Config.Web.ReaderKeyEnv)
	}

	switch p.Config.Effort {
	case "", "low", "medium", "high":
	default:
		return fmt.Errorf("effort %q is not one of low, medium, high", p.Config.Effort)
	}
	return nil
}

func validateSubscriptionTransport(pr ProviderConfig) error {
	want := ""
	wantMode := ""
	switch pr.Subscription {
	case "codex", "qwen", "kimi":
		want = "openai"
		wantMode = "chat_completions"
		if pr.Subscription == "codex" {
			wantMode = "responses"
		}
	case "xai":
		want = "openai"
		wantMode = "chat_completions"
	case "claude", "minimax":
		want = "anthropic"
		wantMode = "anthropic_messages"
	}
	if want != "" && pr.Kind != want {
		return fmt.Errorf("provider %q: subscription %q requires kind %q", pr.Name, pr.Subscription, want)
	}
	if pr.APIMode != "" && pr.APIMode != "auto" && wantMode != "" && pr.APIMode != wantMode {
		return fmt.Errorf("provider %q: subscription %q requires api_mode %q", pr.Name, pr.Subscription, wantMode)
	}
	return nil
}

// HasToolset reports whether a toolset is in the allowlist.
func (p *Profile) HasToolset(name string) bool {
	for _, ts := range p.Config.Toolsets {
		if ts == name {
			return true
		}
	}
	return false
}

// EnsureDirs creates the writable directories a profile needs. Auth is 0700
// because it holds refresh tokens.
func (p *Profile) EnsureDirs() error {
	for _, dir := range []string{p.NotesDir, p.AuthDir, p.StateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

func profileFile(root, name string) (string, error) {
	if strings.TrimSpace(name) == "" || filepath.IsAbs(name) || strings.Contains(name, `\`) {
		return "", fmt.Errorf("invalid relative path %q", name)
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid relative path %q", name)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve profile directory: %w", err)
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(resolvedRoot, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("system file %q resolves outside the profile", name)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("system file %q is not a regular file", name)
	}
	return path, nil
}

// AuthPath is the credential file for a provider.
func (p *Profile) AuthPath(provider string) string {
	return filepath.Join(p.AuthDir, provider+".json")
}

// AccountAuthPath is the credential file for one named provider account.
func (p *Profile) AccountAuthPath(provider, account string) string {
	return filepath.Join(p.AuthDir, provider, account+".json")
}

func validPathName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		alphanumeric := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if alphanumeric || i > 0 && (r == '-' || r == '_') {
			continue
		}
		return false
	}
	return true
}

func validEnvironmentName(name string) bool {
	for i, r := range name {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r == '_' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return name != ""
}

// List returns the profile names available under root, for error messages and
// `odin status`.
func List(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "profiles"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// availableHint names the profiles that do exist, so a typo is obvious rather
// than sending the operator to read the source.
func availableHint(root string) string {
	names, err := List(root)
	if err != nil || len(names) == 0 {
		return ""
	}
	return fmt.Sprintf(" (available: %s)", strings.Join(names, ", "))
}

func knownToolsetNames() string {
	names := make([]string, 0, len(knownToolsets))
	for n := range knownToolsets {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
