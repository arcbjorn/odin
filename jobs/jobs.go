// Package jobs loads scheduled job definitions from a profile directory.
//
// Layout:
//
//	<profile>/jobs/jobs.toml       schedules and enable flags
//	<profile>/jobs/<name>.md       one prompt per job
//
// Prompts live in their own files because they are long, they are edited by
// hand, and they belong in git. Splitting schedule from prompt also means
// editing a prompt cannot corrupt a schedule, and a diff shows what actually
// changed rather than one reflowed JSON line.
//
// There is deliberately no create/update/delete API and no tool that writes
// here. Jobs are declared by the operator, not by the model.
package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/arcbjorn/odin/sched"
)

// Set is a profile's loaded jobs.
type Set struct {
	Jobs []sched.Job
}

// Load reads <profile>/jobs. A missing directory returns os.ErrNotExist so
// callers can treat "no jobs" as a valid configuration.
func Load(profileDir string) (*Set, error) {
	dir := filepath.Join(profileDir, "jobs")
	manifest := filepath.Join(dir, "jobs.toml")

	raw, err := os.ReadFile(manifest)
	if err != nil {
		return nil, err
	}

	entries, err := parseManifest(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", manifest, err)
	}

	set := &Set{}
	for _, e := range entries {
		schedule, err := sched.Parse(e.Schedule)
		if err != nil {
			return nil, fmt.Errorf("job %q: %w", e.Name, err)
		}

		promptFile := e.Prompt
		if promptFile == "" {
			promptFile = slug(e.Name) + ".md"
		}
		promptPath := filepath.Join(dir, promptFile)

		body, err := os.ReadFile(promptPath)
		if err != nil {
			return nil, fmt.Errorf("job %q: read prompt %s: %w", e.Name, promptPath, err)
		}
		prompt := strings.TrimSpace(string(body))
		if prompt == "" {
			return nil, fmt.Errorf("job %q: prompt %s is empty", e.Name, promptPath)
		}

		set.Jobs = append(set.Jobs, sched.Job{
			Name:     e.Name,
			Schedule: schedule,
			Prompt:   prompt,
			Skills:   e.Skills,
			Enabled:  e.Enabled,
		})
	}

	sort.Slice(set.Jobs, func(i, j int) bool { return set.Jobs[i].Name < set.Jobs[j].Name })
	return set, nil
}

// Find returns a job by name, case-insensitively.
func (s *Set) Find(name string) (sched.Job, bool) {
	for _, j := range s.Jobs {
		if strings.EqualFold(j.Name, name) {
			return j, true
		}
	}
	return sched.Job{}, false
}

// Names lists job names.
func (s *Set) Names() []string {
	out := make([]string, 0, len(s.Jobs))
	for _, j := range s.Jobs {
		out = append(out, j.Name)
	}
	return out
}

type entry struct {
	Name     string
	Schedule string
	Prompt   string
	Skills   []string
	Enabled  bool
}

// parseManifest reads repeated [[job]] tables.
func parseManifest(src string) ([]entry, error) {
	var entries []entry
	seen := map[string]bool{}
	inJob := false

	for n, line := range strings.Split(src, "\n") {
		lineNo := n + 1
		line = strings.TrimSpace(stripComment(line))
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "[[") {
			name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "[["), "]]"))
			if name != "job" {
				return nil, fmt.Errorf("line %d: unknown table [[%s]]", lineNo, name)
			}
			// Enabled defaults true: a job someone bothered to declare should
			// run unless explicitly switched off.
			entries = append(entries, entry{Enabled: true})
			inJob = true
			continue
		}
		if strings.HasPrefix(line, "[") {
			return nil, fmt.Errorf("line %d: only [[job]] tables are supported", lineNo)
		}
		if !inJob {
			return nil, fmt.Errorf("line %d: key outside any [[job]] block", lineNo)
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key = value", lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		e := &entries[len(entries)-1]

		switch key {
		case "name":
			s, err := unquote(value)
			if err != nil {
				return nil, fmt.Errorf("line %d: name: %w", lineNo, err)
			}
			if seen[s] {
				return nil, fmt.Errorf("line %d: duplicate job name %q", lineNo, s)
			}
			seen[s] = true
			e.Name = s
		case "schedule":
			s, err := unquote(value)
			if err != nil {
				return nil, fmt.Errorf("line %d: schedule: %w", lineNo, err)
			}
			e.Schedule = s
		case "prompt":
			s, err := unquote(value)
			if err != nil {
				return nil, fmt.Errorf("line %d: prompt: %w", lineNo, err)
			}
			e.Prompt = s
		case "skills":
			list, err := unquoteList(value)
			if err != nil {
				return nil, fmt.Errorf("line %d: skills: %w", lineNo, err)
			}
			e.Skills = list
		case "enabled":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return nil, fmt.Errorf("line %d: enabled: expected true or false", lineNo)
			}
			e.Enabled = b
		default:
			return nil, fmt.Errorf("line %d: unknown key %q", lineNo, key)
		}
	}

	for i, e := range entries {
		if e.Name == "" {
			return nil, fmt.Errorf("job %d has no name", i+1)
		}
		if e.Schedule == "" {
			return nil, fmt.Errorf("job %q has no schedule", e.Name)
		}
	}
	return entries, nil
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

func unquote(v string) (string, error) {
	if len(v) < 2 || !strings.HasPrefix(v, `"`) || !strings.HasSuffix(v, `"`) {
		return "", fmt.Errorf("expected a quoted string, got %s", v)
	}
	return v[1 : len(v)-1], nil
}

func unquoteList(v string) ([]string, error) {
	if !strings.HasPrefix(v, "[") || !strings.HasSuffix(v, "]") {
		return nil, fmt.Errorf("expected [ ... ], got %s", v)
	}
	inner := strings.TrimSpace(v[1 : len(v)-1])
	if inner == "" {
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(inner, ",") {
		s, err := unquote(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// slug turns "Morning brief" into "morning-brief".
func slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ', r == '_', r == '-':
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
