package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/arcbjorn/odin/agent"
	"github.com/arcbjorn/odin/model"
)

// maxSkillBytes caps one skill document.
const maxSkillBytes = 200 << 10

// Skills exposes markdown skill documents to the model.
//
// A skill is a markdown file in the profile's skills directory.
type Skills struct {
	dir string
}

// NewSkills builds the skill toolset over a directory of markdown files.
func NewSkills(dir string) (*Skills, error) {
	if dir == "" {
		return nil, fmt.Errorf("skills dir is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("skills dir %s: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("skills path %s is not a directory", abs)
	}
	return &Skills{dir: abs}, nil
}

// Tool returns the skill-reading tool.
func (s *Skills) Tool() agent.Tool {
	return agent.Tool{
		Def: model.Tool{
			Name:        "read_skill",
			Description: "Read a skill document by name. Skills hold procedures and schema references.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "description": "Skill name without the .md extension."},
				},
				"required": []string{"name"},
			},
		},
		Handle: s.handleRead,
	}
}

type skillInput struct {
	Name string `json:"name"`
}

func (s *Skills) handleRead(_ context.Context, raw json.RawMessage) (string, error) {
	var in skillInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	name = strings.TrimSuffix(name, ".md")

	// Skill names are flat identifiers, not paths. Rejecting separators
	// outright is simpler and tighter than resolving traversal.
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", fmt.Errorf("skill name must not contain a path")
	}

	path := filepath.Join(s.dir, name+".md")
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			available, _ := s.List()
			if len(available) == 0 {
				return "", fmt.Errorf("no skill %q; none are installed", name)
			}
			return "", fmt.Errorf("no skill %q; available: %s", name, strings.Join(available, ", "))
		}
		return "", err
	}
	if info.Size() > maxSkillBytes {
		return "", fmt.Errorf("skill %q is %d bytes, over the %d byte limit", name, info.Size(), maxSkillBytes)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// List returns installed skill names, sorted.
func (s *Skills) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".md"))
	}
	sort.Strings(names)
	return names, nil
}

// Catalog renders a one-line-per-skill summary for the system prompt, so the
// model knows what exists without loading every document into context.
//
// The summary is each skill's first markdown heading, or its first non-empty
// line. That keeps the catalog cheap: a few dozen tokens instead of thousands.
func (s *Skills) Catalog() (string, error) {
	names, err := s.List()
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", nil
	}

	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "- %s", name)
		if desc := s.summarize(name); desc != "" {
			fmt.Fprintf(&b, ": %s", desc)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (s *Skills) summarize(name string) string {
	data, err := os.ReadFile(filepath.Join(s.dir, name+".md"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimLeft(line, "# ")
		if line == "" {
			continue
		}
		if len(line) > 120 {
			line = line[:120] + "..."
		}
		return line
	}
	return ""
}
