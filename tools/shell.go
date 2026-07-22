package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/arcbjorn/odin/agent"
	"github.com/arcbjorn/odin/model"
)

// Shell runs shell commands for an ops profile (the maintenance agent).
//
// It is a deliberately thin primitive: `sh -c <command>`, a timeout, and an
// output cap. It contains no allowlist and no command parsing on purpose. The
// security boundary is the operating system — this toolset is meant to run as
// an unprivileged, read-only service user whose kubeconfig is a read-only
// ServiceAccount. Encoding "safe commands" in application logic is brittle and
// gives a false sense of safety; a read-only OS user cannot mutate the cluster
// no matter what string the model produces, and RBAC refuses the write itself.
//
// It follows that this toolset must only ever be enabled for a profile whose
// service user is suitably confined. Enabling it for a user with write access
// would hand the model that access.
type Shell struct {
	timeout    time.Duration
	maxTimeout time.Duration
	maxOutput  int
}

// ShellConfig configures the shell toolset.
type ShellConfig struct {
	// Timeout is the default per-command wall clock. A command may request
	// less or more up to MaxTimeout.
	Timeout time.Duration
	// MaxTimeout caps what a single command may request.
	MaxTimeout time.Duration
	// MaxOutput caps combined stdout+stderr in bytes; longer output is
	// truncated so a chatty command cannot blow the context window.
	MaxOutput int
}

// NewShell builds the shell toolset with sane defaults.
func NewShell(cfg ShellConfig) *Shell {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.MaxTimeout <= 0 {
		cfg.MaxTimeout = 180 * time.Second
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = 96 << 10
	}
	return &Shell{timeout: cfg.Timeout, maxTimeout: cfg.MaxTimeout, maxOutput: cfg.MaxOutput}
}

// Tool returns the single shell tool.
func (s *Shell) Tool() agent.Tool {
	return agent.Tool{
		Def: model.Tool{
			Name: "shell",
			Description: "Run a read-only shell command and return combined stdout+stderr. " +
				"Use for cluster and host inspection: kubectl get/describe/logs, journalctl, " +
				"df, free, systemctl status, git log/diff. Runs as an unprivileged read-only " +
				"user, so any mutation (kubectl delete/apply, rm, systemctl restart) is refused " +
				"by the OS or RBAC — investigate and report, do not attempt to change state.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The command line, run via `sh -c`.",
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Optional wall-clock limit, default 60, max 180.",
					},
				},
				"required": []string{"command"},
			},
		},
		Handle: s.handle,
	}
}

type shellInput struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout_seconds,omitempty"`
}

func (s *Shell) handle(ctx context.Context, raw json.RawMessage) (string, error) {
	var in shellInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	cmdline := strings.TrimSpace(in.Command)
	if cmdline == "" {
		return "", fmt.Errorf("command is required")
	}

	timeout := s.timeout
	if in.Timeout > 0 {
		timeout = time.Duration(in.Timeout) * time.Second
		if timeout > s.maxTimeout {
			timeout = s.maxTimeout
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", cmdline)
	// Without this, a command whose child keeps the output pipe open (a bare
	// `sleep` forked by the shell, a backgrounded process) leaves
	// CombinedOutput blocked long after the context deadline killed the shell.
	// WaitDelay forces the pipes closed shortly after cancellation.
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()

	body := truncateOutput(string(out), s.maxOutput)

	// A timeout is worth naming: the model should know the command was killed
	// rather than that it produced empty output.
	if runCtx.Err() == context.DeadlineExceeded {
		return withStatus(body, fmt.Sprintf("timed out after %s", timeout)), nil
	}
	// A non-zero exit is information, not a tool failure — grep with no match,
	// a pod that does not exist, an RBAC denial. Hand it back for the model to
	// read rather than tripping the repeated-failure guardrail.
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return withStatus(body, fmt.Sprintf("exit status %d", exit.ExitCode())), nil
		}
		return "", fmt.Errorf("run command: %w", err)
	}
	if body == "" {
		return "(no output)", nil
	}
	return body, nil
}

func withStatus(body, status string) string {
	if body == "" {
		return "[" + status + "]"
	}
	return body + "\n[" + status + "]"
}

// truncateOutput keeps the tail: for logs and long listings the end (latest
// events, the error) is usually what matters more than the head.
func truncateOutput(s string, max int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= max {
		return s
	}
	tail := s[len(s)-max:]
	if nl := strings.IndexByte(tail, '\n'); nl >= 0 && nl < len(tail)-1 {
		tail = tail[nl+1:]
	}
	return "[output truncated to last " + fmt.Sprint(max) + " bytes]\n" + tail
}
