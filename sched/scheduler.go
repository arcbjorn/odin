package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Job is one scheduled task.
//
// There is no cronjob tool and no jobs.json written by the model: jobs are
// declared in config and their prompts live in files. That removes the whole
// class of failure where a job's stored provider snapshot drifts from the
// global config and the job hard-skips with a spend-guard error — the morning
// brief died that way and stayed dead until someone noticed the silence.
type Job struct {
	Name     string
	Schedule *Schedule

	// Prompt is the instruction sent to the agent when the job fires.
	Prompt string

	// Skills lists documents loaded into this job's isolated prompt before it
	// runs. Interactive turns still load skills on demand with read_skill.
	Skills []string

	// Enabled allows a job to be switched off without deleting its history.
	Enabled bool
}

// Runner executes a job. Returning an error marks the run failed; the
// scheduler records it and continues rather than exiting.
type Runner func(ctx context.Context, job Job) error

// Scheduler fires jobs on local wall-clock time.
type Scheduler struct {
	jobs  []Job
	loc   *time.Location
	run   Runner
	log   *slog.Logger
	state *stateStore

	// now is swappable so tests can drive time deterministically.
	now func() time.Time

	// jitter spreads simultaneous jobs. Zero in tests.
	jitter time.Duration

	mu      sync.Mutex
	nextRun map[string]time.Time
	running map[string]bool
}

// Config configures a Scheduler.
type Config struct {
	Jobs []Job

	// Location must come from the database's settings table, not from the host
	// clock or config.toml. It is fixed for this process; restart Odin after a
	// travel-related timezone change so every job moves with it.
	Location *time.Location

	Runner Runner
	Logger *slog.Logger

	// StatePath persists last-run times so a restart does not re-fire a job
	// that already ran, and so a missed run is visible.
	StatePath string

	// Jitter delays each fire by up to this much, so several jobs sharing a
	// minute do not hit the provider at once. Defaults to 10s.
	Jitter time.Duration

	now func() time.Time
}

// New builds a Scheduler.
func New(cfg Config) (*Scheduler, error) {
	if cfg.Location == nil {
		// Defaulting to UTC would silently misfile every late-night session.
		return nil, errors.New("scheduler requires a location from the database")
	}
	if cfg.Runner == nil {
		return nil, errors.New("scheduler requires a runner")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	jitter := cfg.Jitter
	if jitter == 0 {
		jitter = 10 * time.Second
	}

	seen := map[string]bool{}
	for _, j := range cfg.Jobs {
		if j.Name == "" {
			return nil, errors.New("job name is required")
		}
		if seen[j.Name] {
			return nil, fmt.Errorf("duplicate job name %q", j.Name)
		}
		seen[j.Name] = true
		if j.Schedule == nil {
			return nil, fmt.Errorf("job %q has no schedule", j.Name)
		}
		if j.Prompt == "" {
			return nil, fmt.Errorf("job %q has no prompt", j.Name)
		}
	}

	s := &Scheduler{
		jobs:    cfg.Jobs,
		loc:     cfg.Location,
		run:     cfg.Runner,
		log:     cfg.Logger,
		now:     cfg.now,
		jitter:  jitter,
		nextRun: make(map[string]time.Time),
		running: make(map[string]bool),
	}

	store, err := newStateStore(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	s.state = store
	s.state.setTimezone(s.loc.String())

	now := s.now().In(s.loc)
	for _, j := range s.jobs {
		if !j.Enabled {
			continue
		}
		next, err := j.Schedule.Next(now)
		if err != nil {
			return nil, fmt.Errorf("job %q: %w", j.Name, err)
		}
		s.nextRun[j.Name] = next
	}
	return s, nil
}

// Run fires jobs until ctx is cancelled.
//
// Ticks every 30s rather than sleeping until the next due time, so a laptop
// suspend or a clock jump is noticed within half a minute instead of leaving
// the agent asleep on a stale timer.
func (s *Scheduler) Run(ctx context.Context) error {
	s.log.Info("scheduler started",
		"jobs", len(s.jobs), "timezone", s.loc.String(), "next", s.NextRuns())

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			s.log.Info("scheduler stopped")
			return ctx.Err()
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	now := s.now().In(s.loc)

	for _, job := range s.jobs {
		if !job.Enabled {
			continue
		}

		s.mu.Lock()
		due, ok := s.nextRun[job.Name]
		s.mu.Unlock()
		if !ok || now.Before(due) {
			continue
		}

		// Schedule the following run before executing, so a slow or panicking
		// run cannot wedge the job permanently.
		next, err := job.Schedule.Next(now)
		if err != nil {
			s.log.Error("cannot compute next run", "job", job.Name, "error", err)
			continue
		}
		s.mu.Lock()
		s.nextRun[job.Name] = next
		s.mu.Unlock()

		// A run more than an hour late means the process was down. Firing a
		// time-sensitive job hours after its window is worse than skipping it —
		// the content is stale and it reads as the system being confused.
		if late := now.Sub(due); late > time.Hour {
			s.log.Warn("skipping stale run", "job", job.Name,
				"due", due.Format(time.RFC3339), "late", late.Round(time.Minute))
			s.state.record(job.Name, runRecord{
				At: now, Timezone: s.loc.String(), Skipped: true,
				Error: fmt.Sprintf("skipped: %s late", late.Round(time.Minute)),
			})
			continue
		}

		s.mu.Lock()
		if s.running[job.Name] {
			s.mu.Unlock()
			s.log.Warn("skipping overlapping run", "job", job.Name,
				"due", due.Format(time.RFC3339))
			s.state.record(job.Name, runRecord{
				At: now, Timezone: s.loc.String(), Skipped: true,
				Error: "skipped: previous run still active",
			})
			continue
		}
		s.running[job.Name] = true
		s.mu.Unlock()

		go s.execute(ctx, job, due)
	}
}

func (s *Scheduler) execute(ctx context.Context, job Job, due time.Time) {
	defer func() {
		s.mu.Lock()
		delete(s.running, job.Name)
		s.mu.Unlock()
	}()

	if s.jitter > 0 {
		select {
		case <-time.After(jitterFor(job.Name, s.jitter)):
		case <-ctx.Done():
			return
		}
	}

	start := s.now().In(s.loc)
	s.log.Info("job started", "job", job.Name, "due", due.Format(time.RFC3339))

	err := s.safeRun(ctx, job)
	rec := runRecord{At: start, Timezone: s.loc.String(), Duration: s.now().Sub(start)}
	if err != nil {
		rec.Error = err.Error()
		// Failed runs are logged and persisted for status reporting.
		s.log.Error("job failed", "job", job.Name, "error", err,
			"duration", rec.Duration.Round(time.Second))
	} else {
		s.log.Info("job finished", "job", job.Name,
			"duration", rec.Duration.Round(time.Second))
	}
	s.state.record(job.Name, rec)
}

// safeRun converts a panic in a job into an error, so one bad run cannot take
// down a process that is meant to stay up for months.
func (s *Scheduler) safeRun(ctx context.Context, job Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return s.run(ctx, job)
}

// NextRuns reports the next fire time per job, for `odin status`.
func (s *Scheduler) NextRuns() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]string, len(s.nextRun))
	for name, t := range s.nextRun {
		out[name] = t.Format("2006-01-02 15:04 MST")
	}
	return out
}

