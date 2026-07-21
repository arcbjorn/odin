// Command odin-watchdog reports when the agent has gone silently dead.
//
// A separate binary, not an `odin` subcommand. The whole value of a watchdog
// is that it cannot fail the same way as the thing it watches: it shares no
// provider, no agent loop, no database, and no config parser with the agent.
// Folding it into the same binary would give them a common failure mode.
//
// Run from a systemd timer every 30 minutes:
//
//	odin-watchdog --profile-dir /var/lib/odin/profiles/personal
//
// Healthy is silent — no output, no message, exit 0.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/arcbjorn/odin/watchdog"
)

func main() {
	profileDir := flag.String("profile-dir", os.Getenv("ODIN_PROFILE_DIR"),
		"the agent profile directory to watch")
	stateDir := flag.String("state-dir", "/var/lib/odin-watchdog",
		"where to keep alert-dedupe state")
	overdue := flag.Duration("overdue", 45*time.Minute,
		"how far past a scheduled run counts as stalled")
	realert := flag.Duration("realert", 6*time.Hour,
		"suppress an identical alert for this long")
	dryRun := flag.Bool("dry-run", false, "print findings instead of sending them")
	check := flag.Bool("check", false, "print findings and exit non-zero if any (for scripts)")
	flag.Parse()

	if *profileDir == "" {
		fmt.Fprintln(os.Stderr, "odin-watchdog: --profile-dir is required")
		os.Exit(2)
	}

	// Credentials come from the environment, never from a flag: a flag would
	// put the bot token in the process list for every user on the box.
	token := os.Getenv("TELEGRAM_TOKEN")
	var chatID int64
	if raw := os.Getenv("TELEGRAM_CHAT_ID"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "odin-watchdog: invalid TELEGRAM_CHAT_ID: %v\n", err)
			os.Exit(2)
		}
		chatID = id
	}

	w, err := watchdog.New(watchdog.Config{
		ProfileDir: *profileDir,
		StateDir:   *stateDir,
		Token:      token,
		ChatID:     chatID,
		Overdue:    *overdue,
		Realert:    *realert,
		DryRun:     *dryRun || *check,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "odin-watchdog:", err)
		os.Exit(2)
	}

	// --check is for humans and scripts: report findings on stdout and signal
	// through the exit code, without touching dedupe state or sending.
	if *check {
		findings, err := w.Check()
		if err != nil {
			fmt.Fprintln(os.Stderr, "odin-watchdog:", err)
			os.Exit(2)
		}
		if len(findings) == 0 {
			fmt.Println("healthy")
			return
		}
		for _, f := range findings {
			fmt.Println(f)
		}
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := w.Run(ctx); err != nil {
		// Failing to deliver an alert is itself worth a non-zero exit, so the
		// timer's own failure shows up in journalctl.
		fmt.Fprintln(os.Stderr, "odin-watchdog:", err)
		os.Exit(1)
	}
}
