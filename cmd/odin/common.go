package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/arcbjorn/odin/model"
	"github.com/arcbjorn/odin/profile"
)

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
