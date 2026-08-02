package profile

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/arcbjorn/odin/agent"
	"github.com/arcbjorn/odin/model"
	"github.com/arcbjorn/odin/tools"
)

// Runtime is a profile assembled into live components.
type Runtime struct {
	Profile *Profile
	// Provider is the chain from config.toml. It is never switched, so
	// anything bound to it keeps the committed configuration.
	Provider model.Provider
	Tools    *agent.Registry
	// Loop runs scheduled jobs against Provider. A /model switch does not
	// reach it: a job's model is whatever config.toml says, always.
	Loop *agent.Loop

	// Router is the swappable provider behind interactive turns.
	Router *model.Router
	// Switcher resolves and applies /model changes. Nil only when the
	// profile configures no providers, which Load already rejects.
	Switcher *Switcher
	// Chat runs interactive turns (Telegram, `odin ask`) through Router, so a
	// switch takes effect without rebuilding the agent or losing history.
	Chat *agent.Loop

	// DB is nil when the db toolset is not enabled.
	DB       *sql.DB
	Store    *tools.SQLite
	Skills   *tools.Skills
	Location *time.Location
}

// Close releases resources held by the runtime.
func (r *Runtime) Close() error {
	if r.DB != nil {
		return r.DB.Close()
	}
	return nil
}

// Build turns a loaded profile into a runnable agent.
//
// Tools are registered strictly from the allowlist. A toolset absent from
// config.toml is never constructed, so its capability does not exist.
func Build(p *Profile, log *slog.Logger) (*Runtime, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := p.EnsureDirs(); err != nil {
		return nil, err
	}
	location, err := p.Location()
	if err != nil {
		return nil, err
	}

	// Credentials are resolved once and reused: a later /model switch rebuilds
	// transports, and a second token source over the same OAuth credential
	// would race the first one's refresh.
	tokens, err := providerTokens(p)
	if err != nil {
		return nil, err
	}
	provider, err := buildProviderWith(p, tokens, log)
	if err != nil {
		return nil, err
	}

	rt := &Runtime{Profile: p, Provider: provider, Tools: agent.NewRegistry(), Location: location}
	rt.Router = model.NewRouter(provider)
	rt.Switcher, err = NewSwitcher(p, rt.Router, tokens, log)
	if err != nil {
		return nil, err
	}
	// A persisted override is applied offline — no catalog call — so startup
	// stays fast and a provider that is merely unreachable at boot does not
	// discard the user's choice. An override that no longer resolves is
	// reported and dropped rather than failing the whole start.
	if overrideProvider, overrideModel, err := p.ModelOverride(); err != nil {
		log.Warn("could not read stored model override", "error", err)
	} else if overrideProvider != "" {
		if err := rt.Switcher.applyStored(overrideProvider, overrideModel); err != nil {
			log.Warn("ignoring stored model override",
				"provider", overrideProvider, "model", overrideModel, "error", err)
			if clearErr := p.SetModelOverride("", ""); clearErr != nil {
				log.Warn("could not clear stale model override", "error", clearErr)
			}
		} else {
			log.Info("interactive model override active", "target", rt.Switcher.Target().String())
		}
	}

	if p.HasToolset("db") {
		db, err := tools.OpenDB(p.DBPath)
		if err != nil {
			return nil, err
		}
		if applied, err := ApplyMigrations(context.Background(), db, p.MigrationsDir); err != nil {
			db.Close()
			return nil, err
		} else if applied > 0 {
			log.Info("database migrations applied", "count", applied)
		}
		store, err := tools.NewSQLite(db, tools.SQLiteConfig{
			Location: location, MaxAffectedRows: p.Config.Database.MaxAffectedRows,
		})
		if err != nil {
			db.Close()
			return nil, err
		}
		rt.DB, rt.Store = db, store

		for _, t := range []agent.Tool{store.QueryTool(), store.ExecTool()} {
			if err := rt.Tools.Register(t); err != nil {
				rt.Close()
				return nil, err
			}
		}

	}

	if p.HasToolset("file") {
		files, err := tools.NewFiles(tools.FilesConfig{
			Root: p.NotesDir,
			// File writes require the database-enabled profile boundary.
			ReadOnly: !p.HasToolset("db"),
		})
		if err != nil {
			rt.Close()
			return nil, err
		}
		for _, t := range files.Tools() {
			if err := rt.Tools.Register(t); err != nil {
				rt.Close()
				return nil, err
			}
		}
	}

	if p.HasToolset("skills") {
		skills, err := tools.NewSkills(p.SkillsDir)
		if err != nil {
			rt.Close()
			return nil, err
		}
		rt.Skills = skills
		if err := rt.Tools.Register(skills.Tool()); err != nil {
			rt.Close()
			return nil, err
		}
	}

	if p.HasToolset("web") {
		web, err := buildWeb(p, log)
		if err != nil {
			rt.Close()
			return nil, err
		}
		for _, t := range web.Tools() {
			if err := rt.Tools.Register(t); err != nil {
				rt.Close()
				return nil, err
			}
		}
	}

	if p.HasToolset("shell") {
		// A read-only ops primitive whose safety is the OS user it runs as, not
		// application logic. See tools.Shell. Enable it only for a profile whose
		// service user is confined (unprivileged, read-only kubeconfig).
		if err := rt.Tools.Register(tools.NewShell(tools.ShellConfig{}).Tool()); err != nil {
			rt.Close()
			return nil, err
		}
	}

	// Two loops over one tool registry and one system prompt, differing only in
	// which provider they call. Jobs get the committed chain; interactive
	// turns get the router, so /model moves the conversation without ever
	// moving the schedule.
	loopCfg := agent.Config{
		Provider:  provider,
		Tools:     rt.Tools,
		Logger:    log,
		System:    rt.System(),
		MaxTurns:  p.Config.MaxTurns,
		MaxTokens: p.Config.MaxTokens,
		Effort:    p.Config.Effort,
	}
	loop, err := agent.New(loopCfg)
	if err != nil {
		rt.Close()
		return nil, err
	}
	rt.Loop = loop

	chatCfg := loopCfg
	chatCfg.Provider = rt.Router
	chat, err := agent.New(chatCfg)
	if err != nil {
		rt.Close()
		return nil, err
	}
	rt.Chat = chat
	return rt, nil
}

