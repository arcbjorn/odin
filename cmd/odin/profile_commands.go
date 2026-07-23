package main

import (
	"errors"
	"flag"
	"fmt"

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
