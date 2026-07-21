package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newSkills(t *testing.T, files map[string]string) *Skills {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	s, err := NewSkills(dir)
	if err != nil {
		t.Fatalf("NewSkills: %v", err)
	}
	return s
}

func TestReadSkill(t *testing.T) {
	s := newSkills(t, map[string]string{
		"database-guide.md": "# Database guide\n\nThe day is the LOCAL date it started.",
	})

	out, err := callJSON(t, s.handleRead, map[string]any{"name": "database-guide"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out, "LOCAL date it started") {
		t.Fatalf("got %q", out)
	}
}

// The model may include the extension; that should not be a failure.
func TestReadSkillToleratesExtension(t *testing.T) {
	s := newSkills(t, map[string]string{"database-guide.md": "# Database guide\n"})
	if _, err := callJSON(t, s.handleRead, map[string]any{"name": "database-guide.md"}); err != nil {
		t.Fatalf("read with extension: %v", err)
	}
}

// Skill names are flat identifiers, never paths.
func TestReadSkillRejectsPaths(t *testing.T) {
	s := newSkills(t, map[string]string{"database-guide.md": "# Database guide\n"})
	for _, name := range []string{
		"../../../etc/passwd",
		"../secrets",
		"sub/skill",
		`..\windows`,
	} {
		if _, err := callJSON(t, s.handleRead, map[string]any{"name": name}); err == nil {
			t.Errorf("expected %q to be refused", name)
		}
	}
}

// A hallucinated skill name should be recoverable: name what does exist.
func TestMissingSkillListsAvailable(t *testing.T) {
	s := newSkills(t, map[string]string{
		"database-guide.md": "# Database guide\n",
		"reference.md":      "# Reference guide\n",
	})

	_, err := callJSON(t, s.handleRead, map[string]any{"name": "nonexistent"})
	if err == nil {
		t.Fatal("expected an error for a missing skill")
	}
	if !strings.Contains(err.Error(), "database-guide") || !strings.Contains(err.Error(), "reference") {
		t.Fatalf("error should list available skills, got: %v", err)
	}
}

func TestSkillList(t *testing.T) {
	s := newSkills(t, map[string]string{
		"zeta.md":           "# Zeta\n",
		"alpha.md":          "# Alpha\n",
		"notes.txt":         "not a skill",
		"database-guide.md": "# Database guide\n",
	})

	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"alpha", "database-guide", "zeta"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("got %v, want %v (sorted)", names, want)
		}
	}
}

// The catalog goes in the system prompt every turn, so it must stay cheap:
// one line per skill, not the documents themselves.
func TestCatalogIsOneLinePerSkill(t *testing.T) {
	s := newSkills(t, map[string]string{
		"database-guide.md": "# Database guide\n\n" + strings.Repeat("body line\n", 500),
		"reference.md":      "# Reference procedures\n",
	})

	cat, err := s.Catalog()
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if lines := strings.Count(cat, "\n") + 1; lines != 2 {
		t.Fatalf("expected 2 lines, got %d:\n%s", lines, cat)
	}
	if !strings.Contains(cat, "Database guide") || !strings.Contains(cat, "Reference procedures") {
		t.Fatalf("catalog missing headings:\n%s", cat)
	}
	if strings.Contains(cat, "body line") {
		t.Fatalf("catalog leaked document body:\n%s", cat)
	}
}

func TestCatalogEmptyWhenNoSkills(t *testing.T) {
	s := newSkills(t, nil)
	cat, err := s.Catalog()
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if cat != "" {
		t.Fatalf("expected empty catalog, got %q", cat)
	}
}

// The configured directory is the only source of skills.
func TestSkillsDirIsTheOnlySource(t *testing.T) {
	s := newSkills(t, map[string]string{"only.md": "# Only\n"})
	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 1 || names[0] != "only" {
		t.Fatalf("expected exactly the installed skill, got %v", names)
	}
}

func TestNewSkillsRejectsMissingDir(t *testing.T) {
	if _, err := NewSkills(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected an error for a missing skills directory")
	}
}