// buildWeb assembles the web toolset. Search is only wired when a backend URL
// is configured — an absent capability beats one that always errors.
func buildWeb(p *Profile, log *slog.Logger) (*tools.Web, error) {
	cfg := tools.WebConfig{ReaderURL: p.Config.Web.ReaderURL}

	// The reader key is optional; it only raises the rate limit. Like every
	// other secret it comes from the environment, never from config.toml.
	if env := p.Config.Web.ReaderKeyEnv; env != "" {
		key := os.Getenv(env)
		if key == "" {
			log.Warn("reader key env var is unset; using the lower keyless rate limit",
				"env", env)
		}
		cfg.ReaderKey = key
	}

	if url := p.Config.Web.SearchURL; url != "" {
		searcher, err := tools.NewSearXNG(tools.SearXNGConfig{BaseURL: url})
		if err != nil {
			return nil, fmt.Errorf("web search: %w", err)
		}
		cfg.Searcher = searcher
	}
	return tools.NewWeb(cfg), nil
}

// System builds the stable system prompt from configured files and the skill
// catalog.
//
// Assembled once at startup and never rebuilt per turn. It must stay
// byte-identical across requests or the provider's prompt cache misses and
// the whole prefix is re-billed at full rate every call. Nothing volatile —
// no timestamps, no per-request IDs — belongs here.
func (r *Runtime) System() string {
	var b strings.Builder
	b.WriteString(r.Profile.System)

	if r.Skills != nil {
		if catalog, err := r.Skills.Catalog(); err == nil && catalog != "" {
			b.WriteString("\n\n## Available skills\n\n")
			b.WriteString(catalog)
			b.WriteString("\n\nRead a skill with read_skill before relying on its procedures.")
		}
	}
	return b.String()
}

// buildProvider assembles the fallback chain from config, in order.
func buildProvider(p *Profile, log *slog.Logger) (model.Provider, error) {
	sources, err := providerTokens(p)
	if err != nil {
		return nil, err
	}
	return buildProviderWith(p, sources, log)
}

