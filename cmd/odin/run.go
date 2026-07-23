package main

import (
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
)

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
	chatID := rt.Profile.Config.Telegram.ChatID

	return sched.New(sched.Config{
		Jobs:      defs.Jobs,
		Location:  rt.Location,
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

	prompt, err := buildJobPrompt(rt, job)
	if err != nil {
		return "", err
	}

	res, err := rt.Loop.Run(ctx, []model.Message{{Role: model.RoleUser, Content: prompt}})
	if err != nil {
		if res != nil && res.Text != "" {
			return res.Text, err
		}
		return "", err
	}
	return res.Text, nil
}

// buildJobPrompt resolves declared skill dependencies before the model call.
// This makes jobs deterministic and avoids spending a turn hoping the model
// notices an advisory skill name in the catalog.
func buildJobPrompt(rt *profile.Runtime, job sched.Job) (string, error) {
	if len(job.Skills) == 0 {
		return job.Prompt, nil
	}
	if rt.Skills == nil {
		return "", fmt.Errorf("job %q declares skills but the skills toolset is disabled", job.Name)
	}

	var b strings.Builder
	b.WriteString("The following skill documents are required for this job. Follow their procedures.\n")
	for _, name := range job.Skills {
		body, err := rt.Skills.Read(name)
		if err != nil {
			return "", fmt.Errorf("job %q: load skill %q: %w", job.Name, name, err)
		}
		fmt.Fprintf(&b, "\n## Required skill: %s\n\n%s\n", name, strings.TrimSpace(body))
	}
	b.WriteString("\n## Job\n\n")
	b.WriteString(job.Prompt)
	return b.String(), nil
}
