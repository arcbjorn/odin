package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func runShell(t *testing.T, s *Shell, in shellInput) string {
	t.Helper()
	raw, _ := json.Marshal(in)
	out, err := s.handle(context.Background(), raw)
	if err != nil {
		t.Fatalf("handle(%q): %v", in.Command, err)
	}
	return out
}

func TestShellCapturesStdout(t *testing.T) {
	s := NewShell(ShellConfig{})
	if got := runShell(t, s, shellInput{Command: "echo hello"}); got != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestShellCapturesStderrAndExitStatus(t *testing.T) {
	s := NewShell(ShellConfig{})
	// A non-zero exit is a normal result, not a Go error.
	got := runShell(t, s, shellInput{Command: "echo oops >&2; exit 3"})
	if !strings.Contains(got, "oops") {
		t.Fatalf("stderr not captured: %q", got)
	}
	if !strings.Contains(got, "exit status 3") {
		t.Fatalf("exit status not reported: %q", got)
	}
}

func TestShellEmptyCommandErrors(t *testing.T) {
	s := NewShell(ShellConfig{})
	if _, err := s.handle(context.Background(), json.RawMessage(`{"command":"  "}`)); err == nil {
		t.Fatal("empty command should error")
	}
}

func TestShellTimeout(t *testing.T) {
	s := NewShell(ShellConfig{})
	got := runShell(t, s, shellInput{Command: "sleep 5", Timeout: 1})
	if !strings.Contains(got, "timed out") {
		t.Fatalf("expected timeout note, got %q", got)
	}
}

func TestShellTimeoutCappedAtMax(t *testing.T) {
	s := NewShell(ShellConfig{MaxTimeout: 2 * time.Second})
	start := time.Now()
	// Requests 60s but MaxTimeout is 2s, so it must be killed near 2s.
	runShell(t, s, shellInput{Command: "sleep 30", Timeout: 60})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout not capped: ran %s", elapsed)
	}
}

func TestShellTruncatesToTail(t *testing.T) {
	s := NewShell(ShellConfig{MaxOutput: 64})
	got := runShell(t, s, shellInput{Command: "seq 1 1000"})
	if !strings.Contains(got, "truncated") {
		t.Fatalf("expected truncation marker: %q", got)
	}
	// The tail (latest lines) must survive, the head must not.
	if !strings.Contains(got, "1000") {
		t.Fatalf("tail lost: %q", got)
	}
	if strings.Contains(got, "\n1\n") {
		t.Fatalf("head should be dropped: %q", got)
	}
}

func TestShellToolSchemaSmall(t *testing.T) {
	def := NewShell(ShellConfig{}).Tool().Def
	if def.Name != "shell" {
		t.Fatalf("tool name = %q", def.Name)
	}
	props, _ := def.Schema["properties"].(map[string]any)
	if len(props) > 6 {
		t.Fatalf("schema too wide: %d props", len(props))
	}
}
