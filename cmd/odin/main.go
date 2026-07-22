// Command odin runs an agent profile.
//
//	odin run     --root DIR --profile NAME   scheduler + gateway
//	odin once    --profile NAME --job NAME   run one job now
//	odin ask     --profile NAME "question"   one turn on stdin/argv
//	odin status  --profile NAME              config, jobs, providers
//	odin auth    --profile NAME --provider P provider login
//	odin usage   --profile NAME              subscription limits
//	odin models  --profile NAME              live model catalogs
//	odin verify  --profile NAME --provider P live tool-loop check
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/arcbjorn/odin/agent"
	"github.com/arcbjorn/odin/gateway"
	"github.com/arcbjorn/odin/jobs"
	"github.com/arcbjorn/odin/model"
	"github.com/arcbjorn/odin/profile"
	"github.com/arcbjorn/odin/sched"
	"github.com/arcbjorn/odin/tools"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return // clean shutdown on SIGTERM
		}
		fmt.Fprintln(os.Stderr, "odin:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return errors.New("a subcommand is required")
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "init":
		return cmdInit(args)
	case "run":
		return cmdRun(args)
	case "once":
		return cmdOnce(args)
	case "ask":
		return cmdAsk(args)
	case "status":
		return cmdStatus(args)
	case "auth":
		return cmdAuth(args)
	case "usage":
		return cmdUsage(args)
	case "models":
		return cmdModels(args)
	case "verify":
		return cmdVerify(args)
	case "backup":
		return cmdBackup(args)
	case "restore":
		return cmdRestore(args)
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `odin - configurable agent

  odin init    --profile NAME --timezone ZONE
  odin run     --profile NAME   scheduler + telegram gateway
  odin once    --profile NAME --job NAME
  odin ask     --profile NAME "question"
  odin status  --profile NAME
  odin auth    --profile NAME --provider NAME [--account NAME]
  odin usage   --profile NAME [--provider NAME]
  odin models  --profile NAME [--provider NAME]
  odin verify  --profile NAME --provider NAME
  odin backup  --profile NAME [--keep N]
  odin restore --profile NAME --from FILE

Common flags:
  --root DIR    profile root (default $ODIN_ROOT, else current directory)
  --verbose     debug logging
`)
}

// cmdVerify pins one provider and runs a real two-turn tool protocol check.
func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	common := bindCommon(fs)
	providerName := fs.String("provider", "", "provider name from config.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}
	if *providerName == "" {
		return errors.New("--provider is required")
	}
	p, err := profile.Load(common.root, common.profile)
	if err != nil {
		return err
	}
	if err := p.EnsureDirs(); err != nil {
		return err
	}
	provider, err := profile.BuildNamedProvider(p, *providerName, common.logger())
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	result, err := model.VerifyProvider(ctx, provider)
	if err != nil {
		return fmt.Errorf("verify %s: %w", *providerName, err)
	}
	fmt.Printf("provider      %s\n", result.Provider)
	fmt.Printf("model         %s\n", result.Model)
	if result.CatalogChecked {
		fmt.Println("catalog       ok")
	} else {
		fmt.Println("catalog       unsupported")
	}
	fmt.Println("tool call     ok")
	fmt.Println("continuation  ok")
	return nil
}

// cmdBackup writes a consistent snapshot of the profile database.
//
// A one-shot command, driven externally by a cron line or systemd timer —
// deliberately outside the agent process, the same operational pattern as the
// watchdog. It uses VACUUM INTO, so it is safe to run while the agent writes.
func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	common := bindCommon(fs)
	keep := fs.Int("keep", 7, "number of backups to retain")
	dir := fs.String("dir", "", "backup directory (default <profile>/backups)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}

	p, err := profile.Load(common.root, common.profile)
	if err != nil {
		return err
	}

	backupDir := *dir
	if backupDir == "" {
		backupDir = filepath.Join(p.Dir, "backups")
	}

	path, err := tools.Backup(p.DBPath, backupDir, *keep)
	if err != nil {
		return err
	}
	fmt.Printf("backup written to %s\n", path)
	return nil
}

