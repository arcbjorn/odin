package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"odin/agent"
	"odin/model"
)

const (
	// maxReadBytes caps a single read. Large files would blow the context
	// window and get re-sent on every subsequent turn.
	maxReadBytes = 100 << 10 // 100 KB

	// maxWriteBytes caps a single write, so a runaway generation can't fill
	// the disk on a small VPS.
	maxWriteBytes = 1 << 20 // 1 MB

	// maxListEntries caps a directory listing.
	maxListEntries = 200
)

// Files gives the model scoped filesystem access.
//
// Every path is resolved and confirmed to sit inside root before any syscall.
// The model supplies these paths, so they are untrusted input: `..`, symlinks
// out of the tree, and absolute paths must all be rejected rather than
// trusted. Confinement is enforced here, in code — not requested in a prompt.
type Files struct {
	root     string
	readOnly bool
}

// FilesConfig configures scoped file access.
type FilesConfig struct {
	// Root is the only directory the model may touch. Resolved once at
	// startup so a later symlink swap on root itself cannot widen scope.
	Root string
	// ReadOnly drops the write tool. The maint profile will want this.
	ReadOnly bool
}

// NewFiles builds a scoped file toolset.
func NewFiles(cfg FilesConfig) (*Files, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("file root is required")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	// EvalSymlinks so a symlinked root compares equal to the paths we later
	// resolve; otherwise every containment check would fail.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root %s: %w", root, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("stat root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("file root %s is not a directory", resolved)
	}
	return &Files{root: resolved, readOnly: cfg.ReadOnly}, nil
}

// Tools returns the file tools this profile is permitted to use.
func (f *Files) Tools() []agent.Tool {
	tools := []agent.Tool{f.readTool(), f.listTool()}
	if !f.readOnly {
		tools = append(tools, f.writeTool())
	}
	return tools
}

type readInput struct {
	Path string `json:"path"`
}

type writeInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append,omitempty"`
}

type listInput struct {
	Path string `json:"path,omitempty"`
}

func (f *Files) readTool() agent.Tool {
	return agent.Tool{
		Def: model.Tool{
			Name:        "read_file",
			Description: "Read a UTF-8 text file. Paths are relative to the notes directory.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Relative path, e.g. notes/ideas.md"},
				},
				"required": []string{"path"},
			},
		},
		Handle: f.handleRead,
	}
}

func (f *Files) writeTool() agent.Tool {
	return agent.Tool{
		Def: model.Tool{
			Name:        "write_file",
			Description: "Write a UTF-8 text file, creating parent directories as needed. Set append to add to the end instead of replacing.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string", "description": "Relative path, e.g. notes/ideas.md"},
					"content": map[string]any{"type": "string", "description": "File contents."},
					"append":  map[string]any{"type": "boolean", "description": "Append instead of replacing."},
				},
				"required": []string{"path", "content"},
			},
		},
		Handle: f.handleWrite,
	}
}

func (f *Files) listTool() agent.Tool {
	return agent.Tool{
		Def: model.Tool{
			Name:        "list_files",
			Description: "List files and directories. Omit path to list the notes root.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Relative directory, e.g. notes"},
				},
			},
		},
		Handle: f.handleList,
	}
}

func (f *Files) handleRead(_ context.Context, raw json.RawMessage) (string, error) {
	var in readInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	full, err := f.resolve(in.Path)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no such file: %s", in.Path)
		}
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory; use list_files", in.Path)
	}
	if info.Size() > maxReadBytes {
		return "", fmt.Errorf("%s is %d bytes, over the %d byte limit", in.Path, info.Size(), maxReadBytes)
	}

	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	if !isProbablyText(data) {
		return "", fmt.Errorf("%s does not look like text", in.Path)
	}
	return string(data), nil
}

func (f *Files) handleWrite(_ context.Context, raw json.RawMessage) (string, error) {
	if f.readOnly {
		return "", fmt.Errorf("this profile has read-only file access")
	}
	var in writeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if len(in.Content) > maxWriteBytes {
		return "", fmt.Errorf("content is %d bytes, over the %d byte limit", len(in.Content), maxWriteBytes)
	}
	full, err := f.resolve(in.Path)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return "", err
	}

	if in.Append {
		fh, err := os.OpenFile(full, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return "", err
		}
		defer fh.Close()
		if _, err := fh.WriteString(in.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("Appended %d bytes to %s.", len(in.Content), in.Path), nil
	}

	// Atomic replace: a crash mid-write leaves the previous file intact
	// rather than a truncated one.
	tmp, err := os.CreateTemp(filepath.Dir(full), ".odin-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(in.Content); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, full); err != nil {
		return "", err
	}
	return fmt.Sprintf("Wrote %d bytes to %s.", len(in.Content), in.Path), nil
}

func (f *Files) handleList(_ context.Context, raw json.RawMessage) (string, error) {
	var in listInput
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
	}
	target := in.Path
	if strings.TrimSpace(target) == "" {
		target = "."
	}
	full, err := f.resolve(target)
	if err != nil {
		return "", err
	}

	entries, err := os.ReadDir(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no such directory: %s", target)
		}
		return "", err
	}

	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue // hidden files are Odin's own state, not user notes
		}
		if e.IsDir() {
			lines = append(lines, e.Name()+"/")
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s\t%d bytes", e.Name(), info.Size()))
	}
	if len(lines) == 0 {
		return "(empty)", nil
	}
	sort.Strings(lines)

	truncated := false
	if len(lines) > maxListEntries {
		lines = lines[:maxListEntries]
		truncated = true
	}
	out := strings.Join(lines, "\n")
	if truncated {
		out += fmt.Sprintf("\n(truncated at %d entries)", maxListEntries)
	}
	return out, nil
}

// resolve turns a model-supplied path into an absolute path proven to sit
// inside root.
//
// Order matters. Absolute paths and `..` are rejected up front, then the path
// is resolved through symlinks and re-checked — a symlink inside root can
// still point outside it, and only the post-resolution check catches that.
func (f *Files) resolve(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative to the notes directory")
	}

	joined := filepath.Join(f.root, rel)
	clean := filepath.Clean(joined)
	if !within(f.root, clean) {
		return "", fmt.Errorf("path escapes the notes directory")
	}

	// If the path exists, resolve symlinks and re-verify. If it does not
	// exist yet (a new file), verify its nearest existing parent instead.
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		parent, perr := filepath.EvalSymlinks(filepath.Dir(clean))
		if perr != nil {
			if os.IsNotExist(perr) {
				return clean, nil // parents get created under root by MkdirAll
			}
			return "", perr
		}
		if !within(f.root, parent) {
			return "", fmt.Errorf("path escapes the notes directory")
		}
		return clean, nil
	}
	if !within(f.root, resolved) {
		return "", fmt.Errorf("path escapes the notes directory")
	}
	return resolved, nil
}

// within reports whether path is root or sits beneath it. Compares path
// segments rather than string prefixes, so /notes-backup does not pass as
// being inside /notes.
func within(root, path string) bool {
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// isProbablyText rejects binaries. A NUL byte is the cheapest reliable signal
// and avoids feeding a JPEG into the context window.
func isProbablyText(data []byte) bool {
	limit := len(data)
	if limit > 8000 {
		limit = 8000
	}
	for _, b := range data[:limit] {
		if b == 0 {
			return false
		}
	}
	return true
}
