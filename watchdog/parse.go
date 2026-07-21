package watchdog

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// This file re-implements just enough manifest and cron parsing for the
// watchdog to work.
//
// Duplicating ~100 lines is deliberate. Importing the agent's jobs and sched
// packages would mean a bug in either can blind the watchdog to the very
// outage it exists to catch — the two would fail together. The parsers here
// are intentionally simpler and read the same files independently.

// readManifest extracts name, schedule, and enabled from jobs.toml. It ignores
// prompts entirely: the watchdog never needs to know what a job says, only
// whether it ran.
func readManifest(path string) ([]jobManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var (
		jobs    []jobManifest
		current *jobManifest
	)

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(stripComment(line))
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "[[") {
			// Default enabled to true, matching the agent's own default. A
			// watchdog that assumed false would go quiet on every job.
			jobs = append(jobs, jobManifest{Enabled: true})
			current = &jobs[len(jobs)-1]
			continue
		}
		if current == nil {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch key {
		case "name":
			current.Name = unquote(value)
		case "schedule":
			current.Schedule = unquote(value)
		case "enabled":
			current.Enabled = value == "true"
		}
	}

	// A job with no name cannot be matched against scheduler state, so it is
	// not something this tool can reason about.
	out := jobs[:0]
	for _, j := range jobs {
		if j.Name != "" && j.Schedule != "" {
			out = append(out, j)
		}
	}
	return out, nil
}

func stripComment(line string) string {
	inQuote := false
	for i, r := range line {
		switch r {
		case '"':
			inQuote = !inQuote
		case '#':
			if !inQuote {
				return line[:i]
			}
		}
	}
	return line
}

func unquote(v string) string {
	if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
		return v[1 : len(v)-1]
	}
	return v
}

// nextDue returns the first time the schedule should have fired after the
// given time, in that time's location.
//
// Steps minute by minute, like the agent's scheduler, because it is easy to
// verify and handles DST for free: a skipped hour is never visited and a
// repeated one is visited twice.
func nextDue(expr string, after time.Time) (time.Time, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return time.Time{}, fmt.Errorf("expected 5 cron fields, got %d", len(fields))
	}

	minutes, err := parseField(fields[0], 0, 59)
	if err != nil {
		return time.Time{}, err
	}
	hours, err := parseField(fields[1], 0, 23)
	if err != nil {
		return time.Time{}, err
	}
	days, err := parseField(fields[2], 1, 31)
	if err != nil {
		return time.Time{}, err
	}
	months, err := parseField(fields[3], 1, 12)
	if err != nil {
		return time.Time{}, err
	}
	weekdays, err := parseField(fields[4], 0, 7)
	if err != nil {
		return time.Time{}, err
	}

	dayRestricted := fields[2] != "*"
	weekdayRestricted := fields[4] != "*"

	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(1, 0, 0)

	for t.Before(limit) {
		if minutes[t.Minute()] && hours[t.Hour()] && months[int(t.Month())] {
			day := days[t.Day()]
			// Sunday is both 0 and 7 in cron.
			weekday := weekdays[int(t.Weekday())] || (t.Weekday() == time.Sunday && weekdays[7])

			var match bool
			switch {
			case dayRestricted && weekdayRestricted:
				match = day || weekday // standard cron: either, not both
			case dayRestricted:
				match = day
			case weekdayRestricted:
				match = weekday
			default:
				match = true
			}
			if match {
				return t, nil
			}
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("schedule %q has no match within a year", expr)
}

// parseField expands one cron field into a lookup set.
func parseField(field string, min, max int) (map[int]bool, error) {
	set := make(map[int]bool)

	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty item in %q", field)
		}

		step := 1
		if base, stepStr, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("invalid step %q", stepStr)
			}
			step = n
			part = base
		}

		lo, hi := min, max
		switch {
		case part == "*":
		case strings.Contains(part, "-"):
			loStr, hiStr, _ := strings.Cut(part, "-")
			var err error
			if lo, err = strconv.Atoi(strings.TrimSpace(loStr)); err != nil {
				return nil, fmt.Errorf("invalid range %q", part)
			}
			if hi, err = strconv.Atoi(strings.TrimSpace(hiStr)); err != nil {
				return nil, fmt.Errorf("invalid range %q", part)
			}
			if lo > hi {
				return nil, fmt.Errorf("inverted range %q", part)
			}
		default:
			v, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid value %q", part)
			}
			lo, hi = v, v
		}

		if lo < min || hi > max {
			return nil, fmt.Errorf("value out of range %d-%d in %q", min, max, field)
		}
		for v := lo; v <= hi; v += step {
			set[v] = true
		}
	}
	return set, nil
}