// cmdRestore replaces the profile database with a backup.
func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	common := bindCommon(fs)
	from := fs.String("from", "", "backup file to restore, or \"latest\"")
	force := fs.Bool("force", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}
	if *from == "" {
		return errors.New("--from is required (a backup file, or \"latest\")")
	}

	p, err := profile.Load(common.root, common.profile)
	if err != nil {
		return err
	}

	source := *from
	if source == "latest" {
		backups, err := tools.ListBackups(filepath.Join(p.Dir, "backups"))
		if err != nil {
			return err
		}
		if len(backups) == 0 {
			return errors.New("no backups found")
		}
		source = backups[0]
	}

	// Restoring under a running agent corrupts both files. We cannot reliably
	// detect a running process, so require an explicit acknowledgement instead.
	if !*force {
		fmt.Printf("Restore %s over %s?\nStop the agent first — restoring under a running writer corrupts the database.\nType 'yes' to proceed: ", source, p.DBPath)
		var answer string
		fmt.Scanln(&answer)
		if answer != "yes" {
			return errors.New("restore cancelled")
		}
	}

	if err := tools.Restore(source, p.DBPath); err != nil {
		return err
	}
	fmt.Printf("restored %s -> %s\n", source, p.DBPath)
	return nil
}

// commonFlags are shared by every subcommand.
type commonFlags struct {
	root    string
	profile string
	verbose bool
}

func bindCommon(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.root, "root", os.Getenv("ODIN_ROOT"), "profile root directory")
	fs.StringVar(&c.profile, "profile", os.Getenv("ODIN_PROFILE"), "profile name")
	fs.BoolVar(&c.verbose, "verbose", false, "debug logging")
	return c
}

func (c *commonFlags) resolve() error {
	if c.root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		c.root = wd
	}
	abs, err := filepath.Abs(c.root)
	if err != nil {
		return err
	}
	c.root = abs

	if c.profile == "" {
		// No default profile, ever. Guessing here is how a wrong target
		// silently attaches to the wrong agent's data.
		names, err := profile.List(c.root)
		if err != nil || len(names) == 0 {
			return fmt.Errorf("--profile is required (no profiles found under %s)", c.root)
		}
		return fmt.Errorf("--profile is required (available: %s)", strings.Join(names, ", "))
	}
	return nil
}

func (c *commonFlags) logger() *slog.Logger {
	level := slog.LevelInfo
	if c.verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// load resolves flags, loads the profile, and builds its runtime.
func (c *commonFlags) load() (*profile.Runtime, *slog.Logger, error) {
	if err := c.resolve(); err != nil {
		return nil, nil, err
	}
	log := c.logger()

	p, err := profile.Load(c.root, c.profile)
	if err != nil {
		return nil, nil, err
	}
	rt, err := profile.Build(p, log)
	if err != nil {
		return nil, nil, err
	}
	return rt, log, nil
}

// loadDiagnosticProviders keeps a selected provider check independent of
// unrelated fallback credentials. Without a selection it preserves the normal
// runtime build so diagnostics still cover the configured chain.
func (c *commonFlags) loadDiagnosticProviders(name string) ([]model.Provider, func(), error) {
	if name == "" {
		rt, _, err := c.load()
		if err != nil {
			return nil, func() {}, err
		}
		providers := []model.Provider{rt.Provider}
		if chain, ok := rt.Provider.(*model.Chain); ok {
			providers = chain.Providers()
		}
		return providers, func() { _ = rt.Close() }, nil
	}

	if err := c.resolve(); err != nil {
		return nil, func() {}, err
	}
	p, err := profile.Load(c.root, c.profile)
	if err != nil {
		return nil, func() {}, err
	}
	if err := p.EnsureDirs(); err != nil {
		return nil, func() {}, err
	}
	provider, err := profile.BuildNamedProvider(p, name, c.logger())
	if err != nil {
		return nil, func() {}, err
	}
	return []model.Provider{provider}, func() {}, nil
}

// cmdInit scaffolds a new profile that loads and runs immediately.
func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	common := bindCommon(fs)
	timezone := fs.String("timezone", "", "IANA timezone, e.g. Europe/Lisbon (required)")
	schema := fs.String("db-schema", "", "optional schema.sql to apply to the new database")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// resolve() reports available profiles when --profile is missing, which is
	// unhelpful here: init is how the first one gets created.
	if common.profile == "" {
		return errors.New("--profile is required")
	}
	if common.root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		common.root = wd
	}
	if *timezone == "" {
		// Asked for, never guessed. This value defines "today" for the whole
		// agent, and a wrong zone misfiles every late-night session.
		return errors.New("--timezone is required (e.g. --timezone Europe/Lisbon)")
	}

	dir, err := profile.Scaffold(profile.ScaffoldOptions{
		Root:       common.root,
		Name:       common.profile,
		Timezone:   *timezone,
		SchemaPath: *schema,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Created profile %q at %s\n\n", common.profile, dir)
	fmt.Printf("Next:\n")
	fmt.Printf("  1. Edit %s/SOUL.md — the persona is the agent.\n", dir)
	fmt.Printf("  2. Configure the provider credential named in config.toml.\n")
	fmt.Printf("  3. odin status --root %s --profile %s\n", common.root, common.profile)
	return nil
}

// cmdRun starts the scheduler and the gateway together.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	common := bindCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	rt, log, err := common.load()
	if err != nil {
		return err
	}
	defer rt.Close()

	// SIGTERM is how systemd stops the unit; both signals drain cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tg, err := buildGateway(rt, log)
	if err != nil {
		return err
	}
	scheduler, err := buildScheduler(rt, tg, log)
	if err != nil {
		return err
	}

	log.Info("odin starting",
		"profile", rt.Profile.Name,
		"tools", rt.Tools.Names(),
		"provider", rt.Provider.Name())

	var wg sync.WaitGroup
	if scheduler != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := scheduler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("scheduler stopped", "error", err)
			}
		}()
	}
	if tg != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tg.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("gateway stopped", "error", err)
			}
		}()
	}
	if scheduler == nil && tg == nil {
		return errors.New("nothing to run: configure jobs or a telegram gateway")
	}

	wg.Wait()
	log.Info("odin stopped")
	return nil
}

