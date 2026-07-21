package sched

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) *Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestParsesCommonSchedules(t *testing.T) {
	cases := map[string]string{
		"Daily report":  "0 7 * * *",
		"Hourly task":   "30 * * * *",
		"Weekly report": "0 18 * * 0",
	}
	for name, expr := range cases {
		if _, err := Parse(expr); err != nil {
			t.Errorf("%s (%q): %v", name, expr, err)
		}
	}
}

func TestNextRunTimes(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}

	cases := []struct {
		expr string
		from time.Time
		want time.Time
	}{
		{ // morning brief later today
			"0 7 * * *",
			time.Date(2026, 7, 20, 6, 30, 0, 0, loc),
			time.Date(2026, 7, 20, 7, 0, 0, 0, loc),
		},
		{ // morning brief rolls to tomorrow
			"0 7 * * *",
			time.Date(2026, 7, 20, 7, 30, 0, 0, loc),
			time.Date(2026, 7, 21, 7, 0, 0, 0, loc),
		},
		{ // evening close
			"30 21 * * *",
			time.Date(2026, 7, 20, 12, 0, 0, 0, loc),
			time.Date(2026, 7, 20, 21, 30, 0, 0, loc),
		},
		{ // weekly debrief: Monday -> the coming Sunday
			"0 18 * * 0",
			time.Date(2026, 7, 20, 12, 0, 0, 0, loc), // 2026-07-20 is a Monday
			time.Date(2026, 7, 26, 18, 0, 0, 0, loc),
		},
	}
	for _, c := range cases {
		got, err := mustParse(t, c.expr).Next(c.from)
		if err != nil {
			t.Errorf("%q: %v", c.expr, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("%q from %s: got %s, want %s", c.expr, c.from, got, c.want)
		}
	}
}

// Firing exactly on a boundary must advance, never repeat within the minute.
func TestNextIsStrictlyAfter(t *testing.T) {
	loc := time.UTC
	at7 := time.Date(2026, 7, 20, 7, 0, 0, 0, loc)

	next, err := mustParse(t, "0 7 * * *").Next(at7)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !next.After(at7) {
		t.Fatalf("Next(%s) = %s; must be strictly after", at7, next)
	}
}

func TestFieldSyntax(t *testing.T) {
	loc := time.UTC
	base := time.Date(2026, 7, 20, 0, 0, 0, 0, loc)

	cases := []struct {
		expr string
		want time.Time
	}{
		{"*/15 * * * *", time.Date(2026, 7, 20, 0, 15, 0, 0, loc)},
		{"0 9-17 * * *", time.Date(2026, 7, 20, 9, 0, 0, 0, loc)},
		{"0 8,12,18 * * *", time.Date(2026, 7, 20, 8, 0, 0, 0, loc)},
		{"0 7 1 * *", time.Date(2026, 8, 1, 7, 0, 0, 0, loc)},
		{"0 7 * * 1-5", time.Date(2026, 7, 20, 7, 0, 0, 0, loc)}, // Monday
	}
	for _, c := range cases {
		got, err := mustParse(t, c.expr).Next(base)
		if err != nil {
			t.Errorf("%q: %v", c.expr, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("%q: got %s, want %s", c.expr, got, c.want)
		}
	}
}

// Sunday accepted as both 0 and 7.
func TestSundayIsZeroOrSeven(t *testing.T) {
	loc := time.UTC
	monday := time.Date(2026, 7, 20, 12, 0, 0, 0, loc)

	a, err := mustParse(t, "0 18 * * 0").Next(monday)
	if err != nil {
		t.Fatalf("weekday 0: %v", err)
	}
	b, err := mustParse(t, "0 18 * * 7").Next(monday)
	if err != nil {
		t.Fatalf("weekday 7: %v", err)
	}
	if !a.Equal(b) {
		t.Fatalf("weekday 0 gave %s, weekday 7 gave %s", a, b)
	}
}

func TestInvalidExpressions(t *testing.T) {
	for _, expr := range []string{
		"0 7 * *",      // too few fields
		"0 7 * * * *",  // too many
		"60 7 * * *",   // minute out of range
		"0 24 * * *",   // hour out of range
		"0 7 32 * *",   // day out of range
		"0 7 * 13 *",   // month out of range
		"0 7 * * 8",    // weekday out of range
		"abc 7 * * *",  // not a number
		"0 17-9 * * *", // inverted range
		"*/0 * * * *",  // zero step
	} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("expected %q to be rejected", expr)
		}
	}
}