// Health reports each job's last outcome.
func (s *Scheduler) Health() []JobHealth {
	s.mu.Lock()
	next := make(map[string]time.Time, len(s.nextRun))
	for k, v := range s.nextRun {
		next[k] = v
	}
	s.mu.Unlock()

	out := make([]JobHealth, 0, len(s.jobs))
	for _, job := range s.jobs {
		h := JobHealth{Name: job.Name, Enabled: job.Enabled, Schedule: job.Schedule.String()}
		if t, ok := next[job.Name]; ok {
			h.NextRun = t
		}
		if rec, ok := s.state.last(job.Name); ok {
			h.LastRun = rec.At
			h.LastError = rec.Error
			h.LastSkipped = rec.Skipped
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// JobHealth is one job's status.
type JobHealth struct {
	Name        string
	Enabled     bool
	Schedule    string
	NextRun     time.Time
	LastRun     time.Time
	LastError   string
	LastSkipped bool
}

// jitterFor derives a stable per-job offset from its name, so two jobs sharing
// a minute reliably separate instead of colliding differently each restart.
func jitterFor(name string, max time.Duration) time.Duration {
	var h uint32 = 2166136261
	for i := 0; i < len(name); i++ {
		h ^= uint32(name[i])
		h *= 16777619
	}
	return time.Duration(uint64(h) % uint64(max))
}

// stateStore persists last-run records.
type stateStore struct {
	path string

	mu   sync.Mutex
	runs map[string]runRecord
}

type runRecord struct {
	At       time.Time     `json:"at"`
	Timezone string        `json:"timezone,omitempty"`
	Duration time.Duration `json:"duration_ns,omitempty"`
	Error    string        `json:"error,omitempty"`
	Skipped  bool          `json:"skipped,omitempty"`
}

func newStateStore(path string) (*stateStore, error) {
	s := &stateStore{path: path, runs: make(map[string]runRecord)}
	if path == "" {
		return s, nil // in-memory, for tests
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read scheduler state: %w", err)
	}
	if err := json.Unmarshal(raw, &s.runs); err != nil {
		// Corrupt state must not block startup: the jobs matter more than
		// the history, and the next run rewrites it.
		s.runs = make(map[string]runRecord)
	}
	return s, nil
}

// setTimezone updates the scheduling context on existing records at startup.
// JSON timestamps retain only a numeric offset, which is insufficient across
// DST or after travel. The watchdog needs the authoritative IANA zone even
// before the next job records a fresh run.
func (s *stateStore) setTimezone(name string) {
	s.mu.Lock()
	changed := false
	for job, rec := range s.runs {
		if rec.Timezone != name {
			rec.Timezone = name
			s.runs[job] = rec
			changed = true
		}
	}
	snapshot := make(map[string]runRecord, len(s.runs))
	for k, v := range s.runs {
		snapshot[k] = v
	}
	s.mu.Unlock()

	if !changed || s.path == "" {
		return
	}
	if err := writeJSONAtomic(s.path, snapshot); err != nil {
		slog.Default().Warn("could not persist scheduler timezone", "error", err)
	}
}

func (s *stateStore) record(job string, rec runRecord) {
	s.mu.Lock()
	s.runs[job] = rec
	snapshot := make(map[string]runRecord, len(s.runs))
	for k, v := range s.runs {
		snapshot[k] = v
	}
	s.mu.Unlock()

	if s.path == "" {
		return
	}
	if err := writeJSONAtomic(s.path, snapshot); err != nil {
		slog.Default().Warn("could not persist scheduler state", "error", err)
	}
}

func (s *stateStore) last(job string) (runRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.runs[job]
	return rec, ok
}

func writeJSONAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sched-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