// cmdOnce runs a single job immediately. This is how a scheduled prompt gets
// tested without waiting for its window, and how a missed run is replayed.
func cmdOnce(args []string) error {
	fs := flag.NewFlagSet("once", flag.ExitOnError)
	common := bindCommon(fs)
	jobName := fs.String("job", "", "job name")
	dryRun := fs.Bool("dry-run", false, "print the result instead of sending it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *jobName == "" {
		return errors.New("--job is required")
	}

	rt, log, err := common.load()
	if err != nil {
		return err
	}
	defer rt.Close()

	defs, err := jobs.Load(rt.Profile.Dir)
	if err != nil {
		return err
	}
	job, ok := defs.Find(*jobName)
	if !ok {
		return fmt.Errorf("no job %q (have: %s)", *jobName, strings.Join(defs.Names(), ", "))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	text, err := runJob(ctx, rt, job)
	if err != nil {
		return err
	}

	if *dryRun {
		fmt.Println(text)
		return nil
	}

	tg, err := buildGateway(rt, log)
	if err != nil {
		return err
	}
	if tg == nil {
		fmt.Println(text)
		return nil
	}
	return tg.Notify(ctx, rt.Profile.Config.Telegram.ChatID, text)
}

// cmdAsk runs one turn from the command line — the fastest way to check a
// profile end to end without Telegram.
func cmdAsk(args []string) error {
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	common := bindCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		return errors.New("a question is required")
	}

	rt, _, err := common.load()
	if err != nil {
		return err
	}
	defer rt.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	res, err := rt.Loop.Run(ctx, []model.Message{{Role: model.RoleUser, Content: question}})
	if res != nil && res.Text != "" {
		fmt.Println(res.Text)
	}
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\n[%s/%s · %d turns · %d tools · %d in / %d out / %d cached]\n",
		res.Provider, res.Model, res.Turns, res.ToolCalls,
		res.Usage.Input, res.Usage.Output, res.Usage.Cached)
	if limits := model.FormatRateLimitCompact(res.RateLimit); limits != "" {
		fmt.Fprintf(os.Stderr, "[limits: %s]\n", limits)
	}
	return nil
}

// cmdUsage fetches account-level subscription windows from providers that
// publish them. It never prints credentials.
func cmdUsage(args []string) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	common := bindCommon(fs)
	providerName := fs.String("provider", "", "provider name from config.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}

	providers, cleanup, err := common.loadDiagnosticProviders(*providerName)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	found := false
	for _, provider := range providers {
		name := strings.SplitN(provider.Name(), "/", 2)[0]
		if *providerName != "" && name != *providerName {
			continue
		}
		reporter, ok := provider.(model.AccountUsageReporter)
		if !ok {
			continue
		}
		snapshot, err := reporter.AccountUsage(ctx)
		if errors.Is(err, model.ErrUsageUnsupported) {
			continue
		}
		found = true
		if err != nil {
			fmt.Printf("provider  %s\nstatus    unavailable: %v\n", name, err)
			continue
		}
		printAccountUsage(snapshot)
	}
	if !found {
		if *providerName != "" {
			return fmt.Errorf("provider %q has no supported subscription usage endpoint", *providerName)
		}
		return errors.New("no configured provider has a supported subscription usage endpoint")
	}
	return nil
}

