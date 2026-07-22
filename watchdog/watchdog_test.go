package watchdog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fourJobs = `
[[job]]
name = "Daily report"
schedule = "0 7 * * *"
prompt = "morning-brief.md"

[[job]]
name = "Nightly summary"
schedule = "30 21 * * *"
prompt = "evening-close.md"

[[job]]
name = "Hourly check"
schedule = "30 22 * * *"
prompt = "night-guard.md"

[[job]]
name = "Weekly rollup"
schedule = "0 18 * * 0"
prompt = "weekly-debrief.md"
enabled = false
`

type stateEntry struct {
	At       time.Time `json:"at"`
	Timezone string    `json:"timezone,omitempty"`
	Error    string    `json:"error,omitempty"`
	Skipped  bool      `json:"skipped,omitempty"`
}

// newProfile lays out just what the watchdog reads: the job manifest and the
// scheduler's state file.
func newProfile(t *testing.T, manifest string, state map[string]stateEntry) string {
	t.Helper()
	dir := t.TempDir()

	if manifest != "" {
		jobsDir := filepath.Join(dir, "jobs")
		if err := os.MkdirAll(jobsDir, 0o700); err != nil {
			t.Fatalf("mkdir jobs: %v", err)
		}
		if err := os.WriteFile(filepath.Join(jobsDir, "jobs.toml"), []byte(manifest), 0o600); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
	if state != nil {
		stateDir := filepath.Join(dir, "state")
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatalf("mkdir state: %v", err)
		}
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("marshal state: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "scheduler.json"), raw, 0o600); err != nil {
			t.Fatalf("write state: %v", err)
		}
	}
	return dir
}

func newWatchdog(t *testing.T, profileDir string, now time.Time) *Watchdog {
	t.Helper()
	w, err := New(Config{
		ProfileDir: profileDir,
		StateDir:   t.TempDir(),
		DryRun:     true,
		now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

// Healthy is silent. An alert on a working system trains the user to ignore
// alerts, which is the same outcome as having none.
func TestHealthySystemIsSilent(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	dir := newProfile(t, fourJobs, map[string]stateEntry{
		"Daily report":    {At: now.Add(-5 * time.Hour)}, // ran at 07:00
		"Nightly summary": {At: now.Add(-14*time.Hour - 30*time.Minute)},
		"Hourly check":    {At: now.Add(-13*time.Hour - 30*time.Minute)},
	})

	findings, err := newWatchdog(t, dir, now).Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("healthy system produced findings: %v", findings)
	}
}

// The core case: the daily report's window came and went with no run. This
// is what a silently dead agent looks like from outside.
func TestOverdueJobIsCaught(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	dir := newProfile(t, fourJobs, map[string]stateEntry{
		// Last ran yesterday morning; today's 07:00 never happened.
		"Daily report":    {At: time.Date(2026, 7, 19, 7, 0, 0, 0, time.UTC)},
		"Nightly summary": {At: now.Add(-14*time.Hour - 30*time.Minute)},
		"Hourly check":    {At: now.Add(-13*time.Hour - 30*time.Minute)},
	})

	findings, err := newWatchdog(t, dir, now).Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %v", findings)
	}
	if findings[0].Job != "Daily report" || !strings.Contains(findings[0].Problem, "overdue") {
		t.Fatalf("wrong finding: %+v", findings[0])
	}
}

// A recorded error is live: the next successful run overwrites it. There is
// no drift to reconcile, unlike the Hermes watchdog which had to model the
// scheduler's spend-guard bookkeeping to avoid crying about fixed failures.
func TestFailedRunIsReported(t *testing.T) {
	now := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	dir := newProfile(t, fourJobs, map[string]stateEntry{
		"Daily report":    {At: now.Add(-time.Hour), Error: "all 3 providers failed"},
		"Nightly summary": {At: now.Add(-10*time.Hour - 30*time.Minute)},
		"Hourly check":    {At: now.Add(-9*time.Hour - 30*time.Minute)},
	})

	findings, err := newWatchdog(t, dir, now).Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %v", findings)
	}
	if !strings.Contains(findings[0].Problem, "all 3 providers failed") {
		t.Fatalf("the underlying error must reach the alert: %+v", findings[0])
	}
}

