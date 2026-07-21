package profile

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/arcbjorn/odin/agent"
	"github.com/arcbjorn/odin/model"
	"github.com/arcbjorn/odin/tools"
)

// Runtime is a profile assembled into live components.
type Runtime struct {
	Profile  *Profile
	Provider model.Provider
	Tools    *agent.Registry
	Loop     *agent.Loop

	// DB is nil when the db toolset is not enabled.
	DB     *sql.DB
	Store  *tools.SQLite
	Skills *tools.Skills
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

	provider, err := buildProvider(p, log)
	if err != nil {
		return nil, err
	}

	rt := &Runtime{Profile: p, Provider: provider, Tools: agent.NewRegistry()}

	if p.HasToolset("db") {
		db, err := tools.OpenDB(p.DBPath)
		if err != nil {
			return nil, err
		}
		store, err := tools.NewSQLite(db)
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

		// The database's timezone is authoritative and switchable live for
		// travel; config.toml's is informational. A mismatch means one of the
		// two is stale, which is worth surfacing before it misfiles a session.
		if p.Config.Timezone != "" && p.Config.Timezone != store.Location().String() {
			log.Warn("timezone mismatch between config and database; the database wins",
				"config", p.Config.Timezone, "db", store.Location().String())
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
		rt.Close()
		return nil, fmt.Errorf("toolset \"shell\" is not implemented yet")
	}

	loop, err := agent.New(agent.Config{
		Provider:  provider,
		Tools:     rt.Tools,
		Logger:    log,
		System:    rt.System(),
		MaxTurns:  p.Config.MaxTurns,
		MaxTokens: p.Config.MaxTokens,
		Effort:    p.Config.Effort,
	})
	if err != nil {
		rt.Close()
		return nil, err
	}
	rt.Loop = loop
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

// System builds the stable system prompt: SOUL.md, then the skill catalog.
//
// Assembled once at startup and never rebuilt per turn. It must stay
// byte-identical across requests or the provider's prompt cache misses and
// the whole prefix is re-billed at full rate every call. Nothing volatile —
// no timestamps, no per-request IDs — belongs here.
func (r *Runtime) System() string {
	var b strings.Builder
	b.WriteString(r.Profile.Soul)

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
	providers := make([]model.Provider, 0, len(p.Config.Providers))

	for _, pc := range p.Config.Providers {
		tokens, err := tokenSource(p, pc)
		if err != nil {
			return nil, err
		}

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
				DropThinking:  !strings.Contains(strings.ToLower(pc.Model), "claude"),
			})
		case "responses":
			provider = model.NewResponses(model.ResponsesConfig{
				Provider: pc.Name, Model: pc.Model, BaseURL: baseURL, Tokens: tokens,
				Codex: pc.Subscription == "codex", XAI: pc.Subscription == "xai",
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
				Headers:    headers,
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
		providers = append(providers, provider)
	}

	if len(providers) == 1 {
		return providers[0], nil
	}
	return model.NewChain(model.ChainConfig{Providers: providers, Logger: log})
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
		key := strings.TrimSpace(os.Getenv(pc.APIKeyEnv))
		if key == "" {
			return nil, fmt.Errorf("provider %q: %s is not set in the environment", pc.Name, pc.APIKeyEnv)
		}
		if pc.Subscription == "qwen" && !strings.HasPrefix(key, "sk-sp-") {
			return nil, fmt.Errorf("provider %q: %s is not a Qwen Coding Plan key (expected sk-sp- prefix)", pc.Name, pc.APIKeyEnv)
		}
		if pc.Subscription == "kimi" && !strings.HasPrefix(key, "sk-kimi-") {
			return nil, fmt.Errorf("provider %q: %s is not a Kimi Code plan key (expected sk-kimi- prefix)", pc.Name, pc.APIKeyEnv)
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

	key := os.Getenv(pc.APIKeyEnv)
	if key == "" {
		// Fail at startup, not at 07:00 inside a cron run with nobody watching.
		return nil, fmt.Errorf("provider %q: %s is not set in the environment", pc.Name, pc.APIKeyEnv)
	}
	return model.StaticToken(key), nil
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