func printAccountUsage(snapshot *model.AccountUsage) {
	fmt.Printf("provider  %s\n", snapshot.Provider)
	if snapshot.Plan != "" {
		fmt.Printf("plan      %s\n", snapshot.Plan)
	}
	for _, window := range snapshot.Windows {
		remaining := 100 - window.UsedPercent
		if remaining < 0 {
			remaining = 0
		}
		fmt.Printf("%-9s %.0f%% remaining (%.0f%% used)", window.Label, remaining, window.UsedPercent)
		if !window.ResetAt.IsZero() {
			fmt.Printf(", resets %s", formatUsageReset(window.ResetAt))
		}
		fmt.Println()
	}
	for _, detail := range snapshot.Details {
		fmt.Println(detail)
	}
}

func formatUsageReset(reset time.Time) string {
	remaining := time.Until(reset)
	if remaining <= 0 {
		return "now"
	}
	remaining = remaining.Round(time.Minute)
	if remaining >= 24*time.Hour {
		return fmt.Sprintf("in %dd %dh", int(remaining/(24*time.Hour)), int(remaining%(24*time.Hour)/time.Hour))
	}
	return "in " + remaining.String()
}

// cmdModels prints the live catalog reported by each configured endpoint.
func cmdModels(args []string) error {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	common := bindCommon(fs)
	providerName := fs.String("provider", "", "provider name from config.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}

	providers, cleanup, err := common.loadDiagnosticProviders(*providerName)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	found := false
	for _, provider := range providers {
		name, current, _ := strings.Cut(provider.Name(), "/")
		if *providerName != "" && name != *providerName {
			continue
		}
		catalog, ok := provider.(model.ModelCatalog)
		if !ok {
			continue
		}
		models, err := catalog.Models(ctx)
		if errors.Is(err, model.ErrCatalogUnsupported) {
			continue
		}
		found = true
		fmt.Printf("provider  %s\n", name)
		if err != nil {
			fmt.Printf("status    unavailable: %v\n", err)
			continue
		}
		for _, id := range models {
			marker := " "
			if id == current {
				marker = "*"
			}
			fmt.Printf("%s %s\n", marker, id)
		}
	}
	if !found {
		if *providerName != "" {
			return fmt.Errorf("provider %q has no supported live model catalog", *providerName)
		}
		return errors.New("no configured provider has a supported live model catalog")
	}
	return nil
}

// cmdStatus reports configuration and health without running a turn.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	common := bindCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	rt, log, err := common.load()
	if err != nil {
		return err
	}
	defer rt.Close()

	p := rt.Profile
	fmt.Printf("profile   %s\n", p.Name)
	fmt.Printf("dir       %s\n", p.Dir)
	fmt.Printf("toolsets  %s\n", strings.Join(p.Config.Toolsets, ", "))
	fmt.Printf("tools     %s\n", strings.Join(rt.Tools.Names(), ", "))
	fmt.Printf("provider  %s\n", rt.Provider.Name())
	for _, provider := range p.Config.Providers {
		if len(provider.Accounts) > 0 {
			fmt.Printf("accounts  %s: %s\n", provider.Name, strings.Join(provider.Accounts, ", "))
		}
	}

	if rt.Store != nil {
		fmt.Printf("database   %s (timezone %s, today %s)\n",
			p.DBPath, rt.Store.Location(), rt.Store.Today())
	}
	if rt.Skills != nil {
		names, _ := rt.Skills.List()
		fmt.Printf("skills    %s\n", strings.Join(names, ", "))
	}

	// Provider health: a chain silently serving from a fallback is exactly
	// what went unnoticed before.
	if chain, ok := rt.Provider.(*model.Chain); ok {
		fmt.Println("\nproviders")
		for name, state := range chain.Status() {
			fmt.Printf("  %-28s %s\n", name, state)
		}
	}

	defs, err := jobs.Load(p.Dir)
	if err != nil {
		fmt.Printf("\njobs      (none: %v)\n", err)
		return nil
	}

	scheduler, err := buildScheduler(rt, nil, log)
	if err != nil {
		return err
	}
	if scheduler == nil {
		return nil
	}

	fmt.Println("\njobs")
	for _, h := range scheduler.Health() {
		state := "enabled"
		if !h.Enabled {
			state = "disabled"
		}
		fmt.Printf("  %-18s %-12s %-14s next %s\n",
			h.Name, h.Schedule, state, formatTime(h.NextRun))
		switch {
		case h.LastSkipped:
			fmt.Printf("  %-18s last run SKIPPED (%s)\n", "", h.LastError)
		case h.LastError != "":
			fmt.Printf("  %-18s last run FAILED at %s: %s\n", "", formatTime(h.LastRun), h.LastError)
		case !h.LastRun.IsZero():
			fmt.Printf("  %-18s last run ok at %s\n", "", formatTime(h.LastRun))
		}
	}
	_ = defs
	return nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format("2006-01-02 15:04 MST")
}

