// Package watchdog reports when the agent has gone silently dead.
//
// Deliberately dumber than what it watches. It reads state files on disk and
// posts to the Telegram Bot API directly — no model call, no provider, no
// agent loop, no database. Everything it could share with the agent is a way
// they could fail together, and a watchdog that dies with the thing it
// watches is worse than none: it converts a visible outage into a silent one.
//
// The failure mode this exists for is silence. An unattended agent that stops
// speaking looks exactly like a quiet day.
//
// It does not self-heal or second-guess the scheduler's bookkeeping: a
// recorded job error is either still live or overwritten by that job's next
// run, so there is nothing to reconcile. It reports what it reads.
package watchdog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Finding is one problem worth waking someone for.
type Finding struct {
	Job     string
	Problem string
}

func (f Finding) String() string {
	if f.Job == "" {
		return f.Problem
	}
	return f.Job + ": " + f.Problem
}

// Config configures a check.
type Config struct {
	// ProfileDir is the agent's profile directory. The watchdog reads its
	// state; it never writes there.
	ProfileDir string

	// StateDir holds the watchdog's own alert-dedupe state, separate from the
	// agent's so a wiped profile does not resurrect old alerts.
	StateDir string

	// Token and ChatID address Telegram directly. Not reusing the agent's
	// gateway is the point: if that code path is broken, this one still runs.
	Token  string
	ChatID int64

	// Overdue is how far past a scheduled run counts as stalled. Long enough
	// that a slow job in progress is not mistaken for a dead one.
	Overdue time.Duration

	// Realert suppresses an identical alert for this long, so a persistent
	// fault does not become a notification every 30 minutes.
	Realert time.Duration

	// DryRun prints instead of sending.
	DryRun bool

	// now is swappable for tests.
	now func() time.Time
}

// Watchdog performs one check per invocation. It holds no long-lived state:
// a systemd timer runs it, it exits. A resident daemon could hang the same
// way the agent might.
type Watchdog struct {
	cfg  Config
	http *http.Client
}

// New builds a Watchdog.
func New(cfg Config) (*Watchdog, error) {
	if cfg.ProfileDir == "" {
		return nil, fmt.Errorf("profile dir is required")
	}
	if !cfg.DryRun {
		if cfg.Token == "" {
			return nil, fmt.Errorf("telegram token is required (use --dry-run to test without one)")
		}
		if cfg.ChatID == 0 {
			return nil, fmt.Errorf("chat id is required")
		}
	}
	if cfg.Overdue == 0 {
		cfg.Overdue = 45 * time.Minute
	}
	if cfg.Realert == 0 {
		cfg.Realert = 6 * time.Hour
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Join(os.TempDir(), "odin-watchdog")
	}
	return &Watchdog{cfg: cfg, http: &http.Client{Timeout: 20 * time.Second}}, nil
}

// schedulerState mirrors what sched writes. Decoded structurally rather than
// by importing the type, so a change to the agent's internals surfaces here
// as a missing field instead of silently coupling the two.
type schedulerState map[string]struct {
	At       time.Time `json:"at"`
	Timezone string    `json:"timezone,omitempty"`
	Error    string    `json:"error,omitempty"`
	Skipped  bool      `json:"skipped,omitempty"`
}

// jobManifest is the minimum needed to know what should have run.
type jobManifest struct {
	Name     string
	Schedule string
	Enabled  bool
}

