package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/arcbjorn/odin/profile"
)

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	common := bindCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}
	report, err := profile.Validate(common.root, common.profile)
	if err != nil {
		return err
	}
	fmt.Printf("profile     %s\n", report.Profile.Name)
	fmt.Printf("system      %d files\n", report.SystemFiles)
	fmt.Printf("timezone    %s (%s)\n", report.Timezone, report.TimeSource)
	fmt.Printf("skills      %d\n", report.Skills)
	fmt.Printf("jobs        %d\n", report.Jobs)
	fmt.Printf("migrations  %d\n", report.Migrations)
	fmt.Println("status      valid")
	return nil
}

// cmdModel reads or changes the interactive model override — the CLI half of
// Telegram's /model. It selects only among providers already in config.toml;
// adding a provider is a config edit and `odin auth`, not a wizard.
func cmdModel(args []string) error {
	fs := flag.NewFlagSet("model", flag.ExitOnError)
	common := bindCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	rt, _, err := common.load()
	if err != nil {
		return err
	}
	defer rt.Close()

	positionals := fs.Args()
	action := "get"
	if len(positionals) > 0 {
		action = positionals[0]
	}

	switch action {
	case "get":
		if len(positionals) > 1 {
			return errors.New("usage: odin model [get|set TARGET|reset]")
		}
		target, overridden := rt.Switcher.Current()
		source := "config"
		if overridden {
			source = "runtime override"
		}
		fmt.Printf("%s (%s)\n", target, source)
		for _, entry := range rt.Switcher.Configured() {
			fmt.Printf("configured  %s\n", entry)
		}
		return nil

	case "set":
		if len(positionals) != 2 {
			return errors.New("model set requires a target, e.g. openai/gpt-5.6-terra")
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		change, err := rt.Switcher.Switch(ctx, positionals[1])
		if err != nil {
			return err
		}
		fmt.Printf("model     %s\n", change.Target)
		fmt.Printf("was       %s\n", change.Previous)
		fmt.Printf("resolved  %s\n", change.ResolvedVia)
		if change.Warning != "" {
			fmt.Printf("warning   %s\n", change.Warning)
		}
		// A running agent reads this at startup, like the timezone override.
		fmt.Println("\nApplies to chat turns only; scheduled jobs keep the configured model.")
		fmt.Println("Restart Odin for a running agent to pick this up.")
		return nil

	case "reset":
		if len(positionals) != 1 {
			return errors.New("model reset takes no target")
		}
		target, err := rt.Switcher.Reset()
		if err != nil {
			return err
		}
		fmt.Printf("model reset to %s from config; restart Odin to apply to a running agent\n", target)
		return nil

	default:
		return fmt.Errorf("unknown model action %q (want get, set, or reset)", action)
	}
}

func cmdTimezone(args []string) error {
	fs := flag.NewFlagSet("timezone", flag.ExitOnError)
	common := bindCommon(fs)
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

	positionals := fs.Args()
	action := "get"
	if len(positionals) > 0 {
		action = positionals[0]
	}
	switch action {
	case "get":
		if len(positionals) != 0 && len(positionals) != 1 {
			return errors.New("usage: odin timezone [get|set ZONE|reset]")
		}
		name, source, err := p.Timezone()
		if err != nil {
			return err
		}
		fmt.Printf("%s (%s)\n", name, source)
		return nil
	case "set":
		if len(positionals) != 2 {
			return errors.New("timezone set requires an IANA zone")
		}
		if err := p.SetTimezone(positionals[1]); err != nil {
			return err
		}
		fmt.Printf("timezone override set to %s; restart Odin to reschedule jobs\n", positionals[1])
		return nil
	case "reset":
		if len(positionals) != 1 {
			return errors.New("timezone reset takes no zone")
		}
		if err := p.SetTimezone(""); err != nil {
			return err
		}
		fmt.Printf("timezone reset to %s from config; restart Odin to reschedule jobs\n", p.Config.Timezone)
		return nil
	default:
		return fmt.Errorf("unknown timezone action %q (want get, set, or reset)", action)
	}
}
