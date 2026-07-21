package profile

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ScaffoldOptions configures a new profile.
type ScaffoldOptions struct {
	Root string
	Name string

	// Timezone seeds the tracker's settings table. This is the value that
	// defines "today" for the whole agent, so it is asked for rather than
	// guessed — a wrong zone misfiles every late-night session.
	Timezone string

	// TrackerSchema is an optional path to a schema.sql to apply. When empty,
	// a minimal schema is created that only carries the timezone setting.
	TrackerSchema string
}

// Scaffold creates a profile directory that loads and runs.
//
// The point is that `odin init` produces something `odin status` accepts
// immediately. A scaffold that needs three undocumented edits before it works
// is a README pretending to be a command.
func Scaffold(opts ScaffoldOptions) (string, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return "", fmt.Errorf("profile name is required")
	}
	if strings.ContainsAny(opts.Name, `/\`) || strings.Contains(opts.Name, "..") {
		return "", fmt.Errorf("invalid profile name %q", opts.Name)
	}
	if opts.Timezone == "" {
		return "", fmt.Errorf("timezone is required; it defines what \"today\" means for this agent")
	}
	if _, err := time.LoadLocation(opts.Timezone); err != nil {
		return "", fmt.Errorf("unknown timezone %q: %w", opts.Timezone, err)
	}

	dir := filepath.Join(opts.Root, "profiles", opts.Name)
	if _, err := os.Stat(dir); err == nil {
		// Never overwrite: the directory holds a tracker database and OAuth
		// tokens, and clobbering either is unrecoverable.
		return "", fmt.Errorf("profile %q already exists at %s", opts.Name, dir)
	}

	for _, sub := range []string{"", "jobs", "skills", "notes", "auth", "state"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return "", fmt.Errorf("create %s: %w", sub, err)
		}
	}

	files := map[string]string{
		"config.toml": scaffoldConfig(opts),
		"SOUL.md":     scaffoldSoul(),
		".gitignore":  scaffoldGitignore(),
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return "", fmt.Errorf("write %s: %w", name, err)
		}
	}

	if err := scaffoldTracker(filepath.Join(dir, "tracker.db"), opts); err != nil {
		return "", err
	}
	return dir, nil
}

func scaffoldTracker(path string, opts ScaffoldOptions) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("create tracker: %w", err)
	}
	defer db.Close()

	// WAL lets the agent and inspection tools read concurrently.
	if _, err := db.Exec(`PRAGMA journal_mode = WAL;`); err != nil {
		return fmt.Errorf("enable wal: %w", err)
	}

	if opts.TrackerSchema != "" {
		raw, err := os.ReadFile(opts.TrackerSchema)
		if err != nil {
			return fmt.Errorf("read schema %s: %w", opts.TrackerSchema, err)
		}
		if _, err := db.Exec(string(raw)); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
	} else {
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY, value TEXT);`); err != nil {
			return fmt.Errorf("create settings: %w", err)
		}
	}

	// The timezone must exist: NewSQLite refuses to start without it rather
	// than defaulting to UTC.
	if _, err := db.Exec(
		`INSERT OR REPLACE INTO settings(key, value) VALUES ('timezone', ?)`,
		opts.Timezone,
	); err != nil {
		return fmt.Errorf("seed timezone: %w", err)
	}
	return nil
}

func scaffoldConfig(opts ScaffoldOptions) string {
	return fmt.Sprintf(`# Profile: %s
# Credentials belong in environment variables, never in this file.
toolsets = ["tracker", "file"]
timezone = "%s"

[agent]
max_turns = 20
max_tokens = 4096
effort = "high"

[[providers]]
kind = "openai"
name = "openai"
model = "gpt-5.4"
base_url = "https://api.openai.com/v1"
api_key_env = "OPENAI_API_KEY"

# Optional Telegram gateway. Replace the placeholder before enabling it.
# [telegram]
# token_env = "TELEGRAM_TOKEN"
# allowed_users = [123456789]
`, opts.Name, opts.Timezone)
}

func scaffoldSoul() string {
	return `# General assistant

You are a practical, privacy-conscious assistant. Follow the user's requests,
use configured tools only when needed, and ground factual claims in data you
actually read. Never invent missing records or user preferences. Ask a
short clarifying question only when a consequential detail cannot be inferred.
Keep responses concise and adapt their tone and detail to the user.
`
}

func scaffoldGitignore() string {
	return `# Credentials and live state. The config and prompts are meant to be
# committed; everything below is machine-local or secret.
auth/
state/
notes/
tracker.db
tracker.db-shm
tracker.db-wal
*.env
`
}
