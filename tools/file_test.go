package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newFiles(t *testing.T, readOnly bool) (*Files, string) {
	t.Helper()
	root := t.TempDir()
	f, err := NewFiles(FilesConfig{Root: root, ReadOnly: readOnly})
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	return f, root
}

func callJSON(t *testing.T, h func(context.Context, json.RawMessage) (string, error), payload map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return h(context.Background(), raw)
}

func TestFileRoundTrip(t *testing.T) {
	f, _ := newFiles(t, false)

	if _, err := callJSON(t, f.handleWrite, map[string]any{
		"path": "notes/ideas.md", "content": "vector recall for the notes pile",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := callJSON(t, f.handleRead, map[string]any{"path": "notes/ideas.md"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != "vector recall for the notes pile" {
		t.Fatalf("got %q", out)
	}
}

func TestFileAppend(t *testing.T) {
	f, _ := newFiles(t, false)

	for _, line := range []string{"one\n", "two\n"} {
		if _, err := callJSON(t, f.handleWrite, map[string]any{
			"path": "log.md", "content": line, "append": true,
		}); err != nil {
			t.Fatalf("append %q: %v", line, err)
		}
	}

	out, err := callJSON(t, f.handleRead, map[string]any{"path": "log.md"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != "one\ntwo\n" {
		t.Fatalf("got %q", out)
	}
}

// Paths come from the model and are untrusted. Every one of these must be
// refused before any syscall touches the target.
func TestFileRejectsTraversal(t *testing.T) {
	f, root := newFiles(t, false)

	// A file just outside root, which each attack below aims at.
	outside := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(outside, []byte("do not read"), 0o600); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	attacks := []string{
		"../secret.txt",
		"../../etc/passwd",
		"notes/../../secret.txt",
		"/etc/passwd",
		"/tmp/secret.txt",
		"notes/./../../secret.txt",
	}
	for _, path := range attacks {
		if out, err := callJSON(t, f.handleRead, map[string]any{"path": path}); err == nil {
			t.Errorf("read %q should have been refused, got %q", path, out)
		}
		if _, err := callJSON(t, f.handleWrite, map[string]any{
			"path": path, "content": "pwned",
		}); err == nil {
			t.Errorf("write %q should have been refused", path)
		}
	}

	if data, err := os.ReadFile(outside); err != nil || string(data) != "do not read" {
		t.Fatal("a traversal attempt modified a file outside the root")
	}
}

// A symlink *inside* root can still point outside it. Only the post-resolution
// check catches this, which is why resolve() re-verifies after EvalSymlinks.
func TestFileRejectsSymlinkEscape(t *testing.T) {
	f, root := newFiles(t, false)

	outside := filepath.Join(filepath.Dir(root), "escape-target.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if out, err := callJSON(t, f.handleRead, map[string]any{"path": "link.txt"}); err == nil {
		t.Fatalf("symlink escape should have been refused, got %q", out)
	}
}

// A sibling directory sharing a name prefix must not pass containment.
func TestFileRejectsPrefixSibling(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "notes")
	sibling := filepath.Join(base, "notes-backup")
	for _, d := range []string{root, sibling} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(sibling, "old.md"), []byte("old"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	f, err := NewFiles(FilesConfig{Root: root})
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	if _, err := callJSON(t, f.handleRead, map[string]any{"path": "../notes-backup/old.md"}); err == nil {
		t.Fatal("notes-backup must not count as inside notes")
	}
}

func TestReadOnlyProfileCannotWrite(t *testing.T) {
	f, _ := newFiles(t, true)

	if _, err := callJSON(t, f.handleWrite, map[string]any{
		"path": "x.md", "content": "nope",
	}); err == nil {
		t.Fatal("read-only profile accepted a write")
	}

	// The write tool must not even be offered — the allowlist is the boundary.
	for _, tool := range f.Tools() {
		if tool.Def.Name == "write_file" {
			t.Fatal("read-only profile exposed write_file")
		}
	}
}

func TestFileListing(t *testing.T) {
	f, root := newFiles(t, false)

	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"a.md", "b.md"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	// Hidden files are Odin's own state, not user notes.
	if err := os.WriteFile(filepath.Join(root, ".auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed hidden: %v", err)
	}

	out, err := callJSON(t, f.handleList, map[string]any{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"a.md", "b.md", "notes/"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, ".auth.json") {
		t.Errorf("listing exposed a hidden file:\n%s", out)
	}
}

func TestEmptyListingIsExplicit(t *testing.T) {
	f, _ := newFiles(t, false)
	out, err := callJSON(t, f.handleList, map[string]any{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if out != "(empty)" {
		t.Fatalf("got %q", out)
	}
}

func TestReadRejectsBinary(t *testing.T) {
	f, root := newFiles(t, false)
	if err := os.WriteFile(filepath.Join(root, "img.png"), []byte{0x89, 'P', 'N', 'G', 0x00, 0x1a}, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := callJSON(t, f.handleRead, map[string]any{"path": "img.png"}); err == nil {
		t.Fatal("expected binary content to be refused")
	}
}

func TestReadRejectsOversizedFile(t *testing.T) {
	f, root := newFiles(t, false)
	big := strings.Repeat("a", maxReadBytes+1)
	if err := os.WriteFile(filepath.Join(root, "big.md"), []byte(big), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := callJSON(t, f.handleRead, map[string]any{"path": "big.md"}); err == nil {
		t.Fatal("expected oversized read to be refused")
	}
}

func TestWriteRejectsOversizedContent(t *testing.T) {
	f, _ := newFiles(t, false)
	if _, err := callJSON(t, f.handleWrite, map[string]any{
		"path": "big.md", "content": strings.Repeat("a", maxWriteBytes+1),
	}); err == nil {
		t.Fatal("expected oversized write to be refused")
	}
}

func TestReadMissingFileNamesIt(t *testing.T) {
	f, _ := newFiles(t, false)
	_, err := callJSON(t, f.handleRead, map[string]any{"path": "nope.md"})
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if !strings.Contains(err.Error(), "nope.md") {
		t.Fatalf("error should name the file, got: %v", err)
	}
}

func TestFileToolSchemasAreSmall(t *testing.T) {
	f, _ := newFiles(t, false)
	for _, tool := range f.Tools() {
		props, _ := tool.Def.Schema["properties"].(map[string]any)
		if len(props) > 6 {
			t.Errorf("%s has %d properties; keep tool schemas small", tool.Def.Name, len(props))
		}
	}
}