// The whole reason the scheduler is in-process: the tracker's timezone is
// switchable live for travel, and job times must move with it. A systemd timer
// would keep firing on host time.
func TestJobTimesFollowTrackerTimezone(t *testing.T) {
	home, err := time.LoadLocation("America/New_York") // UTC-3
	if err != nil {
		t.Fatalf("home zone: %v", err)
	}
	away, err := time.LoadLocation("Asia/Tokyo") // UTC+9
	if err != nil {
		t.Fatalf("away zone: %v", err)
	}

	brief := mustParse(t, "0 7 * * *")
	instant := time.Date(2026, 7, 20, 3, 0, 0, 0, time.UTC)

	atHome, err := brief.Next(instant.In(home))
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	atAway, err := brief.Next(instant.In(away))
	if err != nil {
		t.Fatalf("away: %v", err)
	}

	// Both fire at 07:00 local — a different instant in each zone.
	if atHome.Hour() != 7 || atAway.Hour() != 7 {
		t.Fatalf("expected 07:00 local in both zones, got %s and %s", atHome, atAway)
	}
	if atHome.UTC().Equal(atAway.UTC()) {
		t.Fatal("07:00 in UTC-3 and UTC+9 must be different instants")
	}
}

// Spring forward: 02:30 does not exist on the transition day, so a 02:30 job
// must skip to the next day rather than firing at a wrong hour or hanging.
func TestDSTSpringForwardSkipsMissingHour(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	// 2026-03-08: clocks jump 02:00 -> 03:00.
	before := time.Date(2026, 3, 8, 1, 0, 0, 0, loc)
	next, err := mustParse(t, "30 2 * * *").Next(before)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if next.Day() == 8 && next.Hour() == 2 {
		t.Fatalf("02:30 does not exist on 2026-03-08, got %s", next)
	}
	if next.Hour() != 2 || next.Minute() != 30 {
		t.Fatalf("expected the next real 02:30, got %s", next)
	}
}

// Fall back: 01:30 happens twice. The job must fire, and exactly once per
// scheduler pass — not twice within the same tick.
func TestDSTFallBackFiresOnce(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	// 2026-11-01: clocks fall back 02:00 -> 01:00.
	before := time.Date(2026, 11, 1, 0, 30, 0, 0, loc)
	first, err := mustParse(t, "30 1 * * *").Next(before)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if first.Hour() != 1 || first.Minute() != 30 {
		t.Fatalf("expected 01:30, got %s", first)
	}
	second, err := mustParse(t, "30 1 * * *").Next(first)
	if err != nil {
		t.Fatalf("Next after first: %v", err)
	}
	if !second.After(first) {
		t.Fatalf("second run %s must be after first %s", second, first)
	}
}

