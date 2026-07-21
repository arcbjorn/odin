// Package sched runs jobs on a wall-clock schedule in the agent's own process.
//
// Why in-process rather than systemd timers: the tracker's timezone is
// switchable live for travel, and systemd resolves timers against the host
// clock. One row change would silently leave every timer an hour wrong, which
// is precisely the class of quiet failure this agent must not have. Keeping
// the clock here means "local time" always means the same thing the tracker
// means, and next-run state is queryable rather than parsed out of systemctl.
package sched

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed five-field cron expression:
//
//	minute hour day-of-month month day-of-week
//
// Supports *, lists (1,3,5), ranges (1-5), and steps (*/15). Day-of-week is
// 0-6 with Sunday 0; 7 is accepted as Sunday too. Deliberately no @reboot,
// @yearly, or seconds field — the four real jobs are daily and weekly, and
// unused syntax is untested syntax.
type Schedule struct {
	minutes  [60]bool
	hours    [24]bool
	days     [32]bool // 1-31; index 0 unused
	months   [13]bool // 1-12; index 0 unused
	weekdays [7]bool  // 0-6, Sunday 0

	// When both day-of-month and day-of-week are restricted, cron matches
	// either, not both. Tracked so Next() can apply that rule.
	dayRestricted     bool
	weekdayRestricted bool

	expr string
}

// Parse reads a five-field cron expression.
func Parse(expr string) (*Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression needs 5 fields (minute hour day month weekday), got %d in %q", len(fields), expr)
	}

	s := &Schedule{expr: strings.Join(fields, " ")}

	specs := []struct {
		field string
		min   int
		max   int
		set   func(int)
	}{
		{fields[0], 0, 59, func(v int) { s.minutes[v] = true }},
		{fields[1], 0, 23, func(v int) { s.hours[v] = true }},
		{fields[2], 1, 31, func(v int) { s.days[v] = true }},
		{fields[3], 1, 12, func(v int) { s.months[v] = true }},
		{fields[4], 0, 7, func(v int) { s.weekdays[v%7] = true }},
	}
	for i, spec := range specs {
		if err := parseField(spec.field, spec.min, spec.max, spec.set); err != nil {
			return nil, fmt.Errorf("field %d (%q): %w", i+1, spec.field, err)
		}
	}

	s.dayRestricted = fields[2] != "*"
	s.weekdayRestricted = fields[4] != "*"
	return s, nil
}

// String returns the normalized expression.
func (s *Schedule) String() string { return s.expr }

// Next returns the first matching time strictly after t, in t's location.
//
// Iterates minute by minute rather than solving the constraint directly. A
// year of minutes is ~526k iterations of trivial array lookups — microseconds,
// and far easier to verify correct than clever date arithmetic. It also gets
// DST right for free: the loop walks wall-clock minutes, so a skipped hour is
// simply never visited and a repeated hour is visited twice.
func (s *Schedule) Next(t time.Time) (time.Time, error) {
	// Truncate to the minute and step forward, so a job never fires twice
	// within the same minute.
	next := t.Truncate(time.Minute).Add(time.Minute)

	limit := next.AddDate(1, 0, 0)
	for next.Before(limit) {
		if s.matches(next) {
			return next, nil
		}
		next = next.Add(time.Minute)
	}
	// Only reachable for an expression that can never match, e.g. Feb 30.
	return time.Time{}, fmt.Errorf("schedule %q has no match within a year", s.expr)
}

func (s *Schedule) matches(t time.Time) bool {
	if !s.minutes[t.Minute()] || !s.hours[t.Hour()] || !s.months[int(t.Month())] {
		return false
	}

	day := s.days[t.Day()]
	weekday := s.weekdays[int(t.Weekday())]

	switch {
	case s.dayRestricted && s.weekdayRestricted:
		// Standard cron: either matches, not both.
		return day || weekday
	case s.dayRestricted:
		return day
	case s.weekdayRestricted:
		return weekday
	default:
		return true
	}
}

func parseField(field string, min, max int, set func(int)) error {
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return fmt.Errorf("empty item")
		}

		step := 1
		if base, stepStr, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return fmt.Errorf("invalid step %q", stepStr)
			}
			step = n
			part = base
		}

		lo, hi := min, max
		switch {
		case part == "*":
			// full range
		case strings.Contains(part, "-"):
			loStr, hiStr, _ := strings.Cut(part, "-")
			var err error
			if lo, err = strconv.Atoi(strings.TrimSpace(loStr)); err != nil {
				return fmt.Errorf("invalid range start %q", loStr)
			}
			if hi, err = strconv.Atoi(strings.TrimSpace(hiStr)); err != nil {
				return fmt.Errorf("invalid range end %q", hiStr)
			}
			if lo > hi {
				return fmt.Errorf("range %d-%d is inverted", lo, hi)
			}
		default:
			v, err := strconv.Atoi(part)
			if err != nil {
				return fmt.Errorf("invalid value %q", part)
			}
			lo, hi = v, v
		}

		if lo < min || hi > max {
			return fmt.Errorf("value out of range %d-%d", min, max)
		}
		for v := lo; v <= hi; v += step {
			set(v)
		}
	}
	return nil
}