// Check inspects the agent and returns anything wrong. An empty result means
// healthy — and healthy is silent.
func (w *Watchdog) Check() ([]Finding, error) {
	var findings []Finding
	now := w.cfg.now()

	manifests, err := readManifest(filepath.Join(w.cfg.ProfileDir, "jobs", "jobs.toml"))
	if err != nil {
		// No jobs configured is a valid setup, not a fault.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read job manifest: %w", err)
	}

	statePath := filepath.Join(w.cfg.ProfileDir, "state", "scheduler.json")
	state, err := readState(statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read scheduler state: %w", err)
		}
		// No state at all means nothing has ever run. That is a fault the
		// moment any job is enabled — a fresh install that never started.
		for _, m := range manifests {
			if m.Enabled {
				return []Finding{{
					Problem: "no scheduler state at " + statePath + "; the agent may never have started",
				}}, nil
			}
		}
		return nil, nil
	}

	for _, m := range manifests {
		if !m.Enabled {
			continue
		}
		rec, ran := state[m.Name]

		if !ran {
			findings = append(findings, Finding{
				Job:     m.Name,
				Problem: "has never run",
			})
			continue
		}

		// A recorded error is live: the next successful run overwrites it.
		if rec.Error != "" {
			verb := "failed"
			if rec.Skipped {
				verb = "was skipped"
			}
			findings = append(findings, Finding{
				Job:     m.Name,
				Problem: fmt.Sprintf("%s at %s: %s", verb, rec.At.Format(time.RFC3339), rec.Error),
			})
			continue
		}

		// The core check: a job whose schedule has come and gone without a
		// run. This is what silence looks like from the outside.
		after := rec.At
		if rec.Timezone != "" {
			loc, err := time.LoadLocation(rec.Timezone)
			if err != nil {
				findings = append(findings, Finding{
					Job:     m.Name,
					Problem: fmt.Sprintf("invalid recorded timezone %q", rec.Timezone),
				})
				continue
			}
			after = rec.At.In(loc)
		}
		due, err := nextDue(m.Schedule, after)
		if err != nil {
			findings = append(findings, Finding{
				Job:     m.Name,
				Problem: "unparseable schedule " + m.Schedule,
			})
			continue
		}
		if late := now.Sub(due); late > w.cfg.Overdue {
			findings = append(findings, Finding{
				Job: m.Name,
				Problem: fmt.Sprintf("overdue by %s (expected %s, last ran %s)",
					late.Round(time.Minute), due.Format(time.RFC3339), rec.At.Format(time.RFC3339)),
			})
		}
	}

	sort.Slice(findings, func(i, j int) bool { return findings[i].Job < findings[j].Job })
	return findings, nil
}

// Run performs a check and alerts if needed. Healthy produces no output and
// sends nothing.
func (w *Watchdog) Run(ctx context.Context) error {
	findings, err := w.Check()
	if err != nil {
		// A watchdog that cannot check is itself a fault worth reporting.
		return w.alert(ctx, []Finding{{Problem: "watchdog could not run: " + err.Error()}})
	}
	if len(findings) == 0 {
		return nil
	}
	return w.alert(ctx, findings)
}

func (w *Watchdog) alert(ctx context.Context, findings []Finding) error {
	lines := make([]string, 0, len(findings))
	for _, f := range findings {
		lines = append(lines, "- "+f.String())
	}
	body := "Odin is not healthy:\n" + strings.Join(lines, "\n")

	// Dedupe on content: a persistent fault repeats the same text, and
	// re-sending it every 30 minutes trains the user to ignore the alert.
	fresh, err := w.shouldSend(body)
	if err != nil {
		return err
	}
	if !fresh {
		return nil
	}

	if w.cfg.DryRun {
		fmt.Println(body)
		return nil
	}
	return w.send(ctx, body)
}

// shouldSend reports whether this alert text is new enough to send, and
// records it when it is.
func (w *Watchdog) shouldSend(body string) (bool, error) {
	if err := os.MkdirAll(w.cfg.StateDir, 0o700); err != nil {
		return false, err
	}
	path := filepath.Join(w.cfg.StateDir, "last-alert.json")

	type record struct {
		Body string    `json:"body"`
		At   time.Time `json:"at"`
	}

	var last record
	if raw, err := os.ReadFile(path); err == nil {
		// A corrupt dedupe file must not suppress an alert; on any parse
		// failure we fall through and send.
		_ = json.Unmarshal(raw, &last)
	}

	now := w.cfg.now()
	if last.Body == body && now.Sub(last.At) < w.cfg.Realert {
		return false, nil
	}

	raw, err := json.Marshal(record{Body: body, At: now})
	if err != nil {
		return true, nil // sending matters more than recording
	}
	// Best effort: failing to persist dedupe state is not a reason to stay
	// silent about a real fault.
	_ = os.WriteFile(path, raw, 0o600)
	return true, nil
}

func (w *Watchdog) send(ctx context.Context, text string) error {
	params := url.Values{
		"chat_id": {fmt.Sprint(w.cfg.ChatID)},
		"text":    {text},
	}
	endpoint := "https://api.telegram.org/bot" + w.cfg.Token + "/sendMessage"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := w.http.Do(req)
	if err != nil {
		return fmt.Errorf("send alert: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The token is in the URL, so never echo the endpoint.
		return fmt.Errorf("telegram returned http %d", resp.StatusCode)
	}
	return nil
}

func readState(path string) (schedulerState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state schedulerState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("corrupt state file: %w", err)
	}
	return state, nil
}