// buildProviderWith assembles the chain from already resolved credentials.
func buildProviderWith(p *Profile, sources map[string]model.TokenSource, log *slog.Logger) (model.Provider, error) {
	providers := make([]model.Provider, 0, len(p.Config.Providers))

	for _, pc := range p.Config.Providers {
		tokens, ok := sources[pc.Name]
		if !ok {
			return nil, fmt.Errorf("provider %q: no resolved credential", pc.Name)
		}
		provider, err := buildTransport(pc, tokens, log)
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}

	if len(providers) == 1 {
		return providers[0], nil
	}
	return model.NewChain(model.ChainConfig{Providers: providers, Logger: log})
}

// providerTokens resolves one token source per configured provider.
//
// Resolved once and shared for the process lifetime: rebuilding a transport
// after a /model switch must reuse that provider's existing source. Two
// sources over one credential would refresh the same OAuth token
// independently and race each other's rotation.
func providerTokens(p *Profile) (map[string]model.TokenSource, error) {
	sources := make(map[string]model.TokenSource, len(p.Config.Providers))
	for _, pc := range p.Config.Providers {
		tokens, err := tokenSource(p, pc)
		if err != nil {
			return nil, err
		}
		sources[pc.Name] = tokens
	}
	return sources, nil
}

// buildTransport constructs one provider from its config and an already
// resolved token source.
//
// The model name is an input to construction, not merely a per-request field:
// it selects the wire protocol on aggregator endpoints (see providerAPIMode)
// and decides whether Anthropic thinking fields are sent at all. That is why
// switching models rebuilds the transport rather than mutating a field —
// mutation would keep the previous model's protocol.
func buildTransport(pc ProviderConfig, tokens model.TokenSource, log *slog.Logger) (model.Provider, error) {
	baseURL := providerBaseURL(pc)
	var provider model.Provider
	switch providerAPIMode(pc) {
	case "anthropic_messages":
		if pc.Subscription == "minimax" && !strings.HasSuffix(strings.TrimRight(baseURL, "/"), "/v1") {
			baseURL = strings.TrimRight(baseURL, "/") + "/v1"
		}
		userAgent := ""
		if pc.Subscription == "claude" {
			userAgent = claudeCodeUserAgent()
		}
		provider = model.NewAnthropic(model.AnthropicConfig{
			Provider: pc.Name, Model: pc.Model, BaseURL: baseURL, Tokens: tokens,
			Bearer:        pc.Subscription == "claude" || pc.Subscription == "minimax",
			OAuthIdentity: pc.Subscription == "claude",
			UserAgent:     userAgent,
			Effort:        pc.Effort,
			Logger:        log,
			DropThinking:  !strings.Contains(strings.ToLower(pc.Model), "claude"),
		})
	case "responses":
		provider = model.NewResponses(model.ResponsesConfig{
			Provider: pc.Name, Model: pc.Model, BaseURL: baseURL, Tokens: tokens,
			Codex: pc.Subscription == "codex", XAI: pc.Subscription == "xai",
			Effort: pc.Effort, Logger: log,
		})
	case "chat_completions":
		headers := map[string]string(nil)
		dropEffort := pc.DropEffort
		if pc.Subscription == "xai" {
			// The standard xAI API takes the subscription token as a plain
			// bearer and needs no proxy-specific headers. reasoning_effort
			// is accepted on grok-4.5; leave DropEffort to config.
		}
		if pc.Subscription == "kimi" {
			headers = map[string]string{"User-Agent": "github.com/arcbjorn/odin/1"}
		}
		provider = model.NewOpenAI(model.OpenAIConfig{
			Provider:   pc.Name,
			Model:      pc.Model,
			BaseURL:    baseURL,
			Tokens:     tokens,
			DropEffort: dropEffort,
			Effort:     pc.Effort,
			Headers:    headers,
			Logger:     log,
		})
	default:
		return nil, fmt.Errorf("provider %q: could not resolve api mode", pc.Name)
	}

	if usageKind := providerUsageKind(pc, baseURL); usageKind != "" {
		provider = model.WithAccountUsage(provider, model.AccountUsageConfig{
			Kind:            usageKind,
			Provider:        pc.Name,
			BaseURL:         baseURL,
			Tokens:          tokens,
			WorkspaceID:     os.Getenv("OPENCODE_GO_WORKSPACE_ID"),
			DashboardCookie: os.Getenv("OPENCODE_GO_AUTH_COOKIE"),
		})
	}
	return provider, nil
}

