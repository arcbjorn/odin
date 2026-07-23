package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/arcbjorn/odin/jobs"
	"github.com/arcbjorn/odin/model"
	"github.com/arcbjorn/odin/profile"
)

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

	zone, source, _ := p.Timezone()
	fmt.Printf("timezone  %s (%s)\n", zone, source)
	if rt.Store != nil {
		fmt.Printf("database  %s (today %s)\n", p.DBPath, rt.Store.Today())
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