// A skipped stale run is a real signal: the process was down over its window.
func TestSkippedRunIsReported(t *testing.T) {
	now := time.Date(2026, 7, 20, 16, 0, 0, 0, time.UTC)
	dir := newProfile(t, fourJobs, map[string]stateEntry{
		"Daily report":    {At: now.Add(-time.Hour), Skipped: true, Error: "skipped: 8h0m late"},
		"Nightly summary": {At: now.Add(-18*time.Hour - 30*time.Minute)},
		"Hourly check":    {At: now.Add(-17*time.Hour - 30*time.Minute)},
	})

	findings, err := newWatchdog(t, dir, now).Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0].Problem, "skipped") {
		t.Fatalf("skip not surfaced: %v", findings)
	}
}

// A disabled job is a decision, not a fault.
func TestDisabledJobIsNotReported(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	dir := newProfile(t, fourJobs, map[string]stateEntry{
		"Daily report":    {At: now.Add(-5 * time.Hour)},
		"Nightly summary": {At: now.Add(-14*time.Hour - 30*time.Minute)},
		"Hourly check":    {At: now.Add(-13*time.Hour - 30*time.Minute)},
		// Weekly rollup is enabled = false and has never run.
	})

	findings, err := newWatchdog(t, dir, now).Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for _, f := range findings {
		if f.Job == "Weekly rollup" {
			t.Fatalf("disabled job reported as a fault: %+v", f)
		}
	}
}

// A job present in the manifest but absent from state has never fired.
func TestNeverRunJobIsCaught(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	dir := newProfile(t, fourJobs, map[string]stateEntry{
		"Daily report":    {At: now.Add(-5 * time.Hour)},
		"Nightly summary": {At: now.Add(-14*time.Hour - 30*time.Minute)},
		// Hourly check missing entirely.
	})

	findings, err := newWatchdog(t, dir, now).Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 1 || findings[0].Job != "Hourly check" {
		t.Fatalf("expected Hourly check flagged, got %v", findings)
	}
	if !strings.Contains(findings[0].Problem, "never run") {
		t.Fatalf("wrong problem: %+v", findings[0])
	}
}

// No state file at all with jobs enabled means the agent never started —
// the loudest possible version of silence.
func TestMissingStateWithEnabledJobsIsCaught(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	dir := newProfile(t, fourJobs, nil)

	findings, err := newWatchdog(t, dir, now).Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0].Problem, "never have started") {
		t.Fatalf("expected a never-started finding, got %v", findings)
	}
}

// No jobs configured is a valid setup, not a fault.
func TestNoManifestIsNotAFault(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	dir := newProfile(t, "", nil)

	findings, err := newWatchdog(t, dir, now).Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("a profile with no jobs should be silent, got %v", findings)
	}
}

// A job still inside its grace window is not yet a fault: a slow run in
// progress must not be mistaken for a dead one.
func TestJobWithinGraceWindowIsNotReported(t *testing.T) {
	// 07:20 — the 07:00 brief is 20 minutes late, under the 45m threshold.
	now := time.Date(2026, 7, 20, 7, 20, 0, 0, time.UTC)
	dir := newProfile(t, fourJobs, map[string]stateEntry{
		"Daily report":    {At: time.Date(2026, 7, 19, 7, 0, 0, 0, time.UTC)},
		"Nightly summary": {At: time.Date(2026, 7, 19, 21, 30, 0, 0, time.UTC)},
		"Hourly check":    {At: time.Date(2026, 7, 19, 22, 30, 0, 0, time.UTC)},
	})

	findings, err := newWatchdog(t, dir, now).Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("a job inside the grace window should not alert: %v", findings)
	}
}