func providerUsageKind(pc ProviderConfig, baseURL string) model.AccountUsageKind {
	switch pc.Subscription {
	case "xai":
		return model.AccountUsageGrok
	case "kimi":
		return model.AccountUsageKimi
	}
	parsed, err := url.Parse(baseURL)
	if err == nil && strings.EqualFold(parsed.Scheme, "https") && strings.EqualFold(parsed.Hostname(), "opencode.ai") &&
		strings.TrimRight(parsed.Path, "/") == "/zen/go/v1" {
		return model.AccountUsageOpenCodeGo
	}
	return ""
}

// BuildNamedProvider constructs one configured provider without resolving the
// rest of the fallback chain. Live verification uses this so a missing backup
// credential cannot hide or block the provider being checked.
func BuildNamedProvider(p *Profile, name string, log *slog.Logger) (model.Provider, error) {
	var selected *ProviderConfig
	var names []string
	for i := range p.Config.Providers {
		pc := &p.Config.Providers[i]
		names = append(names, pc.Name)
		if pc.Name == name {
			selected = pc
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("unknown provider %q (configured: %s)", name, strings.Join(names, ", "))
	}
	copyProfile := *p
	copyProfile.Config = p.Config
	copyProfile.Config.Providers = []ProviderConfig{*selected}
	return buildProvider(&copyProfile, log)
}

func providerBaseURL(pc ProviderConfig) string {
	if pc.BaseURL != "" {
		return pc.BaseURL
	}
	switch pc.Subscription {
	case "codex":
		return "https://chatgpt.com/backend-api/codex"
	case "claude":
		return "https://api.anthropic.com/v1"
	case "xai":
		// The standard xAI API accepts a SuperGrok subscription OAuth token as
		// a plain bearer. The cli-chat-proxy.grok.com endpoint is a separate
		// Grok Build product that version-gates requests (HTTP 426); it is not
		// what a SuperGrok plan authenticates against.
		return "https://api.x.ai/v1"
	case "minimax":
		return "https://api.minimax.io/anthropic"
	case "qwen":
		return "https://coding-intl.dashscope.aliyuncs.com/v1"
	case "kimi":
		return "https://api.kimi.com/coding/v1"
	default:
		return pc.BaseURL
	}
}

// tokenSource resolves credentials without ever placing them in config.
func tokenSource(p *Profile, pc ProviderConfig) (model.TokenSource, error) {
	if len(pc.Accounts) > 0 {
		accounts := make([]model.AccountTokenSource, 0, len(pc.Accounts))
		for _, name := range pc.Accounts {
			source, err := singleTokenSource(pc, p.AccountAuthPath(pc.Name, name))
			if err != nil {
				return nil, err
			}
			accounts = append(accounts, model.AccountTokenSource{Name: name, Source: source})
		}
		return model.NewTokenPool(model.TokenPoolConfig{Accounts: accounts})
	}
	return singleTokenSource(pc, p.AuthPath(pc.Name))
}

func singleTokenSource(pc ProviderConfig, authPath string) (model.TokenSource, error) {
	if pc.Subscription == "qwen" || pc.Subscription == "kimi" {
		key, err := providerKey(pc)
		if err != nil {
			return nil, err
		}
		if pc.Subscription == "qwen" && !strings.HasPrefix(key, "sk-sp-") {
			return nil, fmt.Errorf("provider %q: %s is not a Qwen Coding Plan key (expected sk-sp- prefix)", pc.Name, keySourceName(pc))
		}
		if pc.Subscription == "kimi" && !strings.HasPrefix(key, "sk-kimi-") {
			return nil, fmt.Errorf("provider %q: %s is not a Kimi Code plan key (expected sk-kimi- prefix)", pc.Name, keySourceName(pc))
		}
		return model.StaticToken(key), nil
	}
	if pc.Subscription != "" {
		return model.NewSubscriptionSource(pc.Subscription, authPath)
	}
	if pc.OAuth {
		return model.NewOAuthSource(model.OAuthConfig{
			Path:     authPath,
			ClientID: pc.ClientID,
			TokenURL: pc.TokenURL,
			Scope:    pc.Scope,
		}), nil
	}

	key, err := providerKey(pc)
	if err != nil {
		return nil, err
	}
	return model.StaticToken(key), nil
}

// keyCommandTimeout bounds a credential command. A secret store that hangs
// must fail the start rather than wedge it before the gateway ever opens.
const keyCommandTimeout = 30 * time.Second

// providerKey resolves a plan or API key from whichever source config names.
//
// Both paths resolve once, at startup, so a missing credential fails where
// someone is watching rather than at 07:00 inside a cron run with nobody
// looking. Rotating a key means restarting the agent either way.
func providerKey(pc ProviderConfig) (string, error) {
	if pc.APIKeyCmd != "" {
		return runKeyCommand(pc)
	}
	key := strings.TrimSpace(os.Getenv(pc.APIKeyEnv))
	if key == "" {
		return "", fmt.Errorf("provider %q: %s is not set in the environment", pc.Name, pc.APIKeyEnv)
	}
	return key, nil
}

// runKeyCommand takes a command's stdout as the credential.
//
// This exists so a key can come from the store that already holds it — sops,
// a password manager, a vault CLI — instead of widening the systemd unit's
// EnvironmentFile. It runs as the agent's own user, which is the same trust
// boundary as the environment variable it replaces, not a new one.
//
// stdout is the secret and is never logged. stderr is reported on failure,
// because a command that failed produced no key, and without its diagnostics
// a misconfigured secret store is invisible.
func runKeyCommand(pc ProviderConfig) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), keyCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", pc.APIKeyCmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 300 {
			detail = detail[:300] + "\u2026"
		}
		if detail != "" {
			return "", fmt.Errorf("provider %q: api_key_cmd failed: %w: %s", pc.Name, err, detail)
		}
		return "", fmt.Errorf("provider %q: api_key_cmd failed: %w", pc.Name, err)
	}

	key := strings.TrimSpace(stdout.String())
	if key == "" {
		return "", fmt.Errorf("provider %q: api_key_cmd produced no key on stdout", pc.Name)
	}
	return key, nil
}