// A fake clock so scheduler behavior is deterministic.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func TestSchedulerFiresDueJob(t *testing.T) {
	loc := time.UTC
	clock := &fakeClock{t: time.Date(2026, 7, 20, 6, 59, 0, 0, loc)}

	var mu sync.Mutex
	var fired []string

	s, err := New(Config{
		Location: loc,
		Logger:   quiet(),
		Jitter:   time.Nanosecond,
		now:      clock.now,
		Jobs: []Job{{
			Name: "Morning brief", Schedule: mustParse(t, "0 7 * * *"),
			Prompt: "brief", Enabled: true,
		}},
		Runner: func(_ context.Context, job Job) error {
			mu.Lock()
			fired = append(fired, job.Name)
			mu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	s.tick(ctx) // not due yet

	mu.Lock()
	if len(fired) != 0 {
		t.Fatalf("fired before due: %v", fired)
	}
	mu.Unlock()

	clock.set(time.Date(2026, 7, 20, 7, 0, 30, 0, loc))
	s.tick(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(fired)
		mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job did not fire")
}

// One due window fires once, however many ticks land inside it.
func TestSchedulerDoesNotDoubleFire(t *testing.T) {
	loc := time.UTC
	clock := &fakeClock{t: time.Date(2026, 7, 20, 7, 0, 10, 0, loc)}

	var mu sync.Mutex
	count := 0

	s, err := New(Config{
		Location: loc, Logger: quiet(), Jitter: time.Nanosecond, now: clock.now,
		Jobs: []Job{{
			Name: "Morning brief", Schedule: mustParse(t, "0 7 * * *"),
			Prompt: "brief", Enabled: true,
		}},
		Runner: func(context.Context, Job) error {
			mu.Lock()
			count++
			mu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// New() computed next as tomorrow 07:00; force today's window.
	s.nextRun["Morning brief"] = time.Date(2026, 7, 20, 7, 0, 0, 0, loc)

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		s.tick(ctx)
		clock.set(clock.now().Add(30 * time.Second))
	}
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("job fired %d times, want 1", count)
	}
}

// Firing a 07:00 morning brief at 15:00 is worse than skipping it: the content
// is wrong and it reads as the system being confused.
func TestStaleRunIsSkipped(t *testing.T) {
	loc := time.UTC
	clock := &fakeClock{t: time.Date(2026, 7, 20, 15, 0, 0, 0, loc)}

	var mu sync.Mutex
	fired := false

	s, err := New(Config{
		Location: loc, Logger: quiet(), Jitter: time.Nanosecond, now: clock.now,
		Jobs: []Job{{
			Name: "Morning brief", Schedule: mustParse(t, "0 7 * * *"),
			Prompt: "brief", Enabled: true,
		}},
		Runner: func(context.Context, Job) error {
			mu.Lock()
			fired = true
			mu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Simulate the process having been down since 07:00.
	s.nextRun["Morning brief"] = time.Date(2026, 7, 20, 7, 0, 0, 0, loc)

	s.tick(context.Background())
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if fired {
		t.Fatal("an 8-hour-late morning brief should be skipped, not fired")
	}

	// The skip must be recorded for status reporting.
	health := s.Health()
	if len(health) != 1 || !health[0].LastSkipped {
		t.Fatalf("skip not recorded in health: %+v", health)
	}
}

func TestDisabledJobNeverFires(t *testing.T) {
	loc := time.UTC
	clock := &fakeClock{t: time.Date(2026, 7, 20, 7, 0, 30, 0, loc)}

	s, err := New(Config{
		Location: loc, Logger: quiet(), Jitter: time.Nanosecond, now: clock.now,
		Jobs: []Job{{
			Name: "Morning brief", Schedule: mustParse(t, "0 7 * * *"),
			Prompt: "brief", Enabled: false,
		}},
		Runner: func(context.Context, Job) error {
			t.Error("disabled job fired")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.tick(context.Background())
	time.Sleep(50 * time.Millisecond)
}

// A failing job must be recorded and must not stop the scheduler: silence is
// this system's failure mode.
func TestFailedRunIsRecordedAndLoopContinues(t *testing.T) {
	loc := time.UTC
	clock := &fakeClock{t: time.Date(2026, 7, 20, 7, 0, 10, 0, loc)}
	state := filepath.Join(t.TempDir(), "sched.json")

	s, err := New(Config{
		Location: loc, Logger: quiet(), Jitter: time.Nanosecond, now: clock.now,
		StatePath: state,
		Jobs: []Job{{
			Name: "Morning brief", Schedule: mustParse(t, "0 7 * * *"),
			Prompt: "brief", Enabled: true,
		}},
		Runner: func(context.Context, Job) error {
			return errors.New("provider unavailable")
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.nextRun["Morning brief"] = time.Date(2026, 7, 20, 7, 0, 0, 0, loc)

	s.tick(context.Background())
	time.Sleep(150 * time.Millisecond)

	health := s.Health()
	if len(health) != 1 || health[0].LastError == "" {
		t.Fatalf("failure not recorded: %+v", health)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("state not persisted: %v", err)
	}
	// The next run must still be scheduled.
	if health[0].NextRun.IsZero() {
		t.Fatal("next run not scheduled after a failure")
	}
}

// One panicking job must not take down a process meant to run for months.
func TestPanicInJobIsContained(t *testing.T) {
	loc := time.UTC
	clock := &fakeClock{t: time.Date(2026, 7, 20, 7, 0, 10, 0, loc)}

	s, err := New(Config{
		Location: loc, Logger: quiet(), Jitter: time.Nanosecond, now: clock.now,
		Jobs: []Job{{
			Name: "Morning brief", Schedule: mustParse(t, "0 7 * * *"),
			Prompt: "brief", Enabled: true,
		}},
		Runner: func(context.Context, Job) error { panic("boom") },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.nextRun["Morning brief"] = time.Date(2026, 7, 20, 7, 0, 0, 0, loc)

	s.tick(context.Background())
	time.Sleep(150 * time.Millisecond)

	health := s.Health()
	if len(health) != 1 || health[0].LastError == "" {
		t.Fatalf("panic not recorded as a failure: %+v", health)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	loc := time.UTC
	state := filepath.Join(t.TempDir(), "sched.json")
	clock := &fakeClock{t: time.Date(2026, 7, 20, 7, 0, 10, 0, loc)}

	jobs := []Job{{
		Name: "Morning brief", Schedule: mustParse(t, "0 7 * * *"),
		Prompt: "brief", Enabled: true,
	}}

	first, err := New(Config{
		Location: loc, Logger: quiet(), Jitter: time.Nanosecond, now: clock.now,
		StatePath: state, Jobs: jobs,
		Runner: func(context.Context, Job) error { return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first.nextRun["Morning brief"] = time.Date(2026, 7, 20, 7, 0, 0, 0, loc)
	first.tick(context.Background())
	time.Sleep(150 * time.Millisecond)

	// Restart: a fresh scheduler over the same state file.
	second, err := New(Config{
		Location: loc, Logger: quiet(), Jitter: time.Nanosecond, now: clock.now,
		StatePath: state, Jobs: jobs,
		Runner: func(context.Context, Job) error { return nil },
	})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	health := second.Health()
	if len(health) != 1 || health[0].LastRun.IsZero() {
		t.Fatalf("last run did not survive restart: %+v", health)
	}
}

func TestCorruptStateDoesNotBlockStartup(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sched.json")
	if err := os.WriteFile(state, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := New(Config{
		Location: time.UTC, Logger: quiet(), StatePath: state,
		Jobs: []Job{{
			Name: "Morning brief", Schedule: mustParse(t, "0 7 * * *"),
			Prompt: "brief", Enabled: true,
		}},
		Runner: func(context.Context, Job) error { return nil },
	})
	if err != nil {
		t.Fatalf("corrupt state should not block startup: %v", err)
	}
}

func TestSchedulerValidation(t *testing.T) {
	runner := func(context.Context, Job) error { return nil }
	job := Job{Name: "a", Schedule: mustParse(t, "0 7 * * *"), Prompt: "p", Enabled: true}

	// A missing location would silently default to UTC and misfile sessions.
	if _, err := New(Config{Logger: quiet(), Runner: runner, Jobs: []Job{job}}); err == nil {
		t.Error("expected a missing location to be refused")
	}
	if _, err := New(Config{Location: time.UTC, Logger: quiet(), Jobs: []Job{job}}); err == nil {
		t.Error("expected a missing runner to be refused")
	}

	dup := []Job{job, job}
	if _, err := New(Config{Location: time.UTC, Logger: quiet(), Runner: runner, Jobs: dup}); err == nil {
		t.Error("expected duplicate job names to be refused")
	}

	noPrompt := Job{Name: "b", Schedule: mustParse(t, "0 7 * * *"), Enabled: true}
	if _, err := New(Config{Location: time.UTC, Logger: quiet(), Runner: runner, Jobs: []Job{noPrompt}}); err == nil {
		t.Error("expected a job without a prompt to be refused")
	}
}

// Jobs sharing a minute must separate, and do so the same way each restart.
func TestJitterIsStableAndBounded(t *testing.T) {
	const max = 10 * time.Second

	a1, a2 := jitterFor("Evening close", max), jitterFor("Evening close", max)
	if a1 != a2 {
		t.Fatalf("jitter is not stable: %v vs %v", a1, a2)
	}
	if a1 < 0 || a1 >= max {
		t.Fatalf("jitter %v out of bounds", a1)
	}
	if jitterFor("Evening close", max) == jitterFor("Night guard", max) {
		t.Error("distinct jobs should get distinct jitter")
	}
}

func TestHealthReportsAllJobs(t *testing.T) {
	s, err := New(Config{
		Location: time.UTC, Logger: quiet(),
		Jobs: []Job{
			{Name: "Weekly debrief", Schedule: mustParse(t, "0 18 * * 0"), Prompt: "p", Enabled: true},
			{Name: "Morning brief", Schedule: mustParse(t, "0 7 * * *"), Prompt: "p", Enabled: true},
			{Name: "Night guard", Schedule: mustParse(t, "30 22 * * *"), Prompt: "p", Enabled: false},
		},
		Runner: func(context.Context, Job) error { return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	health := s.Health()
	if len(health) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(health))
	}
	if health[0].Name != "Morning brief" {
		t.Fatalf("health should be sorted by name, got %s first", health[0].Name)
	}
	for _, h := range health {
		if h.Enabled && h.NextRun.IsZero() {
			t.Errorf("%s is enabled but has no next run", h.Name)
		}
		if !h.Enabled && !h.NextRun.IsZero() {
			t.Errorf("%s is disabled but has a next run", h.Name)
		}
	}
}