// cmdAuth runs the configured provider login and stores the token 0600.
func cmdAuth(args []string) error {
	fs := flag.NewFlagSet("auth", flag.ExitOnError)
	common := bindCommon(fs)
	providerName := fs.String("provider", "", "provider name from config.toml")
	accountName := fs.String("account", "", "named provider account")
	showStatus := fs.Bool("status", false, "show token expiry instead of logging in")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}

	p, err := profile.Load(common.root, common.profile)
	if err != nil {
		return err
	}
	if err := p.EnsureDirs(); err != nil {
		return err
	}

	var target *profile.ProviderConfig
	for i := range p.Config.Providers {
		if p.Config.Providers[i].Name == *providerName {
			target = &p.Config.Providers[i]
			break
		}
	}
	if target == nil {
		var names []string
		for _, pc := range p.Config.Providers {
			if pc.OAuth || pc.Subscription != "" && pc.Subscription != "qwen" && pc.Subscription != "kimi" {
				names = append(names, pc.Name)
			}
		}
		return fmt.Errorf("--provider is required (login providers: %s)", strings.Join(names, ", "))
	}
	if target.Subscription == "qwen" || target.Subscription == "kimi" {
		return fmt.Errorf("provider %q uses a %s plan key from %s, not OAuth", target.Name, target.Subscription, target.APIKeyEnv)
	}
	if !target.OAuth && target.Subscription == "" {
		return fmt.Errorf("provider %q uses an api key (%s), not oauth", target.Name, target.APIKeyEnv)
	}
	authPath := p.AuthPath(target.Name)
	if len(target.Accounts) > 0 {
		if *accountName == "" {
			return fmt.Errorf("--account is required for provider %q (accounts: %s)", target.Name, strings.Join(target.Accounts, ", "))
		}
		configured := false
		for _, name := range target.Accounts {
			if name == *accountName {
				configured = true
				break
			}
		}
		if !configured {
			return fmt.Errorf("unknown account %q for provider %q (accounts: %s)", *accountName, target.Name, strings.Join(target.Accounts, ", "))
		}
		authPath = p.AccountAuthPath(target.Name, *accountName)
	} else if *accountName != "" {
		return fmt.Errorf("provider %q does not configure an account pool", target.Name)
	}

	var src *model.OAuthSource
	if target.Subscription != "" {
		subscriptionSource, err := model.NewSubscriptionSource(target.Subscription, authPath)
		if err != nil {
			return err
		}
		src = subscriptionSource.(*model.OAuthSource)
	} else {
		src = model.NewOAuthSource(model.OAuthConfig{
			Path: authPath, ClientID: target.ClientID,
			TokenURL: target.TokenURL, Scope: target.Scope,
		})
	}

	if *showStatus {
		expiresIn, lastRefresh, err := src.Status()
		if err != nil {
			return err
		}
		// Never print the token itself — this output gets pasted into issues.
		fmt.Printf("provider      %s\n", target.Name)
		if *accountName != "" {
			fmt.Printf("account       %s\n", *accountName)
		}
		fmt.Printf("credentials   %s\n", authPath)
		fmt.Printf("expires in    %s\n", expiresIn.Round(time.Second))
		fmt.Printf("last refresh  %s\n", formatTime(lastRefresh))
		if expiresIn <= 0 {
			fmt.Println("\nToken is expired; it will refresh on next use.")
		}
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if target.Subscription != "" {
		fmt.Printf("Starting %s subscription login for %s", target.Subscription, target.Name)
		if *accountName != "" {
			fmt.Printf(" account %s", *accountName)
		}
		fmt.Println(".")
		reader := bufio.NewReader(os.Stdin)
		return model.LoginSubscription(ctx, target.Subscription, authPath, model.LoginUI{
			DeviceCode: func(userCode, verifyURL string) {
				fmt.Printf("\n  Open:  %s\n  Code:  %s\n\nWaiting for approval...\n", verifyURL, userCode)
			},
			AuthorizationCode: func(authorizeURL string) (string, error) {
				fmt.Printf("\n  Open: %s\n\nAfter authorizing, paste the code here: ", authorizeURL)
				code, err := reader.ReadString('\n')
				return strings.TrimSpace(code), err
			},
		})
	}

	fmt.Printf("Starting device login for %s.\n", target.Name)
	return src.DeviceLogin(ctx, target.DeviceURL, func(userCode, verifyURL string) {
		// Device flow, not a loopback redirect: the server has no browser and
		// no inbound port. Approve on a phone, the server polls.
		fmt.Printf("\n  Open:  %s\n  Code:  %s\n\nWaiting for approval...\n", verifyURL, userCode)
	})
}