// A persistent fault must not become a notification every 30 minutes.
func TestIdenticalAlertIsDeduped(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	dir := newProfile(t, fourJobs, map[string]stateEntry{
		"Daily report":    {At: now.Add(-time.Hour), Error: "provider down"},
		"Nightly summary": {At: now.Add(-14*time.Hour - 30*time.Minute)},
		"Hourly check":    {At: now.Add(-13*time.Hour - 30*time.Minute)},
	})

	stateDir := t.TempDir()
	build := func(at time.Time) *Watchdog {
		w, err := New(Config{
			ProfileDir: dir, StateDir: stateDir, DryRun: true,
			now: func() time.Time { return at },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return w
	}

	body := "Odin is not healthy:\n- Daily report: failed"

	if send, _ := build(now).shouldSend(body); !send {
		t.Fatal("first alert should send")
	}
	if send, _ := build(now.Add(30 * time.Minute)).shouldSend(body); send {
		t.Fatal("identical alert 30 minutes later should be suppressed")
	}
	// After the re-alert window it must speak again: the fault is still real.
	if send, _ := build(now.Add(7 * time.Hour)).shouldSend(body); !send {
		t.Fatal("alert should repeat after the re-alert window")
	}
}

// A different fault must not be suppressed by an earlier one.
func TestDifferentAlertIsNotDeduped(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	dir := newProfile(t, fourJobs, nil)
	stateDir := t.TempDir()

	build := func() *Watchdog {
		w, _ := New(Config{
			ProfileDir: dir, StateDir: stateDir, DryRun: true,
			now: func() time.Time { return now },
		})
		return w
	}

	if send, _ := build().shouldSend("fault A"); !send {
		t.Fatal("first alert should send")
	}
	if send, _ := build().shouldSend("fault B"); !send {
		t.Fatal("a different fault must not be suppressed")
	}
}

// A corrupt dedupe file must not silence a real alert.
func TestCorruptDedupeStateStillAlerts(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	dir := newProfile(t, fourJobs, nil)
	stateDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(stateDir, "last-alert.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w, _ := New(Config{
		ProfileDir: dir, StateDir: stateDir, DryRun: true,
		now: func() time.Time { return now },
	})
	if send, _ := w.shouldSend("real fault"); !send {
		t.Fatal("a corrupt dedupe file must not suppress an alert")
	}
}

// A corrupt scheduler state file is itself a fault worth reporting, not a
// reason to exit quietly.
func TestCorruptSchedulerStateIsReported(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	dir := newProfile(t, fourJobs, nil)

	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "scheduler.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := newWatchdog(t, dir, now).Check(); err == nil {
		t.Fatal("corrupt scheduler state should surface as an error")
	}

	// Run must convert that error into an alert rather than swallowing it.
	w := newWatchdog(t, dir, now)
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run should report, not fail: %v", err)
	}
}

func TestUnparseableScheduleIsReported(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	manifest := `
[[job]]
name = "Broken"
schedule = "99 7 * * *"
`
	dir := newProfile(t, manifest, map[string]stateEntry{
		"Broken": {At: now.Add(-time.Hour)},
	})

	findings, err := newWatchdog(t, dir, now).Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0].Problem, "unparseable") {
		t.Fatalf("expected an unparseable-schedule finding, got %v", findings)
	}
}

// The watchdog must not need a token to be testable — dry run is how the
// operator verifies it before trusting it.
func TestDryRunNeedsNoCredentials(t *testing.T) {
	if _, err := New(Config{ProfileDir: t.TempDir(), DryRun: true}); err != nil {
		t.Fatalf("dry run should not require a token: %v", err)
	}
	if _, err := New(Config{ProfileDir: t.TempDir()}); err == nil {
		t.Fatal("a live watchdog must require a token")
	}
}

func TestManifestParsing(t *testing.T) {
	dir := newProfile(t, fourJobs, nil)
	jobs, err := readManifest(filepath.Join(dir, "jobs", "jobs.toml"))
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if len(jobs) != 4 {
		t.Fatalf("expected 4 jobs, got %d", len(jobs))
	}

	byName := map[string]jobManifest{}
	for _, j := range jobs {
		byName[j.Name] = j
	}
	if !byName["Daily report"].Enabled {
		t.Error("enabled should default to true")
	}
	if byName["Weekly rollup"].Enabled {
		t.Error("enabled = false not honored")
	}
	if byName["Daily report"].Schedule != "0 7 * * *" {
		t.Errorf("schedule = %q", byName["Daily report"].Schedule)
	}
}