// keySourceName names the configured credential source for an error message,
// without ever revealing what it produced.
func keySourceName(pc ProviderConfig) string {
	if pc.APIKeyCmd != "" {
		return "api_key_cmd output"
	}
	return pc.APIKeyEnv
}

func providerAPIMode(pc ProviderConfig) string {
	if pc.APIMode != "" && pc.APIMode != "auto" {
		return pc.APIMode
	}
	switch pc.Subscription {
	case "codex":
		return "responses"
	case "xai":
		return "chat_completions"
	case "claude", "minimax":
		return "anthropic_messages"
	case "qwen", "kimi":
		return "chat_completions"
	}

	modelName := strings.ToLower(pc.Model)
	if slash := strings.LastIndex(modelName, "/"); slash >= 0 {
		modelName = modelName[slash+1:]
	}
	switch pc.Name {
	case "opencode-go":
		if strings.HasPrefix(modelName, "minimax-") || strings.HasPrefix(modelName, "qwen") {
			return "anthropic_messages"
		}
		return "chat_completions"
	case "opencode-zen", "opencode":
		switch {
		case strings.HasPrefix(modelName, "claude-"), strings.HasPrefix(modelName, "qwen"):
			return "anthropic_messages"
		case strings.HasPrefix(modelName, "gpt-"):
			return "responses"
		default:
			return "chat_completions"
		}
	}
	if pc.Kind == "anthropic" {
		return "anthropic_messages"
	}
	return "chat_completions"
}

func claudeCodeUserAgent() string {
	version := "2.1.74"
	if path, err := exec.LookPath("claude"); err == nil {
		if output, err := exec.Command(path, "--version").Output(); err == nil {
			if fields := strings.Fields(string(output)); len(fields) > 0 && fields[0] != "" {
				version = fields[0]
			}
		}
	}
	return "claude-code/" + version + " (external, cli)"
}