// buildGateway returns nil when the profile has no telegram config.
func buildGateway(rt *profile.Runtime, log *slog.Logger) (*gateway.Telegram, error) {
	cfg := rt.Profile.Config.Telegram
	if cfg.TokenEnv == "" {
		return nil, nil
	}
	token := os.Getenv(cfg.TokenEnv)
	if token == "" {
		return nil, fmt.Errorf("%s is not set in the environment", cfg.TokenEnv)
	}
	chain := make([]string, 0, len(rt.Profile.Config.Providers))
	for _, pc := range rt.Profile.Config.Providers {
		chain = append(chain, pc.Name+"/"+pc.Model)
	}
	return gateway.NewTelegram(gateway.TelegramConfig{
		Token:        token,
		AllowedUsers: cfg.AllowedUsers,
		Agent:        loopAgent{rt.Loop},
		Logger:       log,
		ModelChain:   chain,
	})
}

// loopAgent adapts agent.Loop to the gateway's Agent interface.
type loopAgent struct{ loop *agent.Loop }

func (a loopAgent) Run(ctx context.Context, history []model.Message) (string, []model.Message, error) {
	res, err := a.loop.Run(ctx, history)
	if res == nil {
		return "", history, err
	}
	// Return res.Text even on error: a guardrail stop carries the blocker
	// message the user needs to see.
	return res.Text, res.Messages, err
}

// buildScheduler returns nil when the profile declares no jobs.
func buildScheduler(rt *profile.Runtime, tg *gateway.Telegram, log *slog.Logger) (*sched.Scheduler, error) {
	defs, err := jobs.Load(rt.Profile.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(defs.Jobs) == 0 {
		return nil, nil
	}
	if rt.Store == nil {
		// Local time is defined by the database. Without it the scheduler
		// would have to guess, and a guess misfires every job.
		return nil, errors.New("jobs require the db toolset for its timezone")
	}

	chatID := rt.Profile.Config.Telegram.ChatID

	return sched.New(sched.Config{
		Jobs:      defs.Jobs,
		Location:  rt.Store.Location(),
		Logger:    log,
		StatePath: filepath.Join(rt.Profile.Dir, "state", "scheduler.json"),
		Runner: func(ctx context.Context, job sched.Job) error {
			text, err := runJob(ctx, rt, job)
			if err != nil {
				return err
			}
			if strings.TrimSpace(text) == "" {
				// A job that decided there was nothing to say is a success, not
				// a failure — a conditional job that finds no work to report
				// should stay silent rather than send an empty message.
				log.Info("job produced no message", "job", job.Name)
				return nil
			}
			if tg == nil {
				log.Info("job output (no gateway)", "job", job.Name, "text", text)
				return nil
			}
			return tg.Notify(ctx, chatID, text)
		},
	})
}

// runJob executes one job's prompt as a single agent turn.
func runJob(ctx context.Context, rt *profile.Runtime, job sched.Job) (string, error) {
	// Jobs get their own timeout: a hung provider must not hold the slot
	// until the next day's run.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	res, err := rt.Loop.Run(ctx, []model.Message{{Role: model.RoleUser, Content: job.Prompt}})
	if err != nil {
		if res != nil && res.Text != "" {
			return res.Text, err
		}
		return "", err
	}
	return res.Text, nil
}