// The watchdog's own cron parser must agree with the agent's on the four real
// schedules; a disagreement would produce phantom alerts.
func TestNextDueMatchesRealSchedules(t *testing.T) {
	loc, err := time.LoadLocation("Etc/GMT+3")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}

	cases := []struct {
		expr string
		from time.Time
		want time.Time
	}{
		{"0 7 * * *", time.Date(2026, 7, 20, 6, 30, 0, 0, loc), time.Date(2026, 7, 20, 7, 0, 0, 0, loc)},
		{"30 21 * * *", time.Date(2026, 7, 20, 12, 0, 0, 0, loc), time.Date(2026, 7, 20, 21, 30, 0, 0, loc)},
		{"30 22 * * *", time.Date(2026, 7, 20, 23, 0, 0, 0, loc), time.Date(2026, 7, 21, 22, 30, 0, 0, loc)},
		// 2026-07-20 is a Monday; the next Sunday is the 26th.
		{"0 18 * * 0", time.Date(2026, 7, 20, 12, 0, 0, 0, loc), time.Date(2026, 7, 26, 18, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		got, err := nextDue(c.expr, c.from)
		if err != nil {
			t.Errorf("%q: %v", c.expr, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("%q from %s: got %s, want %s", c.expr, c.from, got, c.want)
		}
	}
}

// Overdue detection must use local wall-clock time, so a traveling user does
// not get phantom alerts from a timezone shift.
func TestOverdueUsesLocalTime(t *testing.T) {
	loc, err := time.LoadLocation("Etc/GMT+3")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}

	// Last ran 07:00 local yesterday; now 12:00 local today. Today's 07:00
	// is five hours past due.
	lastRun := time.Date(2026, 7, 19, 7, 0, 0, 0, loc)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, loc)

	due, err := nextDue("0 7 * * *", lastRun)
	if err != nil {
		t.Fatalf("nextDue: %v", err)
	}
	if late := now.Sub(due); late < 4*time.Hour {
		t.Fatalf("expected roughly 5h overdue, got %s", late)
	}
}

// Serialized time.Time values retain an offset but lose their IANA location.
// The recorded zone must restore DST rules or the watchdog can be one hour
// late exactly when clocks change.
func TestOverdueUsesRecordedTimezoneAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}

	lastRun := time.Date(2026, 3, 7, 7, 0, 0, 0, loc)
	now := time.Date(2026, 3, 8, 8, 0, 0, 0, loc)
	dir := newProfile(t, fourJobs, map[string]stateEntry{
		"Daily report":    {At: lastRun, Timezone: loc.String()},
		"Nightly summary": {At: time.Date(2026, 3, 7, 21, 30, 0, 0, loc), Timezone: loc.String()},
		"Hourly check":    {At: time.Date(2026, 3, 7, 22, 30, 0, 0, loc), Timezone: loc.String()},
	})

	findings, err := newWatchdog(t, dir, now).Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 1 || findings[0].Job != "Daily report" || !strings.Contains(findings[0].Problem, "overdue") {
		t.Fatalf("expected DST-aware overdue finding, got %v", findings)
	}
}

func TestInvalidRecordedTimezoneIsReported(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	dir := newProfile(t, fourJobs, map[string]stateEntry{
		"Daily report":    {At: now.Add(-24 * time.Hour), Timezone: "Not/AZone"},
		"Nightly summary": {At: now.Add(-14*time.Hour - 30*time.Minute)},
		"Hourly check":    {At: now.Add(-13*time.Hour - 30*time.Minute)},
	})

	findings, err := newWatchdog(t, dir, now).Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0].Problem, "invalid recorded timezone") {
		t.Fatalf("expected invalid timezone finding, got %v", findings)
	}
}

// Findings are sorted so a repeated fault produces identical alert text and
// the dedupe actually holds.
func TestFindingsAreSorted(t *testing.T) {
	now := time.Date(2026, 7, 20, 23, 59, 0, 0, time.UTC)
	dir := newProfile(t, fourJobs, map[string]stateEntry{})

	findings, err := newWatchdog(t, dir, now).Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(findings) < 2 {
		t.Skip("need multiple findings to check ordering")
	}
	for i := 1; i < len(findings); i++ {
		if findings[i-1].Job > findings[i].Job {
			t.Fatalf("findings not sorted: %v", findings)
		}
	}
}
